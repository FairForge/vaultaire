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

	"github.com/FairForge/vaultaire/internal/engine"
)

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
