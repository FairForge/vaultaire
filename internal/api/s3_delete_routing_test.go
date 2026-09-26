package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
)

// R6-05: a DELETE must reach the backend recorded in object_head_cache even
// when the engine's in-memory routing map is cold (every restart). Before the
// fix the delete went to the primary, iDrive answered "not found", the API
// treated that as an idempotent miss and removed the head row — the bytes
// stayed on Lyve / Geyser / R2 / the region driver forever.
func TestHandleDelete_RoutesToRecordedBackendAfterRestart(t *testing.T) {
	f := setupAdapterFixture(t)

	// A second local-disk driver stands in for lyve (routing is under test).
	lyveDir, err := os.MkdirTemp("", "vaultaire-lyve-stub-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(lyveDir) })
	lyve := drivers.NewLocalDriver(lyveDir, zap.NewNop())
	f.eng.AddDriver("lyve", lyve)

	container := f.tenant.NamespaceContainer("test-bucket")
	require.NoError(t, os.MkdirAll(filepath.Join(lyveDir, container), 0750))
	ctx := common.WithTenantID(context.Background(), f.tenantID)

	content := []byte("resilient-tier bytes")
	backend, err := f.eng.Put(ctx, container, "on-lyve.bin", bytes.NewReader(content), engine.WithStorageClass("RESILIENT"))
	require.NoError(t, err)
	require.Equal(t, "lyve", backend)
	onLyve := filepath.Join(lyveDir, container, "on-lyve.bin")
	require.FileExists(t, onLyve)

	// What the PUT handler persists: the head row with the backend that holds the bytes.
	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name)
		VALUES ($1, 'test-bucket', 'on-lyve.bin', $2, 'etag', 'application/octet-stream', 'lyve')
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET backend_name = 'lyve'`,
		f.tenantID, len(content))
	require.NoError(t, err)

	// "Restart": a fresh engine over the same drivers has an empty routing map.
	fresh := engine.NewEngine(nil, zap.NewNop(), nil)
	fresh.AddDriver("local", drivers.NewLocalDriver(f.tempDir, zap.NewNop()))
	fresh.AddDriver("lyve", lyve)
	fresh.SetPrimary("local")
	adapter := NewS3ToEngine(fresh, f.db, zap.NewNop())

	req := httptest.NewRequest("DELETE", "/test-bucket/on-lyve.bin", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	adapter.HandleDelete(w, req, "test-bucket", "on-lyve.bin")

	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.NoFileExists(t, onLyve, "the bytes must be deleted from the backend that holds them")

	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'on-lyve.bin'`, f.tenantID).Scan(&n))
	assert.Zero(t, n, "head row removed")
}

// R6-02 on the delete path: when the recorded backend is unreachable, the
// DELETE must not be reported as done (which would drop the head row and
// orphan the bytes); it is a retryable 503.
func TestHandleDelete_RecordedBackendUnavailableIs503(t *testing.T) {
	f := setupAdapterFixture(t)

	_, err := f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name)
		VALUES ($1, 'test-bucket', 'unreachable.bin', 4, 'etag', 'application/octet-stream', 'idrive')
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET backend_name = 'idrive'`,
		f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'unreachable.bin'`, f.tenantID)
	})

	f.eng.AddDriver("idrive", &downDriver{name: "idrive"})

	req := httptest.NewRequest("DELETE", "/test-bucket/unreachable.bin", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleDelete(w, req, "test-bucket", "unreachable.bin")

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "30", w.Header().Get("Retry-After"))

	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'unreachable.bin'`, f.tenantID).Scan(&n))
	assert.Equal(t, 1, n, "the head row must survive an unavailable backend — the bytes are still there")
}

// downDriver refuses every operation like a backend whose endpoint is down.
type downDriver struct{ name string }

func (d *downDriver) Name() string { return d.name }
func (d *downDriver) Get(context.Context, string, string) (io.ReadCloser, error) {
	return nil, errConnRefused
}
func (d *downDriver) Put(context.Context, string, string, io.Reader, ...engine.PutOption) error {
	return errConnRefused
}
func (d *downDriver) Delete(context.Context, string, string) error { return errConnRefused }
func (d *downDriver) List(context.Context, string, string) ([]string, error) {
	return nil, errConnRefused
}
func (d *downDriver) Exists(context.Context, string, string) (bool, error) {
	return false, errConnRefused
}
func (d *downDriver) HealthCheck(context.Context) error { return errConnRefused }

var errConnRefused = errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
