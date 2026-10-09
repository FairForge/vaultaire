package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Prompt 2b.2 C2: a DELETE whose head row names a registered backend that
// fails must fail (503 + Retry-After), keeping the head row, so the client
// retries. Before (2b8adc3): the engine fell back to the primary, whose
// DELETE of an absent key succeeds — 204, row gone, bytes left on the
// recorded backend.

// downDeleter is a registered backend whose deletes fail like a refused
// connection until up is set; its object is gone after a successful one.
type downDeleter struct {
	up      atomic.Bool
	deleted atomic.Int32
}

func (d *downDeleter) Name() string { return "flaky" }
func (d *downDeleter) Get(_ context.Context, c, a string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}
func (d *downDeleter) Put(context.Context, string, string, io.Reader, ...engine.PutOption) error {
	return nil
}
func (d *downDeleter) Delete(context.Context, string, string) error {
	if !d.up.Load() {
		return errors.New("dial tcp 127.0.0.1:4922: connect: connection refused")
	}
	d.deleted.Add(1)
	return nil
}
func (d *downDeleter) List(context.Context, string, string) ([]string, error) { return nil, nil }
func (d *downDeleter) Exists(context.Context, string, string) (bool, error)   { return true, nil }
func (d *downDeleter) HealthCheck(context.Context) error                      { return nil }

// seedOnFlaky stores key through the adapter (on the local primary) and
// records it on the "flaky" backend, as a write to a target-only tier does.
func seedOnFlaky(t *testing.T, f *adapterTestFixture, key string) *downDeleter {
	t.Helper()
	d := &downDeleter{}
	f.eng.AddDriver("flaky", d)
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader([]byte("bytes on the recorded backend")))
	req.ContentLength = 29
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", key)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, err := f.db.Exec(`UPDATE object_head_cache SET backend_name = 'flaky' WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`, f.tenantID, key)
	require.NoError(t, err)
	return d
}

func headRowPresent(t *testing.T, f *adapterTestFixture, key string) bool {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`,
		f.tenantID, key).Scan(&n))
	return n == 1
}

func TestHandleDelete_ARecordedBackendThatFailsIsA503AndKeepsTheRow(t *testing.T) {
	// Arrange
	f := setupAdapterFixture(t)
	d := seedOnFlaky(t, f, "held.bin")
	del := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("DELETE", "/test-bucket/held.bin", nil)
		req = req.WithContext(s3Ctx(req.Context(), f.tenant))
		w := httptest.NewRecorder()
		f.adapter.HandleDelete(w, req, "test-bucket", "held.bin")
		return w
	}

	// Act
	w := del()

	// Assert
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "30", w.Header().Get("Retry-After"))
	assert.True(t, headRowPresent(t, f, "held.bin"), "the row stays: the client retries")
	assert.Zero(t, d.deleted.Load())

	// The backend is back: the retry deletes the bytes there, then the row.
	d.up.Store(true)
	w = del()
	assert.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.Equal(t, int32(1), d.deleted.Load())
	assert.False(t, headRowPresent(t, f, "held.bin"))
}

func TestBatchDeleteKey_ARecordedBackendThatFailsIsServiceUnavailable(t *testing.T) {
	f := setupAdapterFixture(t)
	d := seedOnFlaky(t, f, "batch.bin")
	s := &Server{logger: zap.NewNop(), engine: f.eng, db: f.db, testMode: true}
	req := httptest.NewRequest("POST", "/test-bucket?delete", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))

	de := s.batchDeleteKey(req, f.tenant, "test-bucket", f.tenant.NamespaceContainer("test-bucket"), "batch.bin", s.objectDeleteAftermath())

	require.NotNil(t, de)
	assert.Equal(t, ErrServiceUnavailable, de.Code, "a retryable per-key error, never InternalError")
	assert.True(t, headRowPresent(t, f, "batch.bin"))

	d.up.Store(true)
	assert.Nil(t, s.batchDeleteKey(req, f.tenant, "test-bucket", f.tenant.NamespaceContainer("test-bucket"), "batch.bin", s.objectDeleteAftermath()))
	assert.False(t, headRowPresent(t, f, "batch.bin"))
}
