package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// fullDriver is a backend whose account is full: every Put is the driver's
// ErrBackendFull (Blomp's Swift: 413 "Upload exceeds quota.").
type fullDriver struct{ downDriver }

func (d *fullDriver) Put(_ context.Context, _, _ string, r io.Reader, _ ...engine.PutOption) error {
	_, _ = io.Copy(io.Discard, r)
	return fmt.Errorf("swift PUT k: HTTP 413: Upload exceeds quota.: %w", engine.ErrBackendFull)
}

func s3XMLCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Code string `xml:"Code"`
	}
	require.NoError(t, xml.Unmarshal(body, &e), string(body))
	return e.Code
}

// Prod 2026-10-21: past Blomp's 40 GiB every PUT into the blomp tier
// answered 503 "All storage backends are temporarily unavailable. Please
// retry." — a full account is not a hiccup and a retry cannot succeed.
// The answer is 507 InsufficientStorage, no Retry-After, and nothing stored.
func TestHandlePut_FullBackendIs507NotRetry(t *testing.T) {
	f := setupAdapterFixture(t)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("full", &fullDriver{downDriver{name: "full"}})
	eng.SetPrimary("full")
	f.adapter.engine = eng

	req := httptest.NewRequest("PUT", "/test-bucket/k.bin", bytes.NewReader([]byte("hello")))
	req.ContentLength = 5
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "k.bin")

	assert.Equal(t, http.StatusInsufficientStorage, w.Code, w.Body.String())
	assert.Equal(t, ErrInsufficientStorage, s3XMLCode(t, w.Body.Bytes()))
	assert.Empty(t, w.Header().Get("Retry-After"), "nothing to retry until the storage grows")
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM object_head_cache WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	assert.Zero(t, n, "no head row")
}

func TestCompleteMultipartUpload_FullBackendIs507(t *testing.T) {
	f := setupAdapterFixture(t)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("full", &fullDriver{downDriver{name: "full"}})
	eng.SetPrimary("full")
	s := &Server{engine: eng, db: f.db, logger: zap.NewNop()}

	w := completeUploadWith(t, s, f, "big.bin")

	assert.Equal(t, http.StatusInsufficientStorage, w.Code, w.Body.String())
	assert.Equal(t, ErrInsufficientStorage, s3XMLCode(t, w.Body.Bytes()))
	assert.Empty(t, w.Header().Get("Retry-After"))
}
