package api

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/FairForge/vaultaire/internal/engine"
)

// generatedNameRefusingDriver is the multi-bridge sync driver's answer when
// a bridge refuses a name the DRIVER generated (a stripe piece) past the
// retries while another bridge is healthy: engine.ErrPartiallyUnavailable
// (bench 2026-10-12 finding 2 — it used to be ErrInvalidInput → 400).
type generatedNameRefusingDriver struct{ downDriver }

func (d *generatedNameRefusingDriver) Put(_ context.Context, _, _ string, r io.Reader, _ ...engine.PutOption) error {
	_, _ = io.Copy(io.Discard, r)
	return fmt.Errorf("%w: sync put big.bin (striped, 4 pieces): piece 2: bridge 3: the server refused a name the driver generated: unexpected status 400 Bad Request", engine.ErrPartiallyUnavailable)
}

// completeUploadWith runs initiate → one part → complete on server s and
// returns the complete's recorder.
func completeUploadWith(t *testing.T, s *Server, f *adapterTestFixture, key string) *httptest.ResponseRecorder {
	t.Helper()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM multipart_uploads WHERE tenant_id = $1`, f.tenantID) })
	req := func(method, target, body string) *http.Request {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		return r.WithContext(s3Ctx(r.Context(), f.tenant))
	}
	w := httptest.NewRecorder()
	s.handleInitiateMultipartUpload(w, req("POST", "/test-bucket/"+key+"?uploads", ""), "test-bucket", key)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &init))
	t.Cleanup(func() { _ = os.RemoveAll(multipartDir(init.UploadID)) })
	w = httptest.NewRecorder()
	s.handleUploadPart(w, req("PUT", "/test-bucket/"+key+"?partNumber=1&uploadId="+init.UploadID, "part bytes"), "test-bucket", key)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	body := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, w.Header().Get("ETag"))
	w = httptest.NewRecorder()
	s.handleCompleteMultipartUpload(w, req("POST", "/test-bucket/"+key+"?uploadId="+init.UploadID, body), "test-bucket", key)
	return w
}

// The 400 for a refused key is never silent: the first 1 GiB complete on
// the Sync tier answered 400 on 2026-10-12 and the server log held nothing
// but the keep-alive's status line.
func TestCompleteMultipartUpload_RefusedKeyIsLogged(t *testing.T) {
	f := setupAdapterFixture(t)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("sync", &nameRefusingDriver{downDriver{name: "sync"}})
	eng.SetPrimary("sync")
	core, logs := observer.New(zapcore.WarnLevel)
	s := &Server{engine: eng, db: f.db, logger: zap.New(core)}

	w := completeUploadWith(t, s, f, "CON")

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	entries := logs.FilterMessageSnippet("refused the key").All()
	require.Len(t, entries, 1, "one Warn line naming the cause: %v", logs.All())
	e := entries[0]
	assert.Equal(t, zapcore.WarnLevel, e.Level)
	fields := e.ContextMap()
	assert.Equal(t, "test-bucket", fields["bucket"])
	assert.Equal(t, "CON", fields["key"])
	assert.Contains(t, fields["upload_id"], "upload-")
	assert.Contains(t, fmt.Sprint(fields["error"]), "refused the name")
}

// A bridge's refusal of a name the driver generated is the backend's
// trouble: 503 + Retry-After, logged, never 400 "check your key".
func TestCompleteMultipartUpload_BridgeRefusedGeneratedNameIs503(t *testing.T) {
	f := setupAdapterFixture(t)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("sync", &generatedNameRefusingDriver{downDriver{name: "sync"}})
	eng.SetPrimary("sync")
	core, logs := observer.New(zapcore.WarnLevel)
	s := &Server{engine: eng, db: f.db, logger: zap.New(core)}

	w := completeUploadWith(t, s, f, "big.bin")

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrServiceUnavailable)
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	assert.NotContains(t, w.Body.String(), refusedKeySuggestion)
	assert.Len(t, logs.FilterMessageSnippet("unavailable").All(), 1, "%v", logs.All())
}

// PutObject's refused-key 400 is logged the same way.
func TestPutObject_RefusedKeyIsLogged(t *testing.T) {
	f := setupAdapterFixture(t)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("sync", &nameRefusingDriver{downDriver{name: "sync"}})
	eng.SetPrimary("sync")
	core, logs := observer.New(zapcore.WarnLevel)
	adapter := NewS3ToEngine(eng, f.db, zap.New(core))

	req := httptest.NewRequest("PUT", "/test-bucket/CON", strings.NewReader("hello"))
	req.ContentLength = 5
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	adapter.HandlePut(w, req, "test-bucket", "CON")

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	entries := logs.FilterMessageSnippet("refused the key").All()
	require.Len(t, entries, 1, "%v", logs.All())
	assert.Equal(t, "CON", entries[0].ContextMap()["artifact"])
}

// nameRefusingDriver is a backend that refuses a key it cannot hold, the
// way the WebDAV driver reports a name Sync's bridge refuses: wrapped
// engine.ErrInvalidInput, after reading the body.
type nameRefusingDriver struct{ downDriver }

func (d *nameRefusingDriver) Put(_ context.Context, _, _ string, r io.Reader, _ ...engine.PutOption) error {
	_, _ = io.Copy(io.Discard, r)
	return fmt.Errorf("bridge 3: sync put CON: %w: the server refused the name: unexpected status 400 Bad Request", engine.ErrInvalidInput)
}

// A key the target backend cannot hold is the client's error — 400
// InvalidArgument, never a retryable 503 (Sync's bridge refused a name; the
// API answered 503 and the client retried a request that can never succeed)
// and never stored on another backend.
func TestPutObject_KeyTheBackendRefusesIs400(t *testing.T) {
	f := setupAdapterFixture(t)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("sync", &nameRefusingDriver{downDriver{name: "sync"}})
	eng.SetPrimary("sync")
	adapter := NewS3ToEngine(eng, f.db, zap.NewNop())

	req := httptest.NewRequest("PUT", "/test-bucket/CON", strings.NewReader("hello"))
	req.ContentLength = 5
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	adapter.HandlePut(w, req, "test-bucket", "CON")

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrInvalidArgument)
	assert.Empty(t, w.Header().Get("Retry-After"))
}

func TestCompleteMultipartUpload_KeyTheBackendRefusesIs400(t *testing.T) {
	f := setupAdapterFixture(t)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("sync", &nameRefusingDriver{downDriver{name: "sync"}})
	eng.SetPrimary("sync")
	s := &Server{engine: eng, db: f.db, logger: zap.NewNop()}
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM multipart_uploads WHERE tenant_id = $1`, f.tenantID) })
	req := func(method, target, body string) *http.Request {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		return r.WithContext(s3Ctx(r.Context(), f.tenant))
	}

	w := httptest.NewRecorder()
	s.handleInitiateMultipartUpload(w, req("POST", "/test-bucket/CON?uploads", ""), "test-bucket", "CON")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &init))
	t.Cleanup(func() { _ = os.RemoveAll(multipartDir(init.UploadID)) })
	w = httptest.NewRecorder()
	s.handleUploadPart(w, req("PUT", "/test-bucket/CON?partNumber=1&uploadId="+init.UploadID, "part bytes"), "test-bucket", "CON")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	body := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, w.Header().Get("ETag"))
	w = httptest.NewRecorder()
	s.handleCompleteMultipartUpload(w, req("POST", "/test-bucket/CON?uploadId="+init.UploadID, body), "test-bucket", "CON")

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrInvalidArgument)
}
