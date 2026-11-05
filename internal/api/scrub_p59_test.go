package api

// Stage 1a Prompt 59 Part 0 — the P3s of RP96 on the scrub and the copies.

import (
	"context"
	"crypto/md5" // #nosec G501 -- an S3 ETag
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
)

// RP96-6: a backend due and skipped every run (its breaker open, its vendor
// allowance spent) never had a cycle_started timestamp — scrubBackend
// returned before setting it — so ScrubCycleStale could not see the very
// starvation it documents. The cycle IS in progress: it starts before the
// skip. A backend with nothing to read starts none.
func TestScrub_ABackendSkippedEveryRunHasItsCycleStarted(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM backend_plans WHERE backend = 'edgefake'`) })
	f.exec(`INSERT INTO backend_plans (backend, capacity_bytes, egress_free_multiple, term_start, term_end) VALUES ('edgefake', 1000000000000, 1, NOW(), NOW() + interval '1 year')
		ON CONFLICT (backend) DO UPDATE SET capacity_bytes = 1000000000000, egress_free_multiple = 1`)

	// Nothing stored: no cycle, skipped or not.
	st := f.run().Backends["edgefake"]
	require.NotNil(t, st)
	assert.True(t, st.CycleStartedAt.IsZero(), "nothing to read starts no cycle")

	// A blob, and no capacity run: the allowance is unknown, so it is
	// skipped — and its cycle has begun.
	f.putObject("a.bin", testBytes(5000), nil)
	st = f.run().Backends["edgefake"]
	assert.Equal(t, "vendor_allowance", st.Skipped)
	require.False(t, st.CycleStartedAt.IsZero(), "the cycle is in progress while it is skipped")
	started := st.CycleStartedAt
	st = f.run().Backends["edgefake"]
	assert.Equal(t, started.UTC(), st.CycleStartedAt.UTC(), "the same cycle, run after run")
}

// RP96-7: the scrub and the blob copies spend ONE vendor allowance. Without
// a vendor view (prod: no reseller key) "used" is only what the two jobs
// read, so the bound keeps one multiple for the customers' own reads:
// (multiple − 1) × stored − (the scrub's + the copies' month). With the
// vendor's month-to-date (which counts the customers' reads too) the full
// multiple, less the larger of the vendor's figure and ours. Each job's
// ledger counts the other's month.
func TestScrub_AndCopiesShareOneVendorLedger(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour
	f.job.copiesJob = "blob_copies_test_" + f.tenantID
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM backend_plans WHERE backend = 'edgefake'`)
		_, _ = f.db.Exec(`DELETE FROM job_runs WHERE job = $1`, f.job.copiesJob)
	})
	f.exec(`INSERT INTO backend_plans (backend, capacity_bytes, egress_free_multiple, term_start, term_end) VALUES ('edgefake', 1000000000000, 3, NOW(), NOW() + interval '1 year')
		ON CONFLICT (backend) DO UPDATE SET capacity_bytes = 1000000000000, egress_free_multiple = 3`)
	month := time.Now().UTC().Format("2006-01")
	capacity := func(stored int64, vendor *int64) {
		r := BackendCapacityResult{Backends: map[string]*BackendBytes{"edgefake": {Total: stored}}}
		if vendor != nil {
			r.Vendor = map[string]VendorView{"edgefake": {EgressMonthBytes: *vendor}}
		}
		raw, err := json.Marshal(r)
		require.NoError(t, err)
		f.exec(`INSERT INTO job_runs (job, last_outcome, result) VALUES ($1, 'ok', $2)
			ON CONFLICT (job) DO UPDATE SET result = EXCLUDED.result`, f.job.capacityJob, raw)
	}
	copiesSpent := func(n int64) {
		raw, err := json.Marshal(BlobCopiesResult{Egress: &blobCopyEgress{Month: month, Spent: map[string]int64{"edgefake": n}}})
		require.NoError(t, err)
		f.exec(`INSERT INTO job_runs (job, last_outcome, result) VALUES ($1, 'ok', $2)
			ON CONFLICT (job) DO UPDATE SET result = EXCLUDED.result`, f.job.copiesJob, raw)
	}
	f.putObject("a.bin", testBytes(5000), nil)
	f.putObject("b.bin", testBytes(5000), nil)

	// No vendor view: (3 − 1) × 10,000 − 12,000 (the copies) = 8,000 — one
	// blob, not two (the full multiple would have read both).
	capacity(10000, nil)
	copiesSpent(12000)
	st := f.run().Backends["edgefake"]
	assert.Equal(t, map[string]int{"whole": 1}, st.Checked)
	assert.Equal(t, "vendor_allowance", st.Skipped)

	// The copies' ledger counts the scrub's 5,000: (3 − 1) × 10,000 −
	// (12,000 + 5,000) = 3,000.
	cj := newBlobCopiesJob(f.srv)
	require.NotNil(t, cj)
	cj.JobName, cj.capacityJob, cj.scrubJob, cj.onlyTenant = f.job.copiesJob, f.job.capacityJob, f.job.JobName, f.tenantID
	var cres BlobCopiesResult
	l := cj.egressLedger(context.Background(), &cres)
	assert.Equal(t, int64(3000), l.limit["edgefake"], "the scrub's month is the copies' too")

	// The vendor reports 15,000 this month: the full multiple, 30,000 −
	// max(15,000, 12,000 + 5,000) = 13,000 — the second blob is read.
	v := int64(15000)
	capacity(10000, &v)
	st = f.run().Backends["edgefake"]
	assert.Equal(t, map[string]int{"whole": 1}, st.Checked)
	assert.Empty(t, st.Skipped)
	l = cj.egressLedger(context.Background(), &cres)
	assert.Equal(t, int64(30000-22000), l.limit["edgefake"], "30,000 − max(15,000, 12,000 + 10,000)")
}

// RP96-8: an iDrive region is the idrive ACCOUNT — one account, one plan
// row. A row named for a region was never spent (the budgets look the
// account up), so the PUT refuses it.
func TestBackendPlan_RefusesARegionName(t *testing.T) {
	f := setupPlanFixture(t)
	f.server.engine.AddDriver("idrive-us-west-2", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	w, _ := f.call(t, f.adminID, "PUT", "/api/v1/admin/backends/idrive-us-west-2/plan", validPlanBody("x"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "plan_is_per_account")
	assert.Contains(t, w.Body.String(), "idrive")
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM backend_plans WHERE backend = 'idrive-us-west-2'`).Scan(&n))
	assert.Zero(t, n)
}

// RP96-9: the copies backfill passed a sealed object's PLAINTEXT etag (and
// size), and copyOne refused every copy whose bytes' MD5 was not it — so a
// Private-mode object never had the mirror copy the scrub repairs from. A
// sealed row is enqueued with its backend_etag (the stored bytes' MD5) and
// the stored length; one with no backend_etag is skipped.
func TestBlobCopies_ASealedObjectIsCopied(t *testing.T) {
	f := setupEdgeFixture(t)
	m := f.addMirrorLeg()
	key := []byte("0123456789abcdef0123456789abcdef")
	h := f.putObject("sealed.txt", []byte("a sealed body the mirror must hold whole"), ssecHeaders(key))
	var backendETag string
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(backend_etag, '') FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'sealed.txt'`,
		f.tenantID, f.bucket).Scan(&backendETag))
	require.NotEmpty(t, backendETag, "the row records the stored bytes' MD5")
	// A second sealed object whose row has no backend_etag: skipped.
	f.putObject("no-etag.txt", []byte("another sealed body"), ssecHeaders(key))
	f.exec(`UPDATE object_head_cache SET backend_etag = NULL WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'no-etag.txt'`, f.tenantID, f.bucket)

	j := f.copiesJob()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM job_runs WHERE job = $1`, j.JobName) })
	res, err := j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Enqueued, "the sealed row with a backend_etag; not the one without")
	assert.Equal(t, 1, res.Copied)
	sk := h.sk
	if sk == "" {
		sk = "sealed.txt"
	}
	assert.Equal(t, "ready", f.copyState(sk))
	var etag string
	var size int64
	require.NoError(t, f.db.QueryRow(`SELECT etag, size FROM blob_copies WHERE tenant_id = $1 AND storage_key = $2`, f.tenantID, sk).Scan(&etag, &size))
	stored := f.storedBytes("sealed.txt")
	sum := md5.Sum(stored) // #nosec G401
	assert.Equal(t, hex.EncodeToString(sum[:]), strings.Trim(etag, `"`), "the copy row's etag is the sealed bytes' MD5")
	assert.Equal(t, int64(len(stored)), size, "and its size the stored length")
	assert.True(t, m.has(f.tenantID, f.container(), sk))
}

// storedBytes is the fake primary's bytes of a head row.
func (f *edgeFixture) storedBytes(key string) []byte {
	f.t.Helper()
	h, ok := f.head(key)
	require.True(f.t, ok)
	container, blob := objectBlobAddress(f.tenantID, f.bucket, key, h.sk)
	rc, err := f.drv.Get(common.WithTenantID(context.Background(), f.tenantID), container, blob)
	require.NoError(f.t, err)
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	require.NoError(f.t, err)
	return b
}

// RP96-15: a wrong blob with no mirror wrote one row per cycle for ever (×
// up to 1,000 for a chunk). A finding is one row per (tenant, address,
// verdict, file): found again, its found_at is the last sighting (the
// verified line's query reads it, unchanged), first_found_at the first,
// times the count.
func TestScrub_AFindingFoundAgainIsOneRow(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour
	f.putObject("bad.bin", testBytes(1000), nil)
	f.drv.mu.Lock()
	for k, b := range f.drv.objects {
		if len(b) == 1000 {
			f.drv.objects[k] = b[:999]
		}
	}
	f.drv.mu.Unlock()
	f.run()
	require.Len(t, f.findings(), 1)
	f.exec(`UPDATE scrub_findings SET found_at = NOW() - interval '2 days', first_found_at = NOW() - interval '2 days' WHERE tenant_id = $1`, f.tenantID)

	f.job.now = func() time.Time { return time.Now().Add(25 * time.Hour) } // the next cycle
	f.run()
	rows := f.findings()
	require.Len(t, rows, 1, "the same finding again is the same row")
	var times int
	var first, last time.Time
	require.NoError(t, f.db.QueryRow(`SELECT times, first_found_at, found_at FROM scrub_findings WHERE tenant_id = $1`, f.tenantID).Scan(&times, &first, &last))
	assert.Equal(t, 2, times)
	assert.True(t, first.Before(time.Now().Add(-47*time.Hour)), "first_found_at is the first sighting")
	assert.True(t, last.After(time.Now().Add(-time.Minute)), "found_at is the last")
}

// RP96-16: an edge-codec chunk that does not open was a `mismatch` — a key
// problem labelled as corrupt bytes, and "repaired" from a copy of the same
// bytes. Its stored MD5 (the row's ciphertext_hash, the backend's ETag of
// the sealed bytes) is compared first: wrong bytes are a mismatch; right
// bytes that do not open are labelled apart, and never repaired.
func TestScrub_ACodecChunkThatDoesNotOpenIsNotAMismatch(t *testing.T) {
	f := setupCodecFixture(t)
	j := newBlobScrubJob(f.srv, 24*time.Hour)
	require.NotNil(t, j)
	blob := testBytes(4096) // no 0x06 blob: it cannot open
	sum := md5.Sum(blob)    // #nosec G401
	it := scrubItem{kind: scrubKindChunk, scope: f.tenantID, hash: strings.Repeat("ab", 32), storageKey: "_chunks/x",
		encrypted: true, encAlgo: crypto.EdgeChunkEncryptionAlgo, ciphertextHash: hex.EncodeToString(sum[:]), keyVersion: 1}
	v, detail := j.verifyChunk(context.Background(), it, blob)
	assert.Equal(t, scrubUnopenable, v, detail)
	assert.Contains(t, detail, "its stored MD5 is the row's")

	other := append([]byte(nil), blob...)
	other[0] ^= 0xff
	v, _ = j.verifyChunk(context.Background(), it, other)
	assert.Equal(t, scrubMismatch, v, "wrong bytes are a mismatch")
}

// RP96-17: sia is never read by the scrub — read whole through the driver it
// bypassed hydrate-before-serve and spent the sia.storage read budget the
// customers share. Its rows are no cycle (so a bucket on sia gets no line).
func TestScrub_SiaIsNeverRead(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour
	sia := newEdgeFakeDriver()
	sia.name = "sia"
	f.srv.engine.AddDriver("sia", sia)
	f.putObject("s.bin", testBytes(1000), nil)
	f.exec(`UPDATE object_head_cache SET backend_name = 'sia' WHERE tenant_id = $1 AND bucket = $2 AND object_key = 's.bin'`, f.tenantID, f.bucket)
	sia.getHook = func(string) error { t.Error("the scrub read sia"); return nil }
	st := f.run().Backends["sia"]
	require.NotNil(t, st, "its state is carried")
	assert.Empty(t, st.Checked)
	assert.True(t, st.CycleStartedAt.IsZero())
	assert.Zero(t, sia.ranges.Load())
}
