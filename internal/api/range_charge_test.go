package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Prompt 2b.5 F3.7: a Range GET of a sync object whose backend could not be
// asked charged the `sync` breaker twice — once for the native range read,
// once more for the whole-object fallback, again against [sync] alone — so
// three ranged requests opened it instead of five (before: open after 3).
// Non-strict backends keep the fallback (TestGetObject_TheSecondRange…).
func TestRangeGet_AStrictBackendThatCannotBeAskedIsChargedOncePerRequest(t *testing.T) {
	f := setupID403(t)
	failing, _ := newForbiddenIDrive(t, "sync")
	f.eng.AddDriver("sync", failing)
	_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, floor)
		VALUES ($1, 'test-bucket', 'held.bin', 29, 'deadbeef', 'application/octet-stream', 'sync', 'standard')`, f.tenantID)
	require.NoError(t, err)
	rng := map[string]string{"Range": "bytes=0-9"}

	for i := 0; i < 4; i++ {
		w := f.do(http.MethodGet, "held.bin", nil, rng)
		require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		assert.Equal(t, "30", w.Header().Get("Retry-After"))
	}
	assert.Equal(t, engine.StateClosed.String(), f.eng.GetFailoverStatus()["sync"], "four requests, four charges")

	w := f.do(http.MethodGet, "held.bin", nil, rng)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, engine.StateOpen.String(), f.eng.GetFailoverStatus()["sync"], "the fifth opens it")
}
