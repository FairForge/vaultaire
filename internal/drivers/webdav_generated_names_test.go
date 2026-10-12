package drivers

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// A bridge's 400 on a name the DRIVER generated (a stripe piece, a parity
// shard, a pack) is not the caller's key: the first 1 GiB complete on the
// Sync tier answered the customer 400 "check your key" for a transient
// refusal of a stripe piece, with no log line (bench 2026-10-12 finding 2).
// Such a refusal is retried like a 5xx and, left after the retries, is the
// bridge in trouble — never engine.ErrInvalidInput.

var stripePieceNames = []string{"t-tenant-a", "tenant-a_c%p", "ab", "abcdefabcdefabcdefabcdefabcdef12-20261012T150000Z", "p00000%o"}

func TestGeneratedNames(t *testing.T) {
	for _, tc := range []struct {
		names []string
		want  bool
	}{
		{stripePieceNames, true},
		{[]string{"t-tenant-a", "tenant-a_c%p", "ab", "abcdefabcdefabcdefabcdefabcdef12-20261012T150000Z", "key%o"}, true},
		{[]string{"t-tenant-a", "tenant-a__parity", "0123456789abcdef01234567", "etag-4", "p0"}, true},
		{[]string{"t-_global", "_packs", "ab", "ab" + strings.Repeat("0", 62) + ".pack"}, true},
		{[]string{"t-tenant-a", "tenant-a_c", "photo.jpg%o"}, false},
		{[]string{"t-tenant-a", "tenant-a_c", "big.bin%s"}, false}, // the manifest's leaf is the caller's key
		{[]string{"t-tenant-a", "tenant-a_c%25p", "x%o"}, false},   // a customer bucket named `c%p` maps to `c%25p`
		{[]string{"t-tenant-a", "_packs", "ab", "x.pack"}, false},
		{[]string{"t-tenant-a"}, false},
	} {
		assert.Equal(t, tc.want, generatedNames(tc.names), strings.Join(tc.names, "/"))
	}
}

// refuseOnce answers 400 to the first request of each listed method whose
// path matches, then passes the rest through.
func refuseOnce(match string, methods ...string) (func(http.Handler) http.Handler, *atomic.Int32) {
	var refused atomic.Int32
	seen := map[string]*atomic.Bool{}
	for _, m := range methods {
		seen[m] = &atomic.Bool{}
	}
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s, ok := seen[r.Method]; ok && strings.Contains(r.URL.Path, match) && s.CompareAndSwap(false, true) {
				refused.Add(1)
				http.Error(w, "bad name", http.StatusBadRequest)
				return
			}
			h.ServeHTTP(w, r)
		})
	}, &refused
}

func TestWebDAVDriver_RefusedGeneratedNameIsRetried(t *testing.T) {
	ctx := davCtx("tenant-a")
	wrap, refused := refuseOnce("p00000", http.MethodPut)
	f := newDAVFixture(t, wrap, WithWebDAVRetries(3, 0))
	body := bytes.Repeat([]byte("x"), 1024)

	err := f.drv.putNames(ctx, "k (piece 0)", stripePieceNames, bytes.NewReader(body), engine.WithContentLength(int64(len(body))))

	require.NoError(t, err, "the 400 on a generated name is retried like a 5xx")
	assert.Equal(t, int32(1), refused.Load())
	assert.Equal(t, int32(2), f.methods.put.Load(), "one refusal, one success")
}

func TestWebDAVDriver_RefusedGeneratedFolderIsRetried(t *testing.T) {
	ctx := davCtx("tenant-a")
	wrap, refused := refuseOnce("-20261012T150000Z", "MKCOL")
	f := newDAVFixture(t, wrap, WithWebDAVRetries(3, 0))

	err := f.drv.putNames(ctx, "k (piece 0)", stripePieceNames, bytes.NewReader([]byte("x")), engine.WithContentLength(1))

	require.NoError(t, err, "the MKCOL 400 on a generated folder is retried")
	assert.Equal(t, int32(1), refused.Load())
}

func TestWebDAVDriver_RefusedGeneratedNameLeftAfterRetriesIsNotInvalidInput(t *testing.T) {
	ctx := davCtx("tenant-a")
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "p00000") {
				http.Error(w, "bad name", http.StatusBadRequest)
				return
			}
			h.ServeHTTP(w, r)
		})
	}, WithWebDAVRetries(2, 0))

	err := f.drv.putNames(ctx, "k (piece 0)", stripePieceNames, bytes.NewReader([]byte("x")), engine.WithContentLength(1))

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrWebDAVRefusedGenerated)
	assert.False(t, errors.Is(err, engine.ErrInvalidInput), "never the caller's key: %v", err)
	assert.True(t, bridgeUnavailable(err), "the bridge is in trouble: %v", err)
	assert.Equal(t, int32(2), f.methods.put.Load(), "every attempt used")

	// The caller's own key, refused the same way, stays the caller's error.
	err = f.drv.Put(ctx, "c", "p00000", bytes.NewReader([]byte("x")), engine.WithContentLength(1))
	require.Error(t, err)
	assert.ErrorIs(t, err, engine.ErrInvalidInput)
	assert.False(t, errors.Is(err, ErrWebDAVRefusedGenerated))
}

// Through the multi-bridge driver a persistent refusal of a generated name
// on one bridge is that bridge out for this object while another is healthy:
// engine.ErrPartiallyUnavailable — the S3 API's 503 + Retry-After, never 400.
func TestMultiWebDAVDriver_RefusedGeneratedNameIs503(t *testing.T) {
	refuseParity := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "__parity") {
				http.Error(w, "bad name", http.StatusBadRequest)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	f1 := newDAVFixture(t, refuseParity)
	f2 := newDAVFixture(t, refuseParity)
	m, err := NewMultiWebDAVDriver("sync", WebDAVConfig{
		Bridges: []WebDAVBridge{{URL: f1.srv.URL, Password: davTestPass}, {URL: f2.srv.URL, Password: davTestPass}},
		User:    davTestUser, Root: "vaultaire",
	}, zap.NewNop(), WithWebDAVRetries(2, 0))
	require.NoError(t, err)
	ctx := davCtx("tenant-a")

	err = m.Put(ctx, "tenant-a__parity", "0123456789abcdef01234567/etag-4/p0", bytes.NewReader([]byte("x")), engine.WithContentLength(1))

	require.Error(t, err)
	assert.ErrorIs(t, err, engine.ErrPartiallyUnavailable, "%v", err)
	assert.False(t, errors.Is(err, engine.ErrInvalidInput), "%v", err)
}
