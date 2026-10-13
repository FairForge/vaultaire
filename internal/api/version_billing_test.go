package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R2-1 Part C, PR C2: a retained non-current version holds bytes, so it
// is billed — in tenant_quotas, on its floor in tenant_floor_quotas, by the
// reconcile and by the PUT quota check — and released when it goes.
// Red on 70d38b9 (C1): the flip released the displaced size although the
// version kept its blob; the reconcile summed head rows only.

// versionedQuota turns test-bucket into a versioned bucket of the fixture's
// tenant (status: Enabled | Suspended).
func (f *quotaAccountingFixture) versioning(t *testing.T, status string) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, versioning_status) VALUES ($1, 'test-bucket', 'private', $2)
		ON CONFLICT (tenant_id, name) DO UPDATE SET versioning_status = EXCLUDED.versioning_status`, f.tenantID, status)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM retired_blobs WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.tenantID)
	})
}

// putVersion PUTs through the server path and returns the status and the
// version id it answered.
func (f *quotaAccountingFixture) putVersion(t *testing.T, key string, n int) (int, string) {
	t.Helper()
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(testBytes(n)))
	req.ContentLength = int64(n)
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handlePutObject(w, req, f.s3Req("test-bucket", key))
	return w.Code, w.Header().Get("x-amz-version-id")
}

func (f *quotaAccountingFixture) delVersion(t *testing.T, key, vid string) {
	t.Helper()
	target := "/test-bucket/" + key
	if vid != "" {
		target += "?versionId=" + vid
	}
	req := httptest.NewRequest("DELETE", target, nil)
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handleDeleteObject(w, req, f.s3Req("test-bucket", key))
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
}

func (f *quotaAccountingFixture) floorUsed(t *testing.T, floor string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_floor_quotas WHERE tenant_id = $1 AND floor = $2`,
		f.tenantID, floor).Scan(&n))
	return n
}

// agrees asserts that the incremental ledgers equal want and that the
// reconcile — the source of truth recomputed from the tables — agrees with
// them, total and floor.
func (f *quotaAccountingFixture) agrees(t *testing.T, want int64, step string) {
	t.Helper()
	require.Equal(t, want, f.used(t), "%s: storage_used_bytes", step)
	require.Equal(t, want, f.floorUsed(t, usage.FloorStandard), "%s: the standard floor", step)
	require.NoError(t, f.qm.ReconcileTenantStorageUsage(context.Background(), f.tenantID))
	require.Equal(t, want, f.used(t), "%s: the reconcile disagrees with the incremental ledger", step)
	require.Equal(t, want, f.floorUsed(t, usage.FloorStandard), "%s: the floor reconcile disagrees", step)
}

func TestVersionBytes_BilledAndReleased(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.versioning(t, "Enabled")
	// A house tenant: the floor ledger moves with the total.
	require.NoError(t, f.qm.SetHouse(context.Background(), f.tenantID, usage.House{StdBytes: 20000}))

	code, v1 := f.putVersion(t, "k", 3000)
	require.Equal(t, http.StatusOK, code)
	f.agrees(t, 3000, "first PUT")

	code, _ = f.putVersion(t, "k", 5000)
	require.Equal(t, http.StatusOK, code)
	f.agrees(t, 8000, "versioned overwrite: the retained version is billed")

	f.delVersion(t, "k", v1)
	f.agrees(t, 5000, "the old version deleted: released")

	code, _ = f.putVersion(t, "k", 9000)
	require.Equal(t, http.StatusOK, code)
	f.agrees(t, 14000, "5000 retained + 9000 current")

	// 14000 held + 7000 new > 20000: refused, though the current object
	// alone (9000) + 7000 would fit.
	code, _ = f.putVersion(t, "k", 7000)
	assert.Equal(t, http.StatusForbidden, code, "a versioned overwrite past the quota counting version bytes")
	f.agrees(t, 14000, "the refused PUT holds nothing")
}

// A delete marker keeps the version's bytes (and its bill); a Suspended
// bucket's null-version overwrite replaces the null version's bytes and
// releases them.
func TestVersionBytes_MarkerAndSuspendedNull(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.versioning(t, "Enabled")
	require.NoError(t, f.qm.SetHouse(context.Background(), f.tenantID, usage.House{StdBytes: 1 << 30}))

	code, _ := f.putVersion(t, "k", 3000)
	require.Equal(t, http.StatusOK, code)
	f.delVersion(t, "k", "") // a delete marker: the version keeps its bytes
	f.agrees(t, 3000, "behind a delete marker")

	f.versioning(t, "Suspended")
	code, _ = f.putVersion(t, "k", 1000)
	require.Equal(t, http.StatusOK, code)
	f.agrees(t, 4000, "the null version")
	code, _ = f.putVersion(t, "k", 2000)
	require.Equal(t, http.StatusOK, code)
	f.agrees(t, 5000, "the null version rewritten: its old bytes released")

	f.versioning(t, "Enabled")
	code, _ = f.putVersion(t, "k", 500)
	require.Equal(t, http.StatusOK, code)
	f.agrees(t, 5500, "the null version is non-current now and still billed")

	f.versioning(t, "Suspended")
	code, _ = f.putVersion(t, "k", 700)
	require.Equal(t, http.StatusOK, code)
	f.agrees(t, 4200, "a non-current null version replaced: released (3000 + 500 + 700)")
	assert.Equal(t, retiredOverwritten, f.retiredReasonCount(t), "the replaced null version's blob is retired")
}

// retiredReasonCount: the reason of the one retired blob that is not a
// version delete (the replaced null version's).
func (f *quotaAccountingFixture) retiredReasonCount(t *testing.T) string {
	t.Helper()
	var reason string
	require.NoError(t, f.db.QueryRow(`SELECT reason FROM retired_blobs WHERE tenant_id = $1 ORDER BY retire_after DESC LIMIT 1`,
		f.tenantID).Scan(&reason))
	return reason
}
