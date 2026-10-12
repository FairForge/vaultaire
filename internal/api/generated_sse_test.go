package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R8-1 follow-up A1 (Stage 1a Prompt 2A): no plaintext byte lands in an
// sse_enabled bucket, whatever writer. The generated objects — the GDPR
// export, a delivered access log, an inventory report — went through
// upsertWholeObjectHeadRow with encryption_algorithm = '' while `_exports`
// is created sse_enabled whenever a master key is set. The audit of every
// other writer that bypasses HandlePut found the same hole in CopyObject (a
// plaintext source copied into an sse_enabled bucket), the direct upload
// (a public-read bucket that is sse_enabled), and the read side of it on
// the CDN (an SSE-S3 object served as its raw ciphertext).

// sseFor is an SSE service on its own key store, the tenant's rows dropped
// after the test.
func sseFor(t *testing.T, db *sql.DB, tenantIDs ...string) *crypto.SSEService {
	t.Helper()
	sse, err := testSSEService(db, keyTestMasterA)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, id := range tenantIDs {
			_, _ = db.Exec(`DELETE FROM tenant_key_versions WHERE tenant_id = $1`, id)
		}
	})
	return sse
}

// requireSSEBlob: the bytes on the backend are a 0x02 SSE-S3 blob and the
// plaintext is not in them.
func requireSSEBlob(t *testing.T, path string, plaintext []byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "the blob on the backend")
	require.NotEmpty(t, raw)
	probe := plaintext
	if len(probe) > 64 {
		probe = probe[:64]
	}
	assert.Falsef(t, bytes.Contains(raw, probe), "plaintext on the backend (%d bytes, first byte 0x%02x)", len(raw), raw[0])
	assert.Equalf(t, crypto.SSEVersion, raw[0], "an SSE-S3 blob (first byte 0x%02x, %d bytes)", raw[0], len(raw))
	assert.Equal(t, len(plaintext)+crypto.SSEOverheadBytes, len(raw), "plaintext + the SSE overhead")
}

type headEnc struct {
	algo    string
	version sql.NullInt32
	size    int64
	etag    string
}

func readHeadEnc(t *testing.T, db *sql.DB, tenantID, bucket, key string) headEnc {
	t.Helper()
	var h headEnc
	require.NoError(t, db.QueryRow(`SELECT COALESCE(encryption_algorithm,''), encryption_key_version, size_bytes, etag
		FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key).Scan(&h.algo, &h.version, &h.size, &h.etag))
	return h
}

func refusedCount(kind, reason string) float64 {
	return testutil.ToFloat64(generatedObjectRefused.WithLabelValues(kind, reason))
}

func TestGeneratedObject_IntoSSEBucket_IsEncrypted(t *testing.T) {
	t.Run("GDPR export into _exports", func(t *testing.T) {
		f := setupExportFixture(t)
		f.seed()
		sse := sseFor(t, f.db, f.tenantID)
		f.svc.SetSSE(sse)
		f.server.sseService = sse

		id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
		require.NoError(t, err)
		f.run()
		st := f.status(id)
		require.Equal(t, "completed", st.Status, st.Error)

		var sseBucket bool
		require.NoError(t, f.db.QueryRow(`SELECT sse_enabled FROM buckets WHERE tenant_id = $1 AND name = $2`, f.tenantID, tenant.ExportsBucket).Scan(&sseBucket))
		require.True(t, sseBucket, "_exports is sse_enabled when the process has a master key")

		files := f.exportFiles()
		require.Len(t, files, 1)
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/"+tenant.ExportsBucket+"/"+st.ObjectKey, nil).WithContext(s3Ctx(context.Background(), f.tenant()))
		f.server.handleS3Request(rr, req)
		require.Equal(t, 200, rr.Code, rr.Body.String())
		plain := rr.Body.Bytes()
		t.Logf("export: %d plaintext bytes, blob %s", len(plain), files[0])
		requireSSEBlob(t, files[0], plain)
		assert.Equal(t, "AES256", rr.Header().Get("x-amz-server-side-encryption"))

		h := readHeadEnc(t, f.db, f.tenantID, tenant.ExportsBucket, st.ObjectKey)
		assert.Equal(t, crypto.SSEAlgorithm, h.algo, "the head row says SSE-S3")
		assert.Equal(t, int32(1), h.version.Int32, "the head row names the tenant key version")
		assert.Equal(t, int64(len(plain)), h.size, "the plaintext size, as the S3 path records it")
		assert.Equal(t, st.SizeBytes, h.size)
		assert.Equal(t, md5hex(plain), h.etag, "the plaintext MD5 ETag")
		var doc map[string]any
		require.NoError(t, json.Unmarshal(plain, &doc), "GET returns the JSON document")
	})

	t.Run("delivered access log", func(t *testing.T) {
		f := setupLoggingFixture(t)
		sse := sseFor(t, f.db, f.tenantID)
		f.server.sseService = sse
		w := newGeneratedObjectWriter(f.db, f.eng, nil, nil, zap.NewNop())
		w.sse = sse
		f.server.accessLogTracker.SetWriter(w)
		_, err := f.db.Exec(`UPDATE buckets SET logging_enabled = TRUE, logging_target_bucket = $3, logging_prefix = 'logs/' WHERE tenant_id = $1 AND name = $2`,
			f.tenantID, f.bucket, f.logBucket)
		require.NoError(t, err)
		_, err = f.db.Exec(`UPDATE buckets SET sse_enabled = TRUE WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.logBucket)
		require.NoError(t, err)
		_, err = f.db.Exec(`INSERT INTO s3_access_log (tenant_id, bucket, object_key, operation, status_code, bytes_sent, bytes_received, source_ip, user_agent, request_id, error_code, logged_at)
			VALUES ($1,$2,'secret-key-name','GetObject',200,1,0,'127.0.0.1','ua','rid','',NOW())`, f.tenantID, f.bucket)
		require.NoError(t, err)

		_, dErr := f.server.accessLogTracker.deliverLogs(context.Background())
		require.NoError(t, dErr)

		var key string
		require.NoError(t, f.db.QueryRow(`SELECT object_key FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2`, f.tenantID, f.logBucket).Scan(&key))
		rr := f.s3(t, "GET", "/"+f.logBucket+"/"+key, nil)
		require.Equal(t, 200, rr.Code, rr.Body.String())
		plain := rr.Body.Bytes()
		assert.Contains(t, string(plain), "GetObject secret-key-name 200")
		requireSSEBlob(t, filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.logBucket), key), plain)
		h := readHeadEnc(t, f.db, f.tenantID, f.logBucket, key)
		assert.Equal(t, crypto.SSEAlgorithm, h.algo)
		assert.True(t, h.version.Valid)
		assert.Equal(t, int64(len(plain)), h.size)
		assert.Equal(t, md5hex(plain), h.etag)
	})

	t.Run("inventory report", func(t *testing.T) {
		f := setupInventoryFixture(t)
		f.server.accessLogTracker = nil
		sse := sseFor(t, f.db, f.tenantID)
		f.server.sseService = sse
		_, err := f.db.Exec(`UPDATE buckets SET sse_enabled = TRUE WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.invBucket)
		require.NoError(t, err)
		req := httptest.NewRequest("PUT", "/"+f.bucket+"/inventoried.txt", strings.NewReader("hello"))
		req.ContentLength = 5
		req = req.WithContext(s3Ctx(req.Context(), f.tenant))
		rr := httptest.NewRecorder()
		f.server.handleS3Request(rr, req)
		require.Equal(t, 200, rr.Code, rr.Body.String())

		runner := NewInventoryRunner(f.db, f.eng, zap.NewNop())
		w := newGeneratedObjectWriter(f.db, f.eng, nil, nil, zap.NewNop())
		w.sse = sse
		runner.SetWriter(w)
		runner.GenerateReportNow(context.Background(), f.tenantID, f.bucket, f.invBucket, "inv/", "CSV")

		key := fmt.Sprintf("inv/%s/%sT00-00Z/manifest.csv", f.bucket, time.Now().UTC().Format("2006-01-02"))
		get := httptest.NewRequest("GET", "/"+f.invBucket+"/"+key, nil).WithContext(s3Ctx(context.Background(), f.tenant))
		rr = httptest.NewRecorder()
		f.server.handleS3Request(rr, get)
		require.Equal(t, 200, rr.Code, rr.Body.String())
		plain := rr.Body.Bytes()
		assert.Contains(t, string(plain), "inventoried.txt,5,")
		requireSSEBlob(t, filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.invBucket), key), plain)
		h := readHeadEnc(t, f.db, f.tenantID, f.invBucket, key)
		assert.Equal(t, crypto.SSEAlgorithm, h.algo)
		assert.Equal(t, md5hex(plain), h.etag)
	})
}

func TestGeneratedObject_TooLargeIsRefusedNeverPlaintext(t *testing.T) {
	t.Run("export: failed at once with a reason, no object", func(t *testing.T) {
		f := setupExportFixture(t)
		f.seed()
		sse := sseFor(t, f.db, f.tenantID)
		f.svc.SetSSE(sse)
		f.svc.writer.maxSSE = 512 // every export is larger
		before := refusedCount(generatedKindExport, refusedTooLargeForSSE)

		id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
		require.NoError(t, err)
		f.run()
		st := f.status(id)

		t.Logf("export over the SSE limit: status %q, error %q, %d blobs", st.Status, st.Error, len(f.exportFiles()))
		assert.Equal(t, "failed", st.Status, "a refusal is final: the next attempt would refuse again")
		assert.Contains(t, st.Error, "too large to be encrypted", "the reason the dashboard shows")
		assert.Empty(t, f.exportFiles(), "never stored plaintext")
		var heads int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`, f.tenantID, tenant.ExportsBucket).Scan(&heads))
		assert.Zero(t, heads)
		assert.Equal(t, before+1, refusedCount(generatedKindExport, refusedTooLargeForSSE))
	})

	t.Run("inventory: a note, nothing written", func(t *testing.T) {
		f := setupInventoryFixture(t)
		f.server.accessLogTracker = nil
		sse := sseFor(t, f.db, f.tenantID)
		_, err := f.db.Exec(`UPDATE buckets SET sse_enabled = TRUE WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.invBucket)
		require.NoError(t, err)
		_, err = f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type) VALUES ($1,$2,'x.txt',5,'e','text/plain')`, f.tenantID, f.bucket)
		require.NoError(t, err)
		runner := NewInventoryRunner(f.db, f.eng, zap.NewNop())
		w := newGeneratedObjectWriter(f.db, f.eng, nil, nil, zap.NewNop())
		w.sse = sse
		w.maxSSE = 16
		runner.SetWriter(w)
		before := refusedCount(generatedKindInventory, refusedTooLargeForSSE)

		err = runner.generateReport(context.Background(), f.tenantID, f.bucket, f.invBucket, "inv/", "CSV", time.Now())
		require.Error(t, err, "the report is not written: the run's note says so")
		assert.ErrorIs(t, err, errGeneratedTooLargeForSSE)
		var heads int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`, f.tenantID, f.invBucket).Scan(&heads))
		assert.Zero(t, heads)
		entries, _ := os.ReadDir(filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.invBucket)))
		assert.Empty(t, entries, "no blob")
		assert.Equal(t, before+1, refusedCount(generatedKindInventory, refusedTooLargeForSSE))
	})

	t.Run("no SSE service in this process: refused, rows kept", func(t *testing.T) {
		f := setupLoggingFixture(t)
		w := newGeneratedObjectWriter(f.db, f.eng, nil, nil, zap.NewNop()) // no master key on this slot
		f.server.accessLogTracker.SetWriter(w)
		_, err := f.db.Exec(`UPDATE buckets SET logging_enabled = TRUE, logging_target_bucket = $3, logging_prefix = 'logs/' WHERE tenant_id = $1 AND name = $2`,
			f.tenantID, f.bucket, f.logBucket)
		require.NoError(t, err)
		_, err = f.db.Exec(`UPDATE buckets SET sse_enabled = TRUE WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.logBucket)
		require.NoError(t, err)
		_, err = f.db.Exec(`INSERT INTO s3_access_log (tenant_id, bucket, object_key, operation, status_code, bytes_sent, bytes_received, source_ip, user_agent, request_id, error_code, logged_at)
			VALUES ($1,$2,'k','GetObject',200,1,0,'127.0.0.1','ua','rid','',NOW())`, f.tenantID, f.bucket)
		require.NoError(t, err)
		before := refusedCount(generatedKindAccessLog, refusedSSEUnavailable)

		_, dErr := f.server.accessLogTracker.deliverLogs(context.Background())
		require.NoError(t, dErr)

		var pending, heads int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM s3_access_log WHERE tenant_id=$1`, f.tenantID).Scan(&pending))
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2`, f.tenantID, f.logBucket).Scan(&heads))
		assert.Equal(t, 1, pending, "rows kept: a slot with the key delivers them")
		assert.Zero(t, heads)
		entries, _ := os.ReadDir(filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.logBucket)))
		assert.Empty(t, entries, "nothing on the backend")
		assert.Equal(t, before+1, refusedCount(generatedKindAccessLog, refusedSSEUnavailable))
	})
}

// The audit: CopyObject of a plaintext source into an sse_enabled bucket.
func TestCopyObject_PlaintextSourceIntoSSEBucket_IsEncrypted(t *testing.T) {
	f := setupLoggingFixture(t) // two buckets, a db-backed server
	sse := sseFor(t, f.db, f.tenantID)
	content := []byte("a plaintext object copied into an encrypted bucket")
	rr := f.s3(t, "PUT", "/"+f.bucket+"/src.txt", content)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	f.server.sseService = sse
	_, err := f.db.Exec(`UPDATE buckets SET sse_enabled = TRUE WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.logBucket)
	require.NoError(t, err)

	copyTo := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/"+f.logBucket+"/"+key, nil)
		req.Header.Set("x-amz-copy-source", "/"+f.bucket+"/src.txt")
		req = req.WithContext(s3Ctx(req.Context(), f.tenant))
		w := httptest.NewRecorder()
		f.server.handleS3Request(w, req)
		return w
	}
	w := copyTo("dst.txt")
	require.Equal(t, 200, w.Code, w.Body.String())

	requireSSEBlob(t, filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.logBucket), "dst.txt"), content)
	h := readHeadEnc(t, f.db, f.tenantID, f.logBucket, "dst.txt")
	assert.Equal(t, crypto.SSEAlgorithm, h.algo)
	assert.True(t, h.version.Valid)
	assert.Equal(t, int64(len(content)), h.size, "the plaintext size")
	assert.Equal(t, md5hex(content), h.etag, "the plaintext MD5: the copy's ETag is the source's")
	got := f.s3(t, "GET", "/"+f.logBucket+"/dst.txt", nil)
	require.Equal(t, 200, got.Code, got.Body.String())
	assert.Equal(t, content, got.Body.Bytes())

	// Over the SSE limit: refused before anything is written.
	saved := maxSSEObjectSize
	maxSSEObjectSize = 8
	t.Cleanup(func() { maxSSEObjectSize = saved })
	w = copyTo("too-big.txt")
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, w.Body.String())
	_, statErr := os.Stat(filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.logBucket), "too-big.txt"))
	assert.True(t, os.IsNotExist(statErr), "never stored plaintext")
	maxSSEObjectSize = saved

	// No SSE service on this process: refused, never plaintext.
	f.server.sseService = nil
	w = copyTo("no-key.txt")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	_, statErr = os.Stat(filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.logBucket), "no-key.txt"))
	assert.True(t, os.IsNotExist(statErr), "never stored plaintext")
}

// The audit: a PUT into an sse_enabled bucket on a process with no master
// key stored plaintext (shouldEncrypt needs the service). R8-04 made the
// read fail closed; the write is refused the same way.
func TestHandlePut_SSEBucketWithNoSSEService_IsRefused(t *testing.T) {
	f := setupAdapterFixture(t)
	markBucketSSE(t, f)
	f.adapter.sseService = nil

	req := httptest.NewRequest("PUT", "/test-bucket/plain.txt", bytes.NewReader([]byte("must not land plaintext")))
	req.ContentLength = 23
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "plain.txt")

	t.Logf("PUT into an sse_enabled bucket with no SSE service: %d", w.Code)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	_, statErr := os.Stat(filepath.Join(f.tempDir, f.tenant.NamespaceContainer("test-bucket"), "plain.txt"))
	assert.True(t, os.IsNotExist(statErr), "never stored plaintext")
}

// The audit: a public-read bucket can be sse_enabled (created with a master
// key, then made public on the dashboard). A direct upload puts plaintext
// straight on R2: refused at create, and at complete (the bucket changed in
// between) the uploaded bytes are removed.
func TestMgmtDirectUpload_SSEBucketIsRefused(t *testing.T) {
	f := setupVersioningFixture(t)
	stub := &stubR2{Driver: drivers.NewLocalDriver(t.TempDir(), f.server.logger)}
	f.eng.AddDriver("r2", stub)
	_, err := f.db.Exec(`UPDATE buckets SET visibility = 'public-read', sse_enabled = TRUE WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	require.NoError(t, err)

	w := f.mgmtBucketCall(t, "POST", f.bucket, "/direct-uploads", `{"key":"img/a.png","content_type":"image/png"}`, f.server.handleMgmtCreateDirectUpload)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "bucket_encrypted")
	assert.Equal(t, 0, stub.presigned)

	// Bytes uploaded while the bucket was not encrypted, completed after.
	ctx := common.WithTenantID(context.Background(), f.tenantID)
	require.NoError(t, stub.Driver.Put(ctx, f.tenantID+"_"+f.bucket, "img/b.png", strings.NewReader("png bytes")))
	w = f.mgmtBucketCall(t, "POST", f.bucket, "/direct-uploads/complete", `{"key":"img/b.png"}`, f.server.handleMgmtCompleteDirectUpload)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "bucket_encrypted")
	_, getErr := stub.Driver.Get(ctx, f.tenantID+"_"+f.bucket, "img/b.png")
	assert.Error(t, getErr, "the plaintext bytes are removed from R2")
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`, f.tenantID, f.bucket).Scan(&n))
	assert.Zero(t, n)
}

// The read side on another entry point: the CDN streamed the raw blob of an
// SSE-S3 object — ciphertext under the plaintext Content-Length.
func TestCDN_SSEObject_IsDecrypted(t *testing.T) {
	f := setupCDNFixture(t)
	sse := sseFor(t, f.db, f.tenantID)
	ctx := common.WithTenantID(context.Background(), f.tenantID)
	ct, v, err := sse.EncryptBytes(ctx, f.tenantID, f.content)
	require.NoError(t, err)
	_, err = f.server.engine.Put(ctx, f.tenantID+"_"+f.bucket, f.key, bytes.NewReader(ct))
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE object_head_cache SET encryption_algorithm = $4, encryption_key_version = $5
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, f.tenantID, f.bucket, f.key, crypto.SSEAlgorithm, v)
	require.NoError(t, err)
	f.server.sseService = sse

	get := func(rng string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, req)
		return w
	}
	w := get("")
	body, _ := io.ReadAll(w.Body)
	t.Logf("CDN GET of an SSE-S3 object: %d, %d bytes, first byte 0x%02x", w.Code, len(body), firstByte(body))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, f.content, body, "the plaintext, never the ciphertext")

	w = get("bytes=6-9")
	assert.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, f.content[6:10], w.Body.Bytes())

	// No SSE service on this process: fail closed (R8-04), never ciphertext.
	f.server.sseService = nil
	w = get("")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, w.Body.String(), string(ct[:16]))
}

func firstByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}

// The dashboard says why an export was refused for its size: the export
// service's error carries the marker the page looks for.
func TestExportTooLargeReason_IsTheOneTheDashboardShows(t *testing.T) {
	assert.Contains(t, errGeneratedTooLargeForSSE.Error(), handlers.ExportTooLargeToEncryptMarker)
}
