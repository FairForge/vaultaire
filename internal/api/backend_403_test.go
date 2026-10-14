package api

// 2026-10-11: the prod `idrive` key started answering 403 AccessDenied on
// every OBJECT call while HeadBucket and ListObjects kept working (the third
// credential death). These tests drive the REAL fixed-bucket driver
// (drivers.NewFixedBucketS3Driver, the prod idrive code path, SDK v2 error
// shapes included) against an httptest endpoint shaped like that account,
// and ask every customer-visible path what it answers — the path table in
// internal/engine/CLAUDE.md (Prompt 2b.5 F1, F2.3; from the review's
// red-team pass).

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
)

const idrive403AccessDenied = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message><RequestId>ID403</RequestId></Error>`

// forbiddenIDrive is the prod account's shape on 2026-10-11: HeadBucket 200,
// ListObjectsV2 200 (optionally listing keys), every object call 403 —
// HeadObject with NO body (the SDK then builds the code from the status text).
type forbiddenIDrive struct {
	srv         *httptest.Server
	objectCalls atomic.Int32
	listKeys    []string
	status      atomic.Int32 // 0 = 403 AccessDenied; 500 = InternalError
}

func (fi *forbiddenIDrive) code() (int, string) {
	if fi.status.Load() == http.StatusInternalServerError {
		return http.StatusInternalServerError, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>InternalError</Code><Message>We encountered an internal error.</Message></Error>`
	}
	return http.StatusForbidden, idrive403AccessDenied
}

func newForbiddenIDrive(t *testing.T, name string) (*drivers.IDriveDriver, *forbiddenIDrive) {
	t.Helper()
	fi := &forbiddenIDrive{}
	fi.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-amz-request-id", "ID403REQ")
		w.Header().Set("x-amz-id-2", "ID403HOST")
		path := strings.TrimPrefix(r.URL.Path, "/")
		isBucket := path == "vaultaire" || path == "vaultaire/"
		switch {
		case isBucket && r.Method == http.MethodHead:
			w.WriteHeader(http.StatusOK) // HeadBucket: the probe stays green
		case isBucket && r.Method == http.MethodGet:
			type obj struct {
				Key  string `xml:"Key"`
				Size int64  `xml:"Size"`
			}
			out := struct {
				XMLName     xml.Name `xml:"ListBucketResult"`
				Name        string   `xml:"Name"`
				IsTruncated bool     `xml:"IsTruncated"`
				Contents    []obj    `xml:"Contents"`
			}{Name: "vaultaire"}
			prefix := r.URL.Query().Get("prefix")
			for _, k := range fi.listKeys {
				if strings.HasPrefix(k, prefix) {
					out.Contents = append(out.Contents, obj{Key: k, Size: 29})
				}
			}
			w.Header().Set("Content-Type", "application/xml")
			_ = xml.NewEncoder(w).Encode(out)
		default:
			fi.objectCalls.Add(1)
			code, body := fi.code()
			if r.Method == http.MethodHead {
				w.WriteHeader(code) // HEAD: no body at all
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(fi.srv.Close)
	d, err := drivers.NewFixedBucketS3Driver(name, "AKIAID403", "secret", fi.srv.URL, "us-central-1", "vaultaire", zap.NewNop())
	require.NoError(t, err)
	return d, fi
}

func idrive403Ctx(tenantID string) context.Context {
	return common.WithTenantID(context.Background(), tenantID)
}

// openBreaker charges the backend's breaker to open with failing reads.
func openBreaker(t *testing.T, eng *engine.CoreEngine, backend string, tenantID string) {
	t.Helper()
	for i := 0; i < 6 && eng.GetFailoverStatus()[backend] != engine.StateOpen.String(); i++ {
		_, _ = eng.GetOn(idrive403Ctx(tenantID), backend, "c", "k")
	}
	require.Equal(t, engine.StateOpen.String(), eng.GetFailoverStatus()[backend])
}

// --- 0. the error shapes: a 403 on any object call is never a miss ------------

func TestIDrive403_DriverShapes_AreErrorsNeverMisses(t *testing.T) {
	d, fi := newForbiddenIDrive(t, "idrive")
	ctx := idrive403Ctx("tenant-a")

	// HeadBucket (the probe) is green while every object call is refused.
	require.NoError(t, d.HealthCheck(ctx), "the signed HeadBucket probe stays green")
	names, err := d.List(ctx, "c", "")
	require.NoError(t, err)
	assert.Empty(t, names)

	_, err = d.Get(ctx, "c", "k")
	require.Error(t, err)
	assert.False(t, isObjectMissingErr(err), "GET 403: %v", err)

	_, err = d.GetRange(ctx, "c", "k", 0, 10)
	require.Error(t, err)
	assert.False(t, isObjectMissingErr(err), "ranged GET 403: %v", err)

	ok, err := d.Exists(ctx, "c", "k")
	require.Error(t, err, "HEAD 403 (no body) must be an error, not (false, nil): %v", err)
	assert.False(t, ok)
	assert.False(t, isObjectMissingErr(err), "HEAD 403: %v", err)
	t.Logf("HEAD 403 shape: %v", err)

	err = d.Delete(ctx, "c", "k")
	require.Error(t, err)
	assert.False(t, isObjectMissingErr(err), "DELETE 403: %v", err)

	err = d.Put(ctx, "c", "k", bytes.NewReader([]byte("x")), engine.WithContentLength(1))
	require.Error(t, err)
	assert.False(t, isObjectMissingErr(err), "PUT 403: %v", err)
	assert.GreaterOrEqual(t, fi.objectCalls.Load(), int32(5))
}

// --- fixture: primary lyve (does not hold the bytes), idrive forbidden --------

type idrive403Fixture struct {
	*adapterTestFixture
	idrive *drivers.IDriveDriver
	fi     *forbiddenIDrive
}

func setupID403(t *testing.T) *idrive403Fixture {
	t.Helper()
	f := setupAdapterFixture(t)
	lyveDir, err := os.MkdirTemp("", "idrive403-lyve-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(lyveDir) })
	f.eng.AddDriver("lyve", drivers.NewLocalDriver(lyveDir, zap.NewNop()))
	f.eng.SetPrimary("lyve")
	d, fi := newForbiddenIDrive(t, "idrive")
	f.eng.AddDriver("idrive", d)
	return &idrive403Fixture{adapterTestFixture: f, idrive: d, fi: fi}
}

func (f *idrive403Fixture) seedHeadRow(t *testing.T, key string, size int, floor string) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, floor)
		VALUES ($1, 'test-bucket', $2, $3, 'deadbeef', 'application/octet-stream', 'idrive', $4)`, f.tenantID, key, size, floor)
	require.NoError(t, err)
}

func (f *idrive403Fixture) do(method, key string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, "/test-bucket/"+strings.ReplaceAll(url.PathEscape(key), "%2F", "/"), rd)
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	switch method {
	case http.MethodGet:
		f.adapter.HandleGet(w, req, "test-bucket", key)
	case http.MethodHead:
		s := &Server{logger: zap.NewNop(), engine: f.eng, db: f.db, testMode: true}
		s.handleHeadObject(w, req, &S3Request{Bucket: "test-bucket", Object: key, TenantID: f.tenantID, Method: method})
	case http.MethodPut:
		f.adapter.HandlePut(w, req, "test-bucket", key)
	case http.MethodDelete:
		f.adapter.HandleDelete(w, req, "test-bucket", key)
	}
	return w
}

// --- 1. GetObject -----------------------------------------------------------

func TestIDrive403_GetObject_BreakerClosedAndOpen_Is503(t *testing.T) {
	f := setupID403(t)
	f.seedHeadRow(t, "held.bin", 29, "standard")

	w := f.do(http.MethodGet, "held.bin", nil, nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "breaker closed: %s", w.Body.String())
	assert.Equal(t, "30", w.Header().Get("Retry-After"))
	assert.NotContains(t, w.Body.String(), "NoSuchKey")

	w = f.do(http.MethodGet, "held.bin", nil, map[string]string{"Range": "bytes=0-9"})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "range GET, breaker closed: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "NoSuchKey")

	openBreaker(t, f.eng, "idrive", f.tenantID)
	w = f.do(http.MethodGet, "held.bin", nil, nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "breaker open: %s", w.Body.String())
	assert.Equal(t, "30", w.Header().Get("Retry-After"))
	assert.NotContains(t, w.Body.String(), "NoSuchKey")

	w = f.do(http.MethodGet, "held.bin", nil, map[string]string{"Range": "bytes=0-9"})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "range GET, breaker open: %s", w.Body.String())
}

// --- 2. HeadObject is cache-only --------------------------------------------

func TestIDrive403_HeadObject_IsCacheOnly200(t *testing.T) {
	f := setupID403(t)
	f.seedHeadRow(t, "held.bin", 29, "standard")
	before := f.fi.objectCalls.Load()

	w := f.do(http.MethodHead, "held.bin", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "29", w.Header().Get("Content-Length"))
	assert.Equal(t, before, f.fi.objectCalls.Load(), "HEAD must not touch the backend")
}

// --- 3. DeleteObject keeps the row ------------------------------------------

func TestIDrive403_DeleteObject_Is503RowKept_ClosedAndOpen(t *testing.T) {
	f := setupID403(t)
	f.seedHeadRow(t, "held.bin", 29, "standard")

	w := f.do(http.MethodDelete, "held.bin", nil, nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "breaker closed: %s", w.Body.String())
	assert.True(t, headRowPresent(t, f.adapterTestFixture, "held.bin"))

	openBreaker(t, f.eng, "idrive", f.tenantID)
	w = f.do(http.MethodDelete, "held.bin", nil, nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "breaker open: %s", w.Body.String())
	assert.True(t, headRowPresent(t, f.adapterTestFixture, "held.bin"), "the row stays while the bytes may remain")

	// Batch delete: a per-key ServiceUnavailable, never a silent success.
	s := &Server{logger: zap.NewNop(), engine: f.eng, db: f.db, testMode: true}
	req := httptest.NewRequest("POST", "/test-bucket?delete", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	de := s.batchDeleteKey(req, f.tenant, "test-bucket", f.tenant.NamespaceContainer("test-bucket"), "held.bin", s.objectDeleteAftermath())
	require.NotNil(t, de)
	assert.Equal(t, ErrServiceUnavailable, de.Code)
	assert.True(t, headRowPresent(t, f.adapterTestFixture, "held.bin"))
}

// --- 3b. the jobs' shared gates ---------------------------------------------

func TestIDrive403_Jobs_ExistsOnAndDeleteOnAreErrors(t *testing.T) {
	f := setupID403(t)
	ctx := idrive403Ctx(f.tenantID)

	// dedup GC asks ExistsOn: must be an error.
	ok, err := f.eng.ExistsOn(ctx, "idrive", "c", "k")
	require.Error(t, err)
	assert.False(t, ok)

	// deletion runner's Delete of the recorded backend: an error, never a
	// miss → the tenant defers.
	err = f.eng.Delete(engine.WithRecordedBackend(ctx, "idrive"), "c", "k")
	require.Error(t, err)
	assert.False(t, isObjectMissingErr(err))
	assert.True(t, strings.Contains(err.Error(), "AccessDenied"), err.Error())

	// the sweep's deleteCopyOn / parity shard erase / demotion reclaim: error.
	err = deleteCopyOn(ctx, f.eng, "idrive", f.tenantID, "c", "k")
	require.Error(t, err)

}

// --- 4. PutObject while idrive is the primary --------------------------------

func TestIDrive403_PutStandard_PrimaryIDrive_ClosedIs503_OpenLandsOnLyve(t *testing.T) {
	f := setupID403(t)
	f.eng.SetPrimary("idrive")
	body := []byte("twenty-nine bytes of payload!")

	w := f.do(http.MethodPut, "new.bin", body, nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "breaker closed, body consumed: %s", w.Body.String())
	assert.Equal(t, "30", w.Header().Get("Retry-After"))
	assert.False(t, headRowPresent(t, f.adapterTestFixture, "new.bin"), "no row for bytes that landed nowhere")

	openBreaker(t, f.eng, "idrive", f.tenantID)
	w = f.do(http.MethodPut, "new.bin", body, nil)
	require.Equal(t, http.StatusOK, w.Code, "breaker open: the PUT falls over to lyve: %s", w.Body.String())
	var backend string
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(backend_name,'') FROM object_head_cache WHERE tenant_id=$1 AND bucket='test-bucket' AND object_key='new.bin'`, f.tenantID).Scan(&backend))
	assert.Equal(t, "lyve", backend, "the row records where the bytes went")

	// Read-back routes by the row (idrive repaired or not): 200 with the bytes.
	w = f.do(http.MethodGet, "new.bin", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, body, w.Body.Bytes())
}

// --- 1b. the chunked path ---------------------------------------------------

func TestIDrive403_ChunkedGet_RecordedOnIDrive(t *testing.T) {
	f := setupChunkingFixture(t)
	d, _ := newForbiddenIDrive(t, "idrive")
	f.eng.AddDriver("idrive", d)
	content := generateTestData(8 * 1024)
	putChunkedObject(t, f, "chunked.bin", content, "application/octet-stream")

	// Re-home every chunk row of this object on the forbidden backend.
	res, err := f.db.Exec(`UPDATE global_content_index SET backend_id = 'idrive' WHERE plaintext_hash IN (
		SELECT plaintext_hash FROM tenant_chunk_refs WHERE tenant_id = $1)`, f.tenantID)
	require.NoError(t, err)
	n, _ := res.RowsAffected()
	require.Greater(t, n, int64(0), "chunk rows re-homed")
	if f.adapter.gci != nil {
		// drop any cached chunk location
		rows, qerr := f.db.Query(`SELECT dedup_scope, plaintext_hash FROM global_content_index WHERE backend_id = 'idrive'`)
		require.NoError(t, qerr)
		for rows.Next() {
			var scope, hash string
			require.NoError(t, rows.Scan(&scope, &hash))
			f.adapter.gci.InvalidateCache(scope, hash)
		}
		require.NoError(t, rows.Err())
		_ = rows.Close()
	}

	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/test-bucket/chunked.bin", nil)
		req = req.WithContext(s3Ctx(req.Context(), f.tenant))
		w := httptest.NewRecorder()
		f.adapter.HandleGet(w, req, "test-bucket", "chunked.bin")
		return w
	}
	w := get()
	t.Logf("chunked GET, breaker closed: %d Retry-After=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	assert.NotContains(t, w.Body.String(), "NoSuchKey")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "a backend that cannot be asked is 503 + Retry-After, not 500")
	assert.Equal(t, "30", w.Header().Get("Retry-After"))

	openBreaker(t, f.eng, "idrive", f.tenantID)
	w = get()
	t.Logf("chunked GET, breaker open: %d Retry-After=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	assert.NotContains(t, w.Body.String(), "NoSuchKey")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// --- 5. the erasure sweep sees the listing but cannot delete ----------------

func TestIDrive403_ErasureSweep_ListWorksDeleteForbidden_Defers(t *testing.T) {
	f := setupID403(t)
	// A tenant walk on the forbidden backend: the listing names one blob
	// under the tenant's prefix, the delete of it is refused.
	f.fi.listKeys = []string{"t-" + f.tenantID + "/" + f.tenant.NamespaceContainer("test-bucket") + "/left.bin"}
	walker, ok := interface{}(f.idrive).(engine.TenantWalker)
	require.True(t, ok)
	var seen int
	err := walker.WalkTenant(idrive403Ctx(f.tenantID), f.tenantID, func(o engine.TenantObject) error {
		seen++
		return deleteCopyOn(idrive403Ctx(f.tenantID), f.eng, "idrive", f.tenantID, o.Container, o.Artifact)
	})
	assert.Equal(t, 1, seen, "the listing is served")
	require.Error(t, err, "the delete is refused: the tenant must defer, never 'erased'")
	assert.False(t, isObjectMissingErr(err))
}

// --- 6. the miss predicate never reads the customer's key (F1.1) ------------
//
// isObjectMissingErr / engine.IsObjectMiss fell back to substrings of the
// error TEXT, and every driver embeds container and artifact in that text
// ("idrive get <container>/<artifact>: ..."). A key that contained one of the
// words turned ANY backend error on that object into a "miss". Before: GET
// 404, DELETE 204 with the row dropped, breaker closed; PUT 500.

func TestIDrive403_KeyNamedNotFound_403BecomesNoSuchKeyAndDeleteDropsTheRow(t *testing.T) {
	f := setupID403(t)
	const key = "errors/page not found.html" // a common web asset name
	f.seedHeadRow(t, key, 29, "standard")

	w := f.do(http.MethodGet, key, nil, nil)
	t.Logf("GET: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "a 403 on the recorded backend must never read as NoSuchKey")

	w = f.do(http.MethodDelete, key, nil, nil)
	t.Logf("DELETE: %d", w.Code)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "30", w.Header().Get("Retry-After"))
	assert.True(t, headRowPresent(t, f.adapterTestFixture, key), "row dropped while the bytes remain on idrive")
	// Charged like any other failure: three more GETs make five, the
	// breaker opens (before: it stayed closed whatever the traffic).
	for i := 0; i < 3; i++ {
		w = f.do(http.MethodGet, key, nil, nil)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	}
	assert.Equal(t, engine.StateOpen.String(), f.eng.GetFailoverStatus()["idrive"])
}

// Same class of key, a 500 from the recorded backend (no 403 involved): the
// verdict was "not found" and the breaker never charged. And a PUT of such a
// key onto a failing primary was 500 (isBackendFailure said "not a failure",
// so the error was never ErrAllBackendsUnavailable).
func TestIDrive403_KeyNamedNotFound_500IsAMissToo_AndPutIsNot503(t *testing.T) {
	f := setupID403(t)
	f.fi.status.Store(http.StatusInternalServerError)
	const key = "logs/NotFound-2026.log"
	f.seedHeadRow(t, key, 29, "standard")

	w := f.do(http.MethodGet, key, nil, nil)
	t.Logf("GET with a 500 from idrive: %d", w.Code)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "a 500 on the recorded backend must never read as NoSuchKey")
	assert.Equal(t, "30", w.Header().Get("Retry-After"))
	for i := 0; i < 3; i++ { // four charges: one short of opening
		_ = f.do(http.MethodGet, key, nil, nil)
	}
	require.Equal(t, engine.StateClosed.String(), f.eng.GetFailoverStatus()["idrive"])

	f.eng.SetPrimary("idrive")
	body := []byte("twenty-nine bytes of payload!")
	w = f.do(http.MethodPut, "x/page not found.txt", body, nil)
	t.Logf("PUT of a 'not found' key with idrive 500: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "30", w.Header().Get("Retry-After"))
	assert.Equal(t, engine.StateOpen.String(), f.eng.GetFailoverStatus()["idrive"], "the PUT's failure is the fifth charge")
}

// --- 7. the chunked PUT reports the cause (F1.3) ----------------------------

// A cancelled sibling's error reached the handler before the cause did, and
// "context canceled" mapped to 500 (prod 2026-10-11 03:51). Before: firstErr
// was the first error to arrive, whatever it was.
func TestChunkStorePool_TheFirstRealErrorWinsOverACancelledSibling(t *testing.T) {
	cancelled := 0
	p := &chunkStorePool{cancel: func() { cancelled++ }}
	sibling := fmt.Errorf("store chunk aaaa: request abandoned before lyve: %w", context.Canceled)
	cause := fmt.Errorf("store chunk bbbb: %w: all backends failed: idrive put: AccessDenied", engine.ErrAllBackendsUnavailable)
	later := fmt.Errorf("store chunk cccc: %w", engine.ErrAllBackendsUnavailable)

	p.fail(sibling)
	p.fail(cause)
	p.fail(later)
	p.fail(sibling)

	assert.Same(t, cause, p.firstErr, "the first error that is not a cancellation")
	assert.Equal(t, 4, cancelled)
}

func TestIDrive403_ChunkedPut_PrimaryForbidden_Is503WithRetryAfter(t *testing.T) {
	f := setupChunkingFixture(t)
	// The forbidden primary is the only backend: every chunk store fails.
	d, _ := newForbiddenIDrive(t, "idrive")
	eng := engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "idrive"})
	eng.AddDriver("idrive", d)
	f.adapter = NewS3ToEngine(eng, f.db, zap.NewNop())
	f.adapter.gci = crypto.NewGlobalContentIndex(f.db)
	f.adapter.chunkingThreshold = 1024
	f.adapter.chunkStoreConcurrency = 8
	content := generateTestData(64 * 1024)

	req := httptest.NewRequest("PUT", "/test-bucket/chunked-put.bin", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "chunked-put.bin")

	t.Logf("chunked PUT on a forbidden primary: %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "30", w.Header().Get("Retry-After"))
	assert.False(t, headRowPresent(t, f, "chunked-put.bin"))
}
