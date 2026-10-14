package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/flags"
)

// Prompt 2b.6 G1: the WRITE side of a strict target. A `sync`-tier bucket of
// a flagged tenant on a process with no `sync` driver (a redeploy without
// SYNC_WEBDAV_URLS) used to resolve SYNC to the primary, store the object
// there and answer 200 — off the tier the bucket promises, recorded on the
// primary. Every write path (PUT, multipart Complete, CopyObject destination,
// a PUT the size of a chunked one) must answer 503 + Retry-After, write no
// head row, charge no quota, and count
// vaultaire_engine_strict_target_unregistered_total{backend="sync",op="put"}.
// The tests drive the handlers, never the inner functions: #675's own test
// called buildWriteCandidateList("sync") directly and passed while the API
// never reached it.

// syncFlagOn is a flag service on which every tenant has `sync_backend`.
func syncFlagOn() *flags.Service {
	fl := flags.New(nil, zap.NewNop())
	fl.Register(flagSyncBackend, true)
	return fl
}

func strictPutCount() float64 {
	return promtest.ToFloat64(engine.StrictUnregisteredCounter().WithLabelValues("sync", "put"))
}

// strictWriteFixture is the quota fixture (primary = local, real quota
// manager) with the sync flag on and test-bucket's tier set to `sync`.
func strictWriteFixture(t *testing.T) *quotaAccountingFixture {
	t.Helper()
	f := setupQuotaAccountingFixture(t, 100<<20)
	f.server.flags = syncFlagOn()
	return f
}

func setSyncTier(t *testing.T, f *quotaAccountingFixture) {
	t.Helper()
	seedPlacementBucket(t, f.tenantID, "test-bucket", "private", "sync")
}

func (f *quotaAccountingFixture) headRows(t *testing.T, key string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`,
		f.tenantID, key).Scan(&n))
	return n
}

func (f *quotaAccountingFixture) onPrimary(t *testing.T, key string) bool {
	t.Helper()
	local, ok := f.server.engine.GetDriver("local")
	require.True(t, ok)
	exists, err := local.Exists(f.ctx(context.Background()), f.tenant.NamespaceContainer("test-bucket"), key)
	require.NoError(t, err)
	return exists
}

func TestStrictTargetWrite_PutWithoutSyncDriver_Is503(t *testing.T) {
	f := strictWriteFixture(t)
	setSyncTier(t, f)
	before := strictPutCount()

	req := httptest.NewRequest("PUT", "/test-bucket/k.bin", bytes.NewReader([]byte("customer bytes")))
	req.ContentLength = 14
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handlePutObject(w, req, f.s3Req("test-bucket", "k.bin"))

	t.Logf("sync-tier PUT, no sync driver: %d Retry-After=%q strict put delta=%v", w.Code, w.Header().Get("Retry-After"), strictPutCount()-before)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "30", w.Header().Get("Retry-After"))
	assert.Equal(t, 0, f.headRows(t, "k.bin"), "no head row")
	assert.False(t, f.onPrimary(t, "k.bin"), "nothing on the primary")
	assert.Equal(t, int64(0), f.used(t), "no quota charged")
	assert.Equal(t, 1.0, strictPutCount()-before)
}

// The class is resolved again at Complete from the bucket's tier: an upload
// started while the sync driver was registered and completed after a
// redeploy dropped it must 503 — the upload row stays active for a retry or
// the reaper — and never land on the primary.
func TestStrictTargetWrite_MultipartCompleteAfterDriverDropped_Is503(t *testing.T) {
	f := strictWriteFixture(t)
	setSyncTier(t, f)

	// Before the redeploy: the sync driver is registered.
	withSync := engine.NewEngine(nil, zap.NewNop(), nil)
	withSync.AddDriver("local", drivers.NewLocalDriver(f.tempDir, zap.NewNop()))
	withSync.SetPrimary("local")
	withSync.AddDriver("sync", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	before := &Server{logger: zap.NewNop(), engine: withSync, db: f.db, quotaManager: f.qm, flags: f.server.flags, testMode: true}

	iReq := httptest.NewRequest("POST", "/test-bucket/mp.bin?uploads", nil)
	iReq = iReq.WithContext(f.ctx(iReq.Context()))
	iw := httptest.NewRecorder()
	before.handleInitiateMultipartUpload(iw, iReq, "test-bucket", "mp.bin")
	require.Equal(t, http.StatusOK, iw.Code, iw.Body.String())
	var init InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(iw.Body.Bytes(), &init))
	t.Cleanup(func() { _ = os.RemoveAll(multipartDir(init.UploadID)) })

	part := testBytes(64 << 10)
	pReq := httptest.NewRequest("PUT", fmt.Sprintf("/test-bucket/mp.bin?uploadId=%s&partNumber=1", init.UploadID), bytes.NewReader(part))
	pReq.ContentLength = int64(len(part))
	pReq = pReq.WithContext(f.ctx(pReq.Context()))
	pw := httptest.NewRecorder()
	before.handleUploadPart(pw, pReq, "test-bucket", "mp.bin")
	require.Equal(t, http.StatusOK, pw.Code, pw.Body.String())

	// After the redeploy: f.server's engine has no sync driver.
	strictBefore := strictPutCount()
	body := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, pw.Header().Get("ETag"))
	cReq := httptest.NewRequest("POST", "/test-bucket/mp.bin?uploadId="+init.UploadID, bytes.NewReader([]byte(body)))
	cReq = cReq.WithContext(f.ctx(cReq.Context()))
	cw := httptest.NewRecorder()
	f.server.handleCompleteMultipartUpload(cw, cReq, "test-bucket", "mp.bin")

	t.Logf("Complete of a sync-tier upload after the driver was dropped: %d Retry-After=%q", cw.Code, cw.Header().Get("Retry-After"))
	assert.Equal(t, http.StatusServiceUnavailable, cw.Code, cw.Body.String())
	assert.Equal(t, "30", cw.Header().Get("Retry-After"))
	assert.Equal(t, 0, f.headRows(t, "mp.bin"), "no head row")
	assert.False(t, f.onPrimary(t, "mp.bin"), "nothing on the primary")
	// The upload keeps holding its part (WP-R10-8 Part C); the Complete
	// reserved nothing more.
	assert.Equal(t, int64(len(part)), f.used(t), "only what the upload holds")
	assert.Equal(t, 1.0, strictPutCount()-strictBefore)
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM multipart_uploads WHERE upload_id = $1`, init.UploadID).Scan(&status))
	assert.Equal(t, "active", status, "the upload row stays for a retry or the reaper")
}

func TestStrictTargetWrite_CopyIntoSyncBucketWithoutDriver_Is503(t *testing.T) {
	f := strictWriteFixture(t)
	// The source is written while the bucket is still `auto`.
	require.Equal(t, http.StatusOK, f.put(t, "src.bin", []byte("source bytes")))
	usedBefore := f.used(t)
	setSyncTier(t, f)
	before := strictPutCount()

	req := httptest.NewRequest("PUT", "/test-bucket/dst.bin", nil)
	req.Header.Set("x-amz-copy-source", "/test-bucket/src.bin")
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handleCopyObject(w, req, f.s3Req("test-bucket", "dst.bin"))

	t.Logf("CopyObject into a sync-tier bucket, no sync driver: %d %s", w.Code, w.Body.String())
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, 0, f.headRows(t, "dst.bin"), "no destination row")
	assert.False(t, f.onPrimary(t, "dst.bin"), "no destination bytes on the primary")
	assert.Equal(t, usedBefore, f.used(t), "no quota charged for the destination")
	assert.Equal(t, 1.0, strictPutCount()-before)
}

// A SYNC object is never chunked (chunk blobs live on the primary): a PUT
// over the chunking threshold takes the plain path — whole on the bridge
// when it is registered, 503 when it is not, and never a manifest whose
// chunks sit on the primary.
func TestStrictTargetWrite_ChunkSizedPut_StaysWhole(t *testing.T) {
	put := func(t *testing.T, f *adapterTestFixture, key string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req = req.WithContext(s3Ctx(req.Context(), f.tenant))
		w := httptest.NewRecorder()
		f.adapter.HandlePut(w, req, "test-bucket", key)
		return w
	}
	chunkRefs := func(t *testing.T, f *adapterTestFixture) int {
		var n int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM tenant_chunk_refs WHERE tenant_id = $1`, f.tenantID).Scan(&n))
		return n
	}
	syncFixture := func(t *testing.T) *adapterTestFixture {
		f := setupChunkingFixture(t)
		fl := syncFlagOn()
		fl.Register(flagChunking, true)
		f.adapter.flags = fl
		seedPlacementBucket(t, f.tenantID, "test-bucket", "private", "sync")
		return f
	}
	body := generateTestData(8 * 1024) // 8 KiB, over the fixture's 1 KiB threshold

	t.Run("driver registered: whole on the bridge", func(t *testing.T) {
		f := syncFixture(t)
		bridge := drivers.NewLocalDriver(t.TempDir(), zap.NewNop())
		f.eng.AddDriver("sync", bridge)
		w := put(t, f, "big.bin", body)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.False(t, isChunkedInHeadCache(t, f, "big.bin"))
		assert.Equal(t, 0, chunkRefs(t, f), "no chunk rows")
		var backend string
		require.NoError(t, f.db.QueryRow(`SELECT backend_name FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'big.bin'`, f.tenantID).Scan(&backend))
		assert.Equal(t, "sync", backend)
	})

	t.Run("driver not registered: 503, no manifest", func(t *testing.T) {
		f := syncFixture(t)
		before := strictPutCount()
		w := put(t, f, "big.bin", body)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		assert.Equal(t, "30", w.Header().Get("Retry-After"))
		assert.Equal(t, 0, chunkRefs(t, f), "no chunk rows")
		var n int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM object_head_cache WHERE tenant_id = $1`, f.tenantID).Scan(&n))
		assert.Equal(t, 0, n, "no head row")
		assert.Equal(t, 1.0, strictPutCount()-before)
	})
}

// R6-04 is unchanged for every target-only backend that is NOT strict: a
// class whose backend is unregistered, or registered and failing, degrades
// to the primary (200). This change is for strictTargetBackends only.
func TestStrictTargetWrite_NonStrictTargetsStillDegradeToThePrimary(t *testing.T) {
	cases := []struct {
		name       string
		class      string // x-amz-storage-class header
		visibility string
		backend    string
		register   bool // registered but failing
	}{
		{"geyser unregistered", "GLACIER", "private", "geyser", false},
		{"geyser failing", "GLACIER", "private", "geyser", true},
		{"r2 failing", "", "public-read", "r2", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setupQuotaAccountingFixture(t, 100<<20)
			f.server.flags = syncFlagOn()
			seedPlacementBucket(t, f.tenantID, "test-bucket", tc.visibility, "auto")
			if tc.register {
				f.server.engine.AddDriver(tc.backend, &downDriver{name: tc.backend})
			}
			req := httptest.NewRequest("PUT", "/test-bucket/d.bin", bytes.NewReader([]byte("degrades")))
			req.ContentLength = 8
			if tc.class != "" {
				req.Header.Set("x-amz-storage-class", tc.class)
			}
			req = req.WithContext(f.ctx(req.Context()))
			w := httptest.NewRecorder()
			f.server.handlePutObject(w, req, f.s3Req("test-bucket", "d.bin"))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var backend string
			require.NoError(t, f.db.QueryRow(`SELECT backend_name FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'd.bin'`, f.tenantID).Scan(&backend))
			assert.Equal(t, "local", backend, "degraded to the primary (R6-04)")
		})
	}
}
