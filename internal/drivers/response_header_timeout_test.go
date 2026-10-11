package drivers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
)

// hangingS3 never answers; it records how long each request was held before
// the client gave it up.
type hangingS3 struct {
	srv   *httptest.Server
	mu    sync.Mutex
	lives []time.Duration
}

func newHangingS3(t *testing.T) *hangingS3 {
	t.Helper()
	h := &hangingS3{}
	release := make(chan struct{})
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		h.mu.Lock()
		h.lives = append(h.lives, time.Since(start))
		h.mu.Unlock()
	}))
	t.Cleanup(func() { close(release); h.srv.Close() })
	return h
}

// assertEachRequestBounded: every attempt was given up near the header
// timeout, whatever the SDK's retry backoff added between them — the bound
// is per request, not on the sum (Prompt 2b.5 F3.10: a 5 s bound on the
// whole call failed at 5.41 s under the full -race run).
func (h *hangingS3) assertEachRequestBounded(ctx context.Context, t *testing.T) {
	t.Helper()
	require.NoError(t, ctx.Err(), "the call ended before the caller's deadline")
	h.mu.Lock()
	defer h.mu.Unlock()
	require.NotEmpty(t, h.lives)
	for i, d := range h.lives {
		assert.Less(t, d, 2*time.Second, "attempt %d held %s", i, d)
	}
}

func shortHeaderTimeout(t *testing.T) {
	old := fixedBucketResponseHeaderTimeout
	fixedBucketResponseHeaderTimeout = 200 * time.Millisecond
	t.Cleanup(func() { fixedBucketResponseHeaderTimeout = old })
}

// A fixed-bucket GET whose response headers never come must fail in bounded
// time (2b.4 E2.1): the client had no ResponseHeaderTimeout and no Timeout,
// so a hung iDrive GET lasted until TCP gave up — and a parfetch part hung
// on it held the whole ordered download.
func TestFixedBucketDriver_GetWithNoResponseHeadersFailsInBoundedTime(t *testing.T) {
	shortHeaderTimeout(t)
	h := newHangingS3(t)
	d, err := NewFixedBucketS3Driver("wasabi", "ak", "sk", h.srv.URL, "us-west-1", "b", zap.NewNop())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(common.WithTenantID(context.Background(), "t1"), 30*time.Second)
	defer cancel()

	_, err = d.Get(ctx, "c", "k")

	require.Error(t, err)
	h.assertEachRequestBounded(ctx, t)
}
