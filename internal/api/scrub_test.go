package api

// WP-VAULT-2 Part 2 (Stage 1a Prompt 58 PR A): the blob scrub. Every hot
// blob — a head row with bytes, a retained version, a ready chunk — is read
// WHOLE on its recorded backend once per cycle and checked against its row;
// what is not ok is a scrub_findings row; a blob with a ready mirror copy
// is rewritten from it at the same address. The fixture is the edge
// fixture's fake driver, which holds bytes per address: a test corrupts one
// blob, deletes another and gives a third a mirror copy.

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

// blobScrubFixture is the edge fixture with the blob_scrub flag on for its
// tenant and a job scoped to it.
type blobScrubFixture struct {
	*edgeFixture
	job *BlobScrubJob
}

func setupBlobScrubFixture(t *testing.T, cycle time.Duration) *blobScrubFixture {
	t.Helper()
	f := setupEdgeFixture(t)
	require.NoError(t, f.fl.Set(context.Background(), flagBlobScrub, f.tenantID, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	j := newBlobScrubJob(f.srv, cycle)
	require.NotNil(t, j)
	j.JobName = "blob_scrub_test_" + f.tenantID
	j.capacityJob = "backend_capacity_test_" + f.tenantID
	j.onlyTenant = f.tenantID
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM scrub_findings WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM job_runs WHERE job IN ($1, $2)`, j.JobName, j.capacityJob)
	})
	return &blobScrubFixture{edgeFixture: f, job: j}
}

// run is one scheduled run: RunOnce, then its result in job_runs as the
// scheduler records it.
func (f *blobScrubFixture) run() BlobScrubResult {
	f.t.Helper()
	res, err := f.job.RunOnce(context.Background())
	require.NoError(f.t, err)
	raw, err := json.Marshal(res)
	require.NoError(f.t, err)
	f.exec(`INSERT INTO job_runs (job, last_outcome, result) VALUES ($1, 'ok', $2)
		ON CONFLICT (job) DO UPDATE SET result = EXCLUDED.result, last_outcome = 'ok'`, f.job.JobName, raw)
	return res
}

func (f *blobScrubFixture) findings() []scrubFindingRow {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT bucket, object_key, address, backend, kind, verdict, COALESCE(source, '') FROM scrub_findings
		WHERE tenant_id = $1 ORDER BY id`, f.tenantID)
	require.NoError(f.t, err)
	defer func() { _ = rows.Close() }()
	var out []scrubFindingRow
	for rows.Next() {
		var r scrubFindingRow
		require.NoError(f.t, rows.Scan(&r.bucket, &r.key, &r.address, &r.backend, &r.kind, &r.verdict, &r.source))
		out = append(out, r)
	}
	require.NoError(f.t, rows.Err())
	return out
}

type scrubFindingRow struct{ bucket, key, address, backend, kind, verdict, source string }

// blobOf is the fake driver's bytes at a head row's address.
func (f *blobScrubFixture) blobOf(key string) []byte {
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

func (f *blobScrubFixture) deleteBlob(key string) {
	f.t.Helper()
	h, ok := f.head(key)
	require.True(f.t, ok)
	container, blob := objectBlobAddress(f.tenantID, f.bucket, key, h.sk)
	require.NoError(f.t, f.drv.Delete(common.WithTenantID(context.Background(), f.tenantID), container, blob))
}

// chunk writes a ready chunk blob at its one address, its index row and a
// reference from one of the tenant's objects.
func (f *blobScrubFixture) chunk(data []byte) string {
	f.t.Helper()
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	key := "_chunks/" + hash
	require.NoError(f.t, f.drv.Put(engine.ChunkContext(context.Background()), engine.ChunkContainer, key, bytes.NewReader(data)))
	f.exec(`INSERT INTO global_content_index (plaintext_hash, backend_id, storage_key, size_bytes, ref_count, dedup_scope, state)
		VALUES ($1, 'edgefake', $2, $3, 1, '_global', 'ready')`, hash, key, len(data))
	f.exec(`INSERT INTO tenant_chunk_refs (tenant_id, bucket_name, object_key, chunk_index, chunk_offset, plaintext_hash, dedup_scope)
		VALUES ($1, $2, $3, 0, 0, $4, '_global')`, f.tenantID, f.bucket, "chunked.bin@"+hash[:8], hash)
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM tenant_chunk_refs WHERE plaintext_hash = $1`, hash)
		_, _ = f.db.Exec(`DELETE FROM global_content_index WHERE plaintext_hash = $1`, hash)
	})
	return hash
}

func (f *blobScrubFixture) versioned() {
	f.t.Helper()
	f.exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
}

func scrubChecked(backend string) float64 {
	return promtestutil.ToFloat64(scrubChecks.WithLabelValues(backend))
}

func scrubFailedN(backend string) float64 {
	return promtestutil.ToFloat64(scrubFailures.WithLabelValues(backend))
}

// --- the six ---

func TestScrub_ReadsEveryKindOnce(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour // one run's share is the whole cycle
	f.versioned()
	f.putObject("whole.bin", testBytes(3000), nil)
	f.putObject("ver.bin", testBytes(2000), nil)
	f.putObject("ver.bin", testBytes(2500), nil) // the first is a retained version now
	f.chunk(testBytes(1500))

	before := scrubChecked("edgefake")
	res := f.run()
	b := res.Backends["edgefake"]
	require.NotNil(t, b, "%+v", res)
	assert.Equal(t, map[string]int{"whole": 2, "version": 1, "chunk": 1}, b.Checked, "two head rows, one retained version, one chunk")
	assert.EqualValues(t, 3000+2500+2000+1500, b.Bytes, "every byte read")
	assert.Equal(t, before+4, scrubChecked("edgefake"))
	assert.False(t, b.CycleCompletedAt.IsZero(), "the cycle over the backend finished")
	assert.Empty(t, f.findings(), "nothing is wrong: nothing written")

	// The cycle is done: the next run reads nothing until it is due again.
	res = f.run()
	assert.Empty(t, res.Backends["edgefake"].Checked, "every blob once per cycle")
	assert.Equal(t, before+4, scrubChecked("edgefake"))
}

func TestScrub_VerifiesTheRowsDigest(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour
	f.putObject("clean.bin", testBytes(4000), nil)
	f.putObject("corrupt.bin", testBytes(4001), nil)
	f.putObject("gone.bin", testBytes(4002), nil)
	// A multipart ETag and no body_md5: nothing to compare — unverifiable,
	// counted, never a finding.
	f.putObject("multi.bin", testBytes(4003), nil)
	f.exec(`UPDATE object_head_cache SET etag = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-2', body_md5 = NULL WHERE tenant_id = $1 AND object_key = 'multi.bin'`, f.tenantID)
	bad := f.chunk(testBytes(1777))
	f.drv.tamper(t, "corrupt.bin")
	f.deleteBlob("gone.bin")
	f.drv.mu.Lock()
	for k, v := range f.drv.objects {
		if bytes.HasSuffix([]byte(k), []byte(bad)) {
			c := bytes.Clone(v)
			c[0] ^= 0xff
			f.drv.objects[k] = c
		}
	}
	f.drv.mu.Unlock()

	failedBefore := scrubFailedN("edgefake")
	res := f.run()
	b := res.Backends["edgefake"]
	require.NotNil(t, b)
	assert.Equal(t, 3, b.Failed)
	assert.Equal(t, 1, b.Unverifiable)
	assert.Equal(t, failedBefore+3, scrubFailedN("edgefake"))
	got := map[string]string{}
	for _, r := range f.findings() {
		got[r.kind+" "+r.key] = r.verdict
		assert.Equal(t, "edgefake", r.backend)
		assert.Equal(t, f.bucket, r.bucket)
		assert.NotEmpty(t, r.address)
	}
	assert.Equal(t, map[string]string{
		"whole corrupt.bin":            "mismatch",
		"whole gone.bin":               "missing",
		"chunk chunked.bin@" + bad[:8]: "mismatch",
	}, got, "the clean object and the multipart one write nothing")
}

func TestScrub_RepairsFromTheMirror(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour
	mirror := f.addMirrorLeg()
	body := testBytes(6000)
	f.putObject("mirrored.bin", body, nil)
	h, _ := f.head("mirrored.bin")
	container, blob := objectBlobAddress(f.tenantID, f.bucket, "mirrored.bin", h.sk)
	require.NoError(t, mirror.Put(common.WithTenantID(context.Background(), f.tenantID), container, blob, bytes.NewReader(body)))
	sum := md5.Sum(body) // #nosec G401
	f.exec(`INSERT INTO blob_copies (tenant_id, bucket, object_key, storage_key, source_backend, leg, kind, shard_index, state, etag, size)
		VALUES ($1, $2, 'mirrored.bin', $3, 'edgefake', 'edgemirror', 'mirror', 0, 'ready', $4, $5)`,
		f.tenantID, f.bucket, h.sk, hex.EncodeToString(sum[:]), len(body))
	f.drv.tamper(t, "mirrored.bin")
	require.NotEqual(t, body, f.blobOf("mirrored.bin"))

	res := f.run()
	assert.Equal(t, 1, res.Backends["edgefake"].Repaired)
	assert.Equal(t, body, f.blobOf("mirrored.bin"), "rewritten from the mirror at the same address")
	fs := f.findings()
	require.Len(t, fs, 2, "%+v", fs)
	assert.Equal(t, "mismatch", fs[0].verdict)
	assert.Equal(t, "repaired", fs[1].verdict)
	assert.Equal(t, "edgemirror", fs[1].source)
	assert.Equal(t, blob, fs[1].address)

	// A mirror copy that does not verify is never written over the blob.
	f.exec(`DELETE FROM scrub_findings WHERE tenant_id = $1`, f.tenantID)
	f.exec(`DELETE FROM job_runs WHERE job = $1`, f.job.JobName)
	f.drv.tamper(t, "mirrored.bin")
	mirror.tamper(t, "mirrored.bin")
	broken := f.blobOf("mirrored.bin")
	res = f.run()
	assert.Zero(t, res.Backends["edgefake"].Repaired)
	assert.Equal(t, broken, f.blobOf("mirrored.bin"), "left as it was")
	fs = f.findings()
	require.Len(t, fs, 1)
	assert.Equal(t, "mismatch", fs[0].verdict)
}

func TestScrub_NeverSpendsTheVendorsEgressAllowance(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour
	f.putObject("a.bin", testBytes(5000), nil)
	f.putObject("b.bin", testBytes(5000), nil)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM backend_plans WHERE backend = 'edgefake'`) })
	f.exec(`INSERT INTO backend_plans (backend, capacity_bytes, egress_free_multiple, term_start, term_end) VALUES ('edgefake', 1000000000000, 1, NOW(), NOW() + interval '1 year')
		ON CONFLICT (backend) DO UPDATE SET capacity_bytes = 1000000000000, egress_free_multiple = 1`)
	capacity := func(stored, vendorEgress int64) {
		r := BackendCapacityResult{Backends: map[string]*BackendBytes{"edgefake": {Total: stored}},
			Vendor: map[string]VendorView{"edgefake": {EgressMonthBytes: vendorEgress}}}
		raw, err := json.Marshal(r)
		require.NoError(t, err)
		f.exec(`INSERT INTO job_runs (job, last_outcome, result) VALUES ($1, 'ok', $2)
			ON CONFLICT (job) DO UPDATE SET result = EXCLUDED.result`, f.job.capacityJob, raw)
	}

	// The month's free egress (1 × 10,000 stored) already spent: not read.
	capacity(10000, 10000)
	before := scrubChecked("edgefake")
	res := f.run()
	b := res.Backends["edgefake"]
	require.NotNil(t, b)
	assert.Equal(t, "vendor_allowance", b.Skipped)
	assert.Empty(t, b.Checked)
	assert.Equal(t, before, scrubChecked("edgefake"), "nothing read")

	// 6,000 left: one blob of 5,000 fits, the second would pass it.
	capacity(10000, 4000)
	res = f.run()
	b = res.Backends["edgefake"]
	assert.Equal(t, map[string]int{"whole": 1}, b.Checked)
	assert.EqualValues(t, 5000, b.Bytes)
	assert.Equal(t, "vendor_allowance", b.Skipped, "stopped at the allowance")

	// No capacity run at all: a metered backend's allowance is unknown —
	// nothing is read (the blob copies' rule).
	f.exec(`DELETE FROM job_runs WHERE job = $1`, f.job.capacityJob)
	f.exec(`DELETE FROM job_runs WHERE job = $1`, f.job.JobName)
	res = f.run()
	assert.Equal(t, "vendor_allowance", res.Backends["edgefake"].Skipped)
	assert.Empty(t, res.Backends["edgefake"].Checked)
}

func TestScrub_CursorSurvivesARestart(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	for i := 0; i < 3; i++ {
		f.putObject(fmt.Sprintf("k%d.bin", i), testBytes(1000), nil)
	}
	// One run's share: 3,000 bytes over a day in runs of 8 h = 1,000 bytes.
	f.job.Every = 8 * time.Hour
	res := f.run()
	assert.Equal(t, map[string]int{"whole": 1}, res.Backends["edgefake"].Checked)
	first := res.Backends["edgefake"].Cursor

	// A restart: a new job reads its cursor from job_runs.result.
	j := newBlobScrubJob(f.srv, 24*time.Hour)
	j.JobName, j.capacityJob, j.onlyTenant, j.Every = f.job.JobName, f.job.capacityJob, f.tenantID, 8*time.Hour
	f.job = j
	res = f.run()
	b := res.Backends["edgefake"]
	assert.Equal(t, map[string]int{"whole": 1}, b.Checked, "the next blob, not the first again")
	assert.NotEqual(t, first, b.Cursor)
	res = f.run()
	assert.Equal(t, map[string]int{"whole": 1}, res.Backends["edgefake"].Checked)
	assert.False(t, res.Backends["edgefake"].CycleCompletedAt.IsZero(), "three runs, three blobs: the cycle is done")
}

func TestScrub_FindingsErasedWithTheAccount(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour
	f.putObject("lost.bin", testBytes(3000), nil)
	f.deleteBlob("lost.bin")
	f.run()
	require.Len(t, f.findings(), 1)

	var email string
	require.NoError(t, f.db.QueryRow(`SELECT email FROM tenants WHERE id = $1`, f.tenantID).Scan(&email))
	f.exec(`UPDATE users SET status = 'pending_deletion', deletion_scheduled_at = NOW() - interval '1 minute' WHERE id = $1`, f.userID)
	f.exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenantID)   // the runner deletes the objects first
	f.exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, f.tenantID) // the runner erases only a scheduled account
	_, err := account.NewService(f.db, zap.NewNop()).EraseRows(context.Background(), f.userID, f.tenantID, email, time.Now())
	require.NoError(t, err)
	assert.Empty(t, f.findings(), "erased with the account")
	assert.Contains(t, exportExcludedTables, "scrub_findings", "never exported: a finding is ours")
	_ = uuid.NewString
}

// The scrub's series reach the registry (RB2-6: vecs declared and never
// registered): the three counters per registered backend, the cycle gauges.
func TestScrub_MetricsAreRegistered(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour
	f.putObject("m.bin", testBytes(100), nil)
	f.run()
	f.srv.blobScrub = f.job
	f.srv.startTime, f.srv.healthChecker = time.Now(), NewBackendHealthChecker()
	f.srv.initMetrics()
	rr := httptest.NewRecorder()
	f.srv.handleMetrics(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rr.Body.String()
	for _, name := range []string{`vaultaire_scrub_checked_total{backend="edgefake"}`, `vaultaire_scrub_failed_total{backend="edgefake"}`,
		`vaultaire_scrub_bytes_total{backend="edgefake"}`, `vaultaire_scrub_cycle_completed_timestamp_seconds{backend="edgefake"}`,
		"vaultaire_scrub_cycle_seconds 86400"} {
		assert.Contains(t, body, name)
	}
}

// SCRUB_CYCLE: unset = dark; a Go duration or whole days, 1 d–365 d (the
// parity scrub's parser); a rejected value keeps the job dark.
func TestBlobScrubCycleFromEnv(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"": 0, "30d": 30 * 24 * time.Hour, "24h": 24 * time.Hour, "365d": 365 * 24 * time.Hour,
		"366d": 0, "12h": 0, "soon": 0, "0d": 0,
	} {
		got := blobScrubCycleFromEnv(func(string) string { return raw }, zap.NewNop())
		assert.Equal(t, want, got, "SCRUB_CYCLE=%q", raw)
	}
	assert.Nil(t, newBlobScrubJob(&Server{}, 0), "no cycle: no job")
}
