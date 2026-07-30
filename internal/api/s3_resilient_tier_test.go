package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// resilientFixture: chunking fixture + a registered "lyve" driver (local disk
// stand-in — routing is what's under test, not the Lyve wire) + a bucket row
// with tier_preference='resilient'.
func resilientFixture(t *testing.T) (*adapterTestFixture, string) {
	t.Helper()
	f := setupChunkingFixture(t)

	lyveDir, err := os.MkdirTemp("", "vaultaire-lyve-stub-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(lyveDir) })
	f.eng.AddDriver("lyve", drivers.NewLocalDriver(lyveDir, zap.NewNop()))
	require.NoError(t, os.MkdirAll(filepath.Join(lyveDir, f.tenant.NamespaceContainer("test-bucket")), 0750))

	_, err = f.db.Exec(`
		INSERT INTO buckets (tenant_id, name, tier_preference)
		VALUES ($1, 'test-bucket', 'resilient')
		ON CONFLICT (tenant_id, name) DO UPDATE SET tier_preference = 'resilient'`,
		f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1 AND name = 'test-bucket'`, f.tenantID)
	})
	return f, lyveDir
}

func headCacheBackendAndChunked(t *testing.T, f *adapterTestFixture, key string) (string, bool) {
	t.Helper()
	var backend string
	var isChunked bool
	require.NoError(t, f.db.QueryRow(`
		SELECT backend_name, is_chunked FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`,
		f.tenantID, key).Scan(&backend, &isChunked))
	return backend, isChunked
}

// TestHandlePut_ResilientTierRoutesToLyve: a small (plain-path) PUT into a
// resilient-tier bucket must land on the lyve backend.
func TestHandlePut_ResilientTierRoutesToLyve(t *testing.T) {
	f, _ := resilientFixture(t)

	content := generateTestData(512) // below the 1 KB fixture chunk threshold
	req := httptest.NewRequest("PUT", "/test-bucket/small.bin", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "small.bin")
	require.Equal(t, http.StatusOK, w.Code)

	backend, chunked := headCacheBackendAndChunked(t, f, "small.bin")
	assert.Equal(t, "lyve", backend, "resilient tier must route the plain path to lyve")
	assert.False(t, chunked)
}

// TestHandlePut_ResilientTierSkipsChunking: above-threshold objects must take
// the PLAIN path on a resilient bucket — chunk blobs always land on the
// primary backend, which would silently defeat the tier's placement promise.
func TestHandlePut_ResilientTierSkipsChunking(t *testing.T) {
	f, _ := resilientFixture(t)

	content := generateTestData(8 * 1024) // above the 1 KB fixture threshold
	req := httptest.NewRequest("PUT", "/test-bucket/large.bin", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "large.bin")
	require.Equal(t, http.StatusOK, w.Code)

	backend, chunked := headCacheBackendAndChunked(t, f, "large.bin")
	assert.False(t, chunked, "resilient-tier objects must not chunk")
	assert.Equal(t, "lyve", backend)

	// And the object must round-trip.
	getReq := httptest.NewRequest("GET", "/test-bucket/large.bin", nil)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "large.bin")
	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, content, gw.Body.Bytes())
}

// TestMgmtSetBucketTier_Resilient: the management API accepts the new tier.
func TestMgmtSetBucketTier_Resilient(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()

	mock.ExpectExec(`UPDATE buckets SET tier_preference`).
		WithArgs("resilient", "test-tenant", "my-bucket").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := bytes.NewBufferString(`{"tier":"resilient"}`)
	req := httptest.NewRequest("PUT", "/api/v1/manage/buckets/my-bucket/tier", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}
