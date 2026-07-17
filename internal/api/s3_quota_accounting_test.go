package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"database/sql"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

// WP-1 quota accounting invariant under test:
//
//	tenant_quotas.storage_used_bytes == SUM(object_head_cache.size_bytes)
//
// per tenant, at rest, after every S3 write/delete operation. Sizes are
// LOGICAL bytes (chunked/deduped objects bill their logical size, not the
// physical deduplicated bytes).

type quotaAccountingFixture struct {
	server   *Server
	db       *sql.DB
	qm       *usage.QuotaManager
	tenant   *tenant.Tenant
	tenantID string
	tempDir  string
}

func setupQuotaAccountingFixture(t *testing.T, limitBytes int64) *quotaAccountingFixture {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	logger := zap.NewNop()

	tempDir, err := os.MkdirTemp("", "vaultaire-quota-acct-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	eng := engine.NewEngine(nil, logger, nil)
	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	qm := usage.NewQuotaManager(db)

	tenantUUID := uuid.New()
	tenantID := tenantUUID.String()

	_, err = db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, tenantID, "Quota Acct Test", "quota-acct-"+tenantID[:8]+"@test.local",
		"AK-"+tenantID[:8], "SK-"+tenantID[:8])
	require.NoError(t, err)

	require.NoError(t, qm.CreateTenant(context.Background(), tenantID, "starter", limitBytes))

	tn := &tenant.Tenant{
		ID:        tenantID,
		Namespace: "tenant/" + tenantID + "/",
	}
	container := tn.NamespaceContainer("test-bucket")
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, container), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, tn.NamespaceContainer("dest-bucket")), 0o755))

	server := &Server{
		logger:       logger,
		router:       chi.NewRouter(),
		engine:       eng,
		quotaManager: qm,
		db:           db,
		testMode:     true,
	}

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM quota_usage_events WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenant_quotas WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenant_chunk_refs WHERE tenant_id = $1", tenantUUID)
		_, _ = db.Exec(`DELETE FROM global_content_index g
			WHERE g.dedup_scope = $1
			   OR NOT EXISTS (
				SELECT 1 FROM tenant_chunk_refs r
				WHERE r.dedup_scope = g.dedup_scope AND r.plaintext_hash = g.plaintext_hash)`,
			tenantID)
		_, _ = db.Exec("DELETE FROM object_metadata WHERE tenant_id = $1", tenantUUID)
		_, _ = db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM multipart_parts WHERE upload_id IN (SELECT upload_id FROM multipart_uploads WHERE tenant_id = $1)", tenantID)
		_, _ = db.Exec("DELETE FROM multipart_uploads WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})

	return &quotaAccountingFixture{
		server:   server,
		db:       db,
		qm:       qm,
		tenant:   tn,
		tenantID: tenantID,
		tempDir:  tempDir,
	}
}

// used returns the tenant's current storage_used_bytes.
func (f *quotaAccountingFixture) used(t *testing.T) int64 {
	t.Helper()
	var used int64
	require.NoError(t, f.db.QueryRow(
		`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`,
		f.tenantID).Scan(&used))
	return used
}

func (f *quotaAccountingFixture) s3Req(bucket, object string) *S3Request {
	return &S3Request{Bucket: bucket, Object: object, TenantID: f.tenantID}
}

// ctx attaches both tenant context keys the real request path sets.
func (f *quotaAccountingFixture) ctx(ctx context.Context) context.Context {
	return common.WithTenantID(tenant.WithTenant(ctx, f.tenant), f.tenantID)
}

// put uploads body to test-bucket/key through the full server PUT path and
// returns the recorded HTTP status.
func (f *quotaAccountingFixture) put(t *testing.T, key string, body []byte) int {
	t.Helper()
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handlePutObject(w, req, f.s3Req("test-bucket", key))
	return w.Code
}

// del deletes test-bucket/key through the full server DELETE path.
func (f *quotaAccountingFixture) del(t *testing.T, key string) int {
	t.Helper()
	req := httptest.NewRequest("DELETE", "/test-bucket/"+key, nil)
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handleDeleteObject(w, req, f.s3Req("test-bucket", key))
	return w.Code
}

func testBytes(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i % 251)
	}
	return data
}

// --- Spec test 1: PUT counts once, not 2×. ---
func TestQuotaAccounting_PutCountsOnce(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20) // 100 MiB limit

	const size = 1 << 20 // 1 MiB
	require.Equal(t, 200, f.put(t, "once.bin", testBytes(size)))

	assert.Equal(t, int64(size), f.used(t),
		"a single PUT must count exactly once (was double-counted by API + engine reservations)")
}

// --- Spec test 2: PUT then DELETE returns to baseline. ---
func TestQuotaAccounting_PutDeleteBaseline(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)

	require.Equal(t, 200, f.put(t, "cycle.bin", testBytes(1<<20)))
	require.Equal(t, 204, f.del(t, "cycle.bin"))

	assert.Equal(t, int64(0), f.used(t),
		"DELETE must release the object's reserved bytes back to the tenant")
}

// --- Spec test 3: multipart complete over quota → 403, no usage recorded. ---
func TestQuotaAccounting_MultipartCompleteOverQuota(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 4<<10) // 4 KiB limit

	// Initiate
	initReq := httptest.NewRequest("POST", "/test-bucket/mp-over.bin?uploads", nil)
	initReq = initReq.WithContext(f.ctx(initReq.Context()))
	iw := httptest.NewRecorder()
	f.server.handleInitiateMultipartUpload(iw, initReq, "test-bucket", "mp-over.bin")
	require.Equal(t, 200, iw.Code)
	var initRes InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(iw.Body.Bytes(), &initRes))
	uploadID := initRes.UploadID
	require.NotEmpty(t, uploadID)

	// Upload one 8 KiB part (over the 4 KiB limit once assembled)
	part := testBytes(8 << 10)
	pReq := httptest.NewRequest("PUT",
		fmt.Sprintf("/test-bucket/mp-over.bin?uploadId=%s&partNumber=1", uploadID),
		bytes.NewReader(part))
	pReq.ContentLength = int64(len(part))
	pReq = pReq.WithContext(f.ctx(pReq.Context()))
	pw := httptest.NewRecorder()
	f.server.handleUploadPart(pw, pReq, "test-bucket", "mp-over.bin")
	require.Equal(t, 200, pw.Code)

	// Complete must be rejected with 403 QuotaExceeded (not 500)
	cReq := httptest.NewRequest("POST",
		fmt.Sprintf("/test-bucket/mp-over.bin?uploadId=%s", uploadID), nil)
	cReq = cReq.WithContext(f.ctx(cReq.Context()))
	cw := httptest.NewRecorder()
	f.server.handleCompleteMultipartUpload(cw, cReq, "test-bucket", "mp-over.bin")

	assert.Equal(t, 403, cw.Code, "multipart complete over quota must return 403")
	assert.Contains(t, cw.Body.String(), "QuotaExceeded")
	assert.Equal(t, int64(0), f.used(t), "rejected multipart complete must not consume quota")
}

// Multipart complete within quota reserves the assembled size exactly once,
// and an overwrite via multipart releases the previous object's size.
func TestQuotaAccounting_MultipartCompleteCounts(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)

	// Pre-existing object at the same key: multipart complete overwrites it.
	require.Equal(t, 200, f.put(t, "mp.bin", testBytes(1<<20)))

	initReq := httptest.NewRequest("POST", "/test-bucket/mp.bin?uploads", nil)
	initReq = initReq.WithContext(f.ctx(initReq.Context()))
	iw := httptest.NewRecorder()
	f.server.handleInitiateMultipartUpload(iw, initReq, "test-bucket", "mp.bin")
	require.Equal(t, 200, iw.Code)
	var initRes InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(iw.Body.Bytes(), &initRes))

	const partSize = 5 << 20
	for pn := 1; pn <= 2; pn++ {
		part := testBytes(partSize)
		pReq := httptest.NewRequest("PUT",
			fmt.Sprintf("/test-bucket/mp.bin?uploadId=%s&partNumber=%d", initRes.UploadID, pn),
			bytes.NewReader(part))
		pReq.ContentLength = int64(len(part))
		pReq = pReq.WithContext(f.ctx(pReq.Context()))
		pw := httptest.NewRecorder()
		f.server.handleUploadPart(pw, pReq, "test-bucket", "mp.bin")
		require.Equal(t, 200, pw.Code)
	}

	cReq := httptest.NewRequest("POST",
		fmt.Sprintf("/test-bucket/mp.bin?uploadId=%s", initRes.UploadID), nil)
	cReq = cReq.WithContext(f.ctx(cReq.Context()))
	cw := httptest.NewRecorder()
	f.server.handleCompleteMultipartUpload(cw, cReq, "test-bucket", "mp.bin")
	require.Equal(t, 200, cw.Code)

	assert.Equal(t, int64(2*partSize), f.used(t),
		"multipart complete must count the assembled size once and release the overwritten object")
}

// --- Spec test 4: CopyObject over quota → 403, usage unchanged. ---
func TestQuotaAccounting_CopyObjectOverQuota(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 6<<10) // 6 KiB limit

	require.Equal(t, 200, f.put(t, "src.bin", testBytes(4<<10)))
	require.Equal(t, int64(4<<10), f.used(t))

	// Copying 4 KiB more would hit 8 KiB > 6 KiB → 403.
	req := httptest.NewRequest("PUT", "/test-bucket/copy.bin", nil)
	req.Header.Set("x-amz-copy-source", "/test-bucket/src.bin")
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handleCopyObject(w, req, f.s3Req("test-bucket", "copy.bin"))

	assert.Equal(t, 403, w.Code, "CopyObject over quota must return 403")
	assert.Contains(t, w.Body.String(), "QuotaExceeded")
	assert.Equal(t, int64(4<<10), f.used(t), "rejected copy must not consume quota")
}

// CopyObject within quota counts the destination once.
func TestQuotaAccounting_CopyObjectCounts(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)

	const size = 256 << 10
	require.Equal(t, 200, f.put(t, "src.bin", testBytes(size)))

	req := httptest.NewRequest("PUT", "/test-bucket/copy.bin", nil)
	req.Header.Set("x-amz-copy-source", "/test-bucket/src.bin")
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handleCopyObject(w, req, f.s3Req("test-bucket", "copy.bin"))
	require.Equal(t, 200, w.Code)

	assert.Equal(t, int64(2*size), f.used(t),
		"source + copied destination must each count exactly once")
}

// --- Spec test 5: two concurrent PUTs jointly over limit → exactly one 403. ---
func TestQuotaAccounting_ConcurrentPutsExactlyOneRejected(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 3<<19) // 1.5 MiB limit

	const size = 1 << 20 // 1 MiB each; only one fits
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.put(t, fmt.Sprintf("conc-%d.bin", i), testBytes(size))
		}(i)
	}
	wg.Wait()

	ok, rejected := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 403:
			rejected++
		}
	}
	assert.Equal(t, 1, ok, "exactly one concurrent PUT must succeed (got %v)", codes)
	assert.Equal(t, 1, rejected, "exactly one concurrent PUT must be rejected (got %v)", codes)
	assert.Equal(t, int64(size), f.used(t), "only the successful PUT may consume quota")
}

// --- Spec test 6: overwrite reflects the delta, not the sum. ---
func TestQuotaAccounting_OverwriteReflectsDelta(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)

	require.Equal(t, 200, f.put(t, "grow.bin", testBytes(1<<20))) // 1 MiB
	require.Equal(t, 200, f.put(t, "grow.bin", testBytes(1<<10))) // overwrite with 1 KiB

	assert.Equal(t, int64(1<<10), f.used(t),
		"overwriting 1 MiB with 1 KiB must leave usage at 1 KiB")

	// And growing again must reflect the new size, not accumulate.
	require.Equal(t, 200, f.put(t, "grow.bin", testBytes(2<<20)))
	assert.Equal(t, int64(2<<20), f.used(t))
}

// A failed PUT (body error mid-stream) must release its reservation.
type failingReader struct {
	remaining int
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, fmt.Errorf("simulated client disconnect")
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	r.remaining -= n
	return n, nil
}

func TestQuotaAccounting_PutFailureReleasesReservation(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)

	req := httptest.NewRequest("PUT", "/test-bucket/fail.bin",
		io.NopCloser(&failingReader{remaining: 10 << 10}))
	req.ContentLength = 1 << 20 // declares 1 MiB, errors after 10 KiB
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handlePutObject(w, req, f.s3Req("test-bucket", "fail.bin"))

	require.NotEqual(t, 200, w.Code, "interrupted PUT must not succeed")
	assert.Equal(t, int64(0), f.used(t),
		"failed PUT must release its up-front reservation")
}

// Batch delete (POST /{bucket}?delete) must release every deleted object.
func TestQuotaAccounting_BatchDeleteReleases(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)

	require.Equal(t, 200, f.put(t, "batch-a.bin", testBytes(4<<10)))
	require.Equal(t, 200, f.put(t, "batch-b.bin", testBytes(2<<10)))
	require.Equal(t, int64(6<<10), f.used(t))

	body := `<Delete><Object><Key>batch-a.bin</Key></Object><Object><Key>batch-b.bin</Key></Object></Delete>`
	req := httptest.NewRequest("POST", "/test-bucket?delete", bytes.NewReader([]byte(body)))
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handleDeleteObjects(w, req, f.s3Req("test-bucket", ""))
	require.Equal(t, 200, w.Code)

	assert.Equal(t, int64(0), f.used(t),
		"batch delete must release every deleted object's bytes")
}

// Chunked (deduplicated) objects bill LOGICAL size on PUT and release
// LOGICAL size on DELETE — dedup means physical bytes ≠ logical bytes.
func TestQuotaAccounting_ChunkedPutDeleteLogicalSize(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 500<<20)
	f.server.gci = crypto.NewGlobalContentIndex(f.db)

	size := 65 << 20 // above the 64 MiB chunking threshold
	require.Equal(t, 200, f.put(t, "chunked.bin", testBytes(size)))

	var isChunked bool
	require.NoError(t, f.db.QueryRow(`
		SELECT is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "chunked.bin").Scan(&isChunked))
	require.True(t, isChunked, "object must take the chunked path for this test to be meaningful")

	assert.Equal(t, int64(size), f.used(t),
		"chunked PUT must reserve the logical size exactly once")

	require.Equal(t, 204, f.del(t, "chunked.bin"))
	assert.Equal(t, int64(0), f.used(t),
		"chunked DELETE must release the logical size")
}

// awsChunkedFrame wraps data in aws-chunked wire framing:
// "<hex-size>\r\n<data>\r\n0\r\n\r\n".
func awsChunkedFrame(data []byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%x\r\n", len(data))
	b.Write(data)
	b.WriteString("\r\n0\r\n\r\n")
	return b.Bytes()
}

// A client that under-declares x-amz-decoded-content-length while streaming
// more decoded bytes must be rejected — otherwise it stores data billed at
// the declared (tiny) size and poisons object_head_cache so reconciliation
// can never detect the theft.
func TestQuotaAccounting_AwsChunkedSizeLieRejected(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)

	payload := testBytes(64 << 10) // streams 64 KiB
	framed := awsChunkedFrame(payload)
	req := httptest.NewRequest("PUT", "/test-bucket/liar.bin", bytes.NewReader(framed))
	req.ContentLength = int64(len(framed))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("x-amz-decoded-content-length", "10") // declares 10 bytes
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handlePutObject(w, req, f.s3Req("test-bucket", "liar.bin"))

	assert.Equal(t, 400, w.Code, "under-declared aws-chunked PUT must be rejected")
	var rows int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, "test-bucket", "liar.bin").Scan(&rows))
	assert.Equal(t, 0, rows, "no head-cache row may record the lied-about size")
	assert.Equal(t, int64(0), f.used(t), "rejected PUT must not consume quota")
}

// An honest aws-chunked PUT (declared == streamed) still works and bills once.
func TestQuotaAccounting_AwsChunkedHonestPutCounts(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)

	payload := testBytes(64 << 10)
	framed := awsChunkedFrame(payload)
	req := httptest.NewRequest("PUT", "/test-bucket/honest.bin", bytes.NewReader(framed))
	req.ContentLength = int64(len(framed))
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("x-amz-decoded-content-length", fmt.Sprintf("%d", len(payload)))
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handlePutObject(w, req, f.s3Req("test-bucket", "honest.bin"))

	require.Equal(t, 200, w.Code)
	assert.Equal(t, int64(len(payload)), f.used(t))
}

// A PUT with no determinable size (no Content-Length, no
// x-amz-decoded-content-length) must be rejected with MissingContentLength —
// it cannot be quota-checked before storage.
func TestQuotaAccounting_UnknownLengthPutRejected(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)

	req := httptest.NewRequest("PUT", "/test-bucket/nolen.bin",
		io.NopCloser(bytes.NewReader(testBytes(4<<10))))
	req.ContentLength = -1
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handlePutObject(w, req, f.s3Req("test-bucket", "nolen.bin"))

	assert.Equal(t, 411, w.Code, "unknown-length PUT must return 411 MissingContentLength")
	assert.Equal(t, int64(0), f.used(t))
}

// Batch delete of a chunked object must decrement GCI chunk refs (not just
// drop the head-cache row) and release the logical size.
func TestQuotaAccounting_BatchDeleteChunkedReleasesRefs(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 500<<20)
	f.server.gci = crypto.NewGlobalContentIndex(f.db)

	size := 65 << 20
	require.Equal(t, 200, f.put(t, "chunk-batch.bin", testBytes(size)))
	require.Equal(t, int64(size), f.used(t))

	body := `<Delete><Object><Key>chunk-batch.bin</Key></Object></Delete>`
	req := httptest.NewRequest("POST", "/test-bucket?delete", bytes.NewReader([]byte(body)))
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handleDeleteObjects(w, req, f.s3Req("test-bucket", ""))
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "<Deleted>")

	assert.Equal(t, int64(0), f.used(t), "batch delete must release the chunked object's logical size")

	tenantUUID, err := uuid.Parse(f.tenantID)
	require.NoError(t, err)
	var refs int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3`,
		tenantUUID, "test-bucket", "chunk-batch.bin").Scan(&refs))
	assert.Equal(t, 0, refs, "batch delete must decrement GCI chunk refs like single delete does")
}

// Copying an encrypted source must be refused (NotImplemented): the copy
// path streams raw stored bytes, so it would both bill ciphertext size and
// hand back undecryptable data on GET.
func TestQuotaAccounting_CopyEncryptedSourceRejected(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)

	content := testBytes(4 << 10)
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.server.engine.Put(context.Background(), container, "enc-src.bin", bytes.NewReader(content))
	require.NoError(t, err)
	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, encryption_algorithm, updated_at)
		VALUES ($1, $2, $3, $4, 'etag', 'application/octet-stream', 'AES256', NOW())`,
		f.tenantID, "test-bucket", "enc-src.bin", len(content))
	require.NoError(t, err)

	req := httptest.NewRequest("PUT", "/test-bucket/enc-copy.bin", nil)
	req.Header.Set("x-amz-copy-source", "/test-bucket/enc-src.bin")
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handleCopyObject(w, req, f.s3Req("test-bucket", "enc-copy.bin"))

	assert.Equal(t, 501, w.Code, "copying an encrypted source must be NotImplemented, not silent corruption")
}

// The reconciliation job rewrites storage_used_bytes from the
// object_head_cache sum (Gate C runs this once before metered billing).
func TestQuotaAccounting_ReconcileRepairsdrift(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)

	require.Equal(t, 200, f.put(t, "rec-a.bin", testBytes(3<<10)))
	require.Equal(t, 200, f.put(t, "rec-b.bin", testBytes(5<<10)))

	// Simulate historical drift (the pre-WP-1 double counting).
	_, err := f.db.Exec(
		`UPDATE tenant_quotas SET storage_used_bytes = 999999999 WHERE tenant_id = $1`,
		f.tenantID)
	require.NoError(t, err)

	n, err := f.qm.ReconcileStorageUsage(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1))

	assert.Equal(t, int64(8<<10), f.used(t),
		"reconciliation must restore usage to the object_head_cache sum")
}
