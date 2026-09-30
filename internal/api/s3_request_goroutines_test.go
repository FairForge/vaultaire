package api

import (
	"bytes"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Review R15: handleS3Request used to construct a fresh events.EventLogger per
// authenticated request. Its constructor started a goroutine ranging over a
// 1000-slot channel that nothing ever closed, so every S3 request leaked one
// goroutine and its buffer for the life of the process (R1's goroutine table
// only covered the background loops). The logger duplicated what
// loggingMiddleware already records and was deleted with internal/events.
// This pins the invariant: N requests must not grow the goroutine count by N.
func TestHandleS3Request_DoesNotLeakGoroutinesPerRequest(t *testing.T) {
	logger := zap.NewNop()
	eng := engine.NewEngine(nil, logger, nil)
	tempDir, err := os.MkdirTemp("", "vaultaire-leak-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })
	eng.AddDriver("local", drivers.NewLocalDriver(tempDir, logger))
	eng.SetPrimary("local")

	server := &Server{logger: logger, router: chi.NewRouter(), engine: eng, testMode: true}
	tn := &tenant.Tenant{ID: "leak-tenant", Namespace: "tenant/leak-tenant/"}
	do := func(method, path string, body []byte) int {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req = req.WithContext(tenant.WithTenant(req.Context(), tn))
		w := httptest.NewRecorder()
		server.handleS3Request(w, req)
		return w.Code
	}

	// Warm up: first-request allocations (driver pools, lazy inits) are not the leak.
	require.Equal(t, 200, do("PUT", "/leak-bucket/warm.txt", []byte("x")))
	runtime.GC()
	before := runtime.NumGoroutine()

	const n = 200
	for i := 0; i < n; i++ {
		require.Equal(t, 200, do("PUT", "/leak-bucket/k.txt", []byte("hello")))
		require.Equal(t, 200, do("GET", "/leak-bucket/k.txt", nil))
	}

	// Give any per-request goroutine that legitimately finishes a moment to exit.
	deadline := time.Now().Add(2 * time.Second)
	var after int
	for {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after-before < 20 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Lessf(t, after-before, 20,
		"%d S3 requests grew the goroutine count from %d to %d — a per-request goroutine is being leaked", 2*n, before, after)
}
