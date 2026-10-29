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
// names the head row's blob or manifest: counted once, as the head row).
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

// VersionRetained is the condition a version row v is a RETAINED
// non-current version under its key's head row h (NULL when the key has no
// current object — behind a delete marker): it names bytes (a whole blob,
// or since WP-VM-1 a manifest — backend 'chunked') and they are not the
// head row's. "The head row's" is matched by address AND kind: a chunked
// head row names its manifest at COALESCE(storage_key, object_key), as a
// whole one names its blob — a chunked version is billed like any other
// (logical size), and the head row's own version is counted once, as the
// head row.
const VersionRetained = `(v.storage_key IS NOT NULL AND NOT v.is_delete_marker
	AND (h.tenant_id IS NULL OR COALESCE(h.storage_key, h.object_key) <> v.storage_key
	     OR h.is_chunked <> (COALESCE(v.backend_name, '') = 'chunked')))`

// RetainedVersionsFrom is the FROM … WHERE of the retained non-current
// versions: v is the version row, h the key's head row. A caller appends its
// own "AND v.tenant_id = …".
const RetainedVersionsFrom = `object_versions v
	LEFT JOIN object_head_cache h
	       ON h.tenant_id = v.tenant_id AND h.bucket = v.bucket AND h.object_key = v.object_key
	WHERE ` + VersionRetained

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

// PoolBilledBytes is what a platform's pool is billed (WP-PLAT-1): the
// owner's billed bytes and every child's, each child's counted under its
// own id by BilledBytes. A tenant with no children is its own pool. A
// sandbox is never in it.
func (m *QuotaManager) PoolBilledBytes(ctx context.Context, ownerID string) (int64, error) {
	var total int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(`+billedBytesOfTQ+`), 0)
		  FROM tenant_quotas tq
		 WHERE tq.tenant_id = $1
		    OR tq.tenant_id IN (SELECT c.id FROM tenants c WHERE c.parent_tenant_id = $1)`, ownerID).Scan(&total); err != nil {
		return 0, fmt.Errorf("billed bytes of the pool of %s: %w", ownerID, err)
	}
	return total, nil
}
