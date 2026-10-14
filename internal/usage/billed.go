// internal/usage/billed.go
//
// What a tenant is billed for, in logical bytes: every head row
// (object_head_cache — the current object of every key) plus every RETAINED
// non-current version (WP-R2-1 Part C, PR C2), plus what every ACTIVE
// multipart upload holds (multipart_uploads.reserved_bytes, WP-R10-8 Part C:
// its staged parts, reserved as they arrive — a free tenant could pin 50 GiB
// × N of hub disk at no quota cost, R10-24 / R3-14). Since C1 (087) a version
// keeps its own blob (object_versions.storage_key); a row that names a blob
// holds bytes and is billed like an object. Not billed: a delete marker, a
// version written before C1 (storage_key NULL — its bytes were overwritten
// in place), and the version that IS the current object (the latest version
// names the head row's blob: counted once, as the head row).
//
// The incremental ledgers (tenant_quotas, tenant_floor_quotas) follow the
// same rule write by write (internal/api/quota_accounting.go); the
// reconciles, the house seeding and the dashboard read these fragments, so
// there is one definition.
package usage

import (
	"context"
	"fmt"
)

// RetainedVersionsFrom is the FROM … WHERE of the retained non-current
// versions: v is the version row, h the key's head row (NULL when the key
// has no current object — behind a delete marker). A caller appends its own
// "AND v.tenant_id = …".
const RetainedVersionsFrom = `object_versions v
	LEFT JOIN object_head_cache h
	       ON h.tenant_id = v.tenant_id AND h.bucket = v.bucket AND h.object_key = v.object_key
	WHERE v.storage_key IS NOT NULL AND NOT v.is_delete_marker
	  AND (h.tenant_id IS NULL OR h.is_chunked OR COALESCE(h.storage_key, h.object_key) <> v.storage_key)`

// RetainedVersionFloor is the floor a retained version is billed on: its
// own (088, written with the version), else the head row's of its key, else
// the attic when its bytes sit on the tape backend, else downstairs. Never a
// vault-floor version billed as standard while either row knows better.
const RetainedVersionFloor = `COALESCE(v.floor, h.floor, CASE WHEN v.backend_name = 'geyser' THEN '` + FloorVault + `' ELSE '` + FloorStandard + `' END)`

// UploadHoldingFloor is the floor an active multipart upload's holding is
// on: the one its parts were reserved on (091), downstairs for an upload
// created before 091 (it holds nothing).
const UploadHoldingFloor = `COALESCE(u.reserved_floor, '` + FloorStandard + `')`

// The billed bytes as scalar subqueries, one constant per call site (SQL is
// never assembled from runtime strings): of the tenant_quotas row tq, of the
// tenant_floor_quotas row f, and of tenant $1 on floor $2.
const (
	billedBytesOfTQ = `(COALESCE((SELECT SUM(o.size_bytes) FROM object_head_cache o WHERE o.tenant_id = tq.tenant_id), 0)
		+ COALESCE((SELECT SUM(v.size_bytes) FROM ` + RetainedVersionsFrom + ` AND v.tenant_id = tq.tenant_id), 0)
		+ COALESCE((SELECT SUM(u.reserved_bytes) FROM multipart_uploads u WHERE u.tenant_id = tq.tenant_id AND u.status = 'active'), 0))`
	billedBytesOfF = `(COALESCE((SELECT SUM(o.size_bytes) FROM object_head_cache o WHERE o.tenant_id = f.tenant_id AND o.floor = f.floor), 0)
		+ COALESCE((SELECT SUM(v.size_bytes) FROM ` + RetainedVersionsFrom + ` AND v.tenant_id = f.tenant_id AND ` + RetainedVersionFloor + ` = f.floor), 0)
		+ COALESCE((SELECT SUM(u.reserved_bytes) FROM multipart_uploads u WHERE u.tenant_id = f.tenant_id AND u.status = 'active' AND ` + UploadHoldingFloor + ` = f.floor), 0))`
	billedBytesOfParams = `(COALESCE((SELECT SUM(o.size_bytes) FROM object_head_cache o WHERE o.tenant_id = $1::TEXT), 0)
		+ COALESCE((SELECT SUM(v.size_bytes) FROM ` + RetainedVersionsFrom + ` AND v.tenant_id = $1::TEXT), 0)
		+ COALESCE((SELECT SUM(u.reserved_bytes) FROM multipart_uploads u WHERE u.tenant_id = $1::TEXT AND u.status = 'active'), 0))`
	billedBytesOfParamsFloor = `(COALESCE((SELECT SUM(o.size_bytes) FROM object_head_cache o WHERE o.tenant_id = $1::TEXT AND o.floor = $2::TEXT), 0)
		+ COALESCE((SELECT SUM(v.size_bytes) FROM ` + RetainedVersionsFrom + ` AND v.tenant_id = $1::TEXT AND ` + RetainedVersionFloor + ` = $2::TEXT), 0)
		+ COALESCE((SELECT SUM(u.reserved_bytes) FROM multipart_uploads u WHERE u.tenant_id = $1::TEXT AND u.status = 'active' AND ` + UploadHoldingFloor + ` = $2::TEXT), 0))`
)

// BilledBytes is what ReconcileTenantStorageUsage would write for tenantID —
// the total and each floor row's — read without writing (tests compare the
// incremental ledgers with it at every step; WP-R10-8).
func (m *QuotaManager) BilledBytes(ctx context.Context, tenantID string) (total int64, floors map[string]int64, err error) {
	if err := m.db.QueryRowContext(ctx, `SELECT `+billedBytesOfParams, tenantID).Scan(&total); err != nil {
		return 0, nil, fmt.Errorf("billed bytes of tenant %s: %w", tenantID, err)
	}
	floors = map[string]int64{}
	for _, f := range []string{FloorStandard, FloorVault} {
		var n int64
		if err := m.db.QueryRowContext(ctx, `SELECT `+billedBytesOfParamsFloor, tenantID, f).Scan(&n); err != nil {
			return 0, nil, fmt.Errorf("billed bytes of tenant %s on %s: %w", tenantID, f, err)
		}
		floors[f] = n
	}
	return total, floors, nil
}
