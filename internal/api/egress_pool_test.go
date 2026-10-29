package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// D-STRIPE-31 / RB9-2 / RP82-2 (Stage 1a Prompt 52, Part 0): a platform's
// children download from their POOL's allowance. The counter the throttle
// decides on is the pool root's; each tenant's bandwidth rows stay its own.

// addChild makes a child tenant of the fixture's tenant (a platform) with
// the fixture's bucket, and registers it as a tenant requests may act as.
func (f *egressFixture) addChild(name string) string {
	f.t.Helper()
	id := "egress-child-" + uuid.NewString()[:8]
	_, err := f.db.Exec(`INSERT INTO tenants (id, name, email, parent_tenant_id) VALUES ($1, $2, $3, $4)`,
		id, name, id+"@platform.invalid", f.tenantID)
	require.NoError(f.t, err)
	_, err = f.db.Exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, tier) VALUES ($1, 0, 'starter')`, id)
	require.NoError(f.t, err)
	_, err = f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, $2, 'private')`, id, egressTestBucket)
	require.NoError(f.t, err)
	tn := &tenant.Tenant{ID: id, Namespace: "tenant/" + id + "/"}
	require.NoError(f.t, os.MkdirAll(filepath.Join(f.tempDir, tn.NamespaceContainer(egressTestBucket)), 0o755))
	f.as.Store(id, tn)
	f.t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM bandwidth_usage_daily WHERE tenant_id = $1`,
			`DELETE FROM bandwidth_alerts WHERE tenant_id = $1`,
			`DELETE FROM events WHERE tenant_id = $1`,
			`DELETE FROM audit_logs WHERE tenant_id = $1`,
			`DELETE FROM object_head_cache WHERE tenant_id = $1`,
			`DELETE FROM object_locations WHERE tenant_id = $1`,
			`DELETE FROM buckets WHERE tenant_id = $1`,
			`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`,
			`DELETE FROM tenant_quotas WHERE tenant_id = $1`,
		} {
			_, _ = f.db.Exec(q, id)
		}
		cleanupTenantChunkRows(f.db, id, id)
		_, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, id)
	})
	return id
}

// putAs uploads size random bytes as tenant id.
func (f *egressFixture) putAs(id, key string, size int64) {
	f.t.Helper()
	body := make([]byte, size)
	_, _ = rand.Read(body)
	req, err := http.NewRequest(http.MethodPut, f.ts.URL+f.s3Path(key), bytes.NewReader(body))
	require.NoError(f.t, err)
	req.ContentLength = size
	req.Header.Set(egressTestAs, id)
	mark := f.mark(req)
	resp, err := f.ts.Client().Do(req)
	require.NoError(f.t, err)
	msg, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(f.t, http.StatusOK, resp.StatusCode, string(msg))
	f.settle(mark)
}

// getAs downloads key as tenant id.
func (f *egressFixture) getAs(id, key string) egressResult {
	return f.do(http.MethodGet, f.s3Path(key), map[string]string{egressTestAs: id})
}

// recordedOwn is one tenant's own bandwidth rows this month (never the pool's).
func (f *egressFixture) recordedOwn(id string) int64 {
	f.t.Helper()
	var n int64
	require.NoError(f.t, f.db.QueryRow(`SELECT COALESCE(SUM(egress_bytes), 0) FROM bandwidth_usage_daily
		WHERE tenant_id = $1 AND date >= $2::date`, id, usage.EgressMonthStart(time.Now()).Format("2006-01-02")).Scan(&n))
	return n
}

func TestPool_AChildsDownloadsSpendThePoolsAllowance(t *testing.T) {
	// Arrange: a platform with an allowance of 10 MiB (the fixture's
	// override: half its 20 MiB quota) and two children with 6 MiB each.
	f := setupEgressFixture(t, 20*mib, testThrottle(), nil)
	_, err := f.db.Exec(`UPDATE tenants SET platform = TRUE WHERE id = $1`, f.tenantID)
	require.NoError(t, err)
	require.Equal(t, 10*mib, f.allowance())
	a, b := f.addChild("A"), f.addChild("B")
	f.putAs(a, "a.bin", 6*mib)
	f.putAs(b, "b.bin", 6*mib)
	f.enforced.Store(true)

	// Act: A downloads 6 MiB, B 6 MiB (the pool crosses 10 MiB), then B again.
	first := f.getAs(a, "a.bin")
	f.getAs(b, "b.bin")
	second := f.getAs(b, "b.bin")

	// Assert: A's download was inside the pool's allowance; B's second is
	// paced — (6 − 1 burst) MiB at 4 MiB/s = 1.25 s.
	require.Equal(t, http.StatusOK, first.status)
	assert.Less(t, first.elapsed, egressFastBound)
	require.Equal(t, http.StatusOK, second.status)
	assert.GreaterOrEqual(t, second.elapsed, 1000*time.Millisecond, "a child past its pool's allowance is paced")

	// With the flag off nothing is slowed and the same decision is counted.
	f.enforced.Store(false)
	would := wouldCount(egressSurfaceS3)
	off := f.getAs(a, "a.bin")
	require.Equal(t, http.StatusOK, off.status)
	assert.Less(t, off.elapsed, egressFastBound)
	assert.Equal(t, would+1, wouldCount(egressSurfaceS3), "the pool's decision is counted with the flag off")

	// The rows stay under A and B; the pool's month is their sum.
	require.Eventually(t, func() bool {
		f.srv.bandwidthTracker.Flush()
		return f.recordedOwn(a) == 12*mib && f.recordedOwn(b) == 12*mib
	}, 5*time.Second, 20*time.Millisecond, "A %d, B %d", f.recordedOwn(a), f.recordedOwn(b))
	assert.Zero(t, f.recordedOwn(f.tenantID), "nothing is recorded under the platform")
	assert.Equal(t, 24*mib, f.recorded(), "the platform's month is the pool's")
	assert.Equal(t, 24*mib, f.used(), "the live counter is the pool's")

	// The alert ladder fires for the platform, never for a child.
	f.enforced.Store(true)
	sender := &fakeBandwidthEmailSender{}
	al := f.alerter(sender)
	cands, err := al.alertCandidates(context.Background())
	require.NoError(t, err)
	assert.Contains(t, cands, f.tenantID, "the pool root is the candidate")
	assert.NotContains(t, cands, a)
	assert.NotContains(t, cands, b)
	al.checkTenantAlerts(context.Background(), f.tenantID)
	rows := f.alertRows()
	assert.True(t, rows[80] && rows[95] && rows[100], "every step fired for the platform: %v", rows)
}
