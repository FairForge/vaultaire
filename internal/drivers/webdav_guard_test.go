package drivers

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Every request URL is the configured scheme + host with a path under the
// configured root (or the server's own path, for the health check): a path
// that would leave it is refused before any request (CodeQL request-forgery).
func TestWebDAVDriver_RequestURLGuard(t *testing.T) {
	d, err := NewWebDAVDriver("sync", "https://dav.example:8443/remote.php/dav", "u", "p", "vaultaire", zap.NewNop())
	require.NoError(t, err)

	u, err := d.requestURL("/remote.php/dav/vaultaire/t-a/c/k%20ey")
	require.NoError(t, err)
	assert.Equal(t, "https", u.Scheme)
	assert.Equal(t, "dav.example:8443", u.Host)
	assert.Equal(t, "/remote.php/dav/vaultaire/t-a/c/k ey", u.Path)
	assert.Equal(t, "https://dav.example:8443/remote.php/dav/vaultaire/t-a/c/k%20ey", u.String())

	for _, ok := range []string{
		"/remote.php/dav/vaultaire/",
		"/remote.php/dav/vaultaire",
		"/remote.php/dav/", // the health check's fallback: the server's own path
		"/remote.php/dav/vaultaire/t-a/c/dir/",
		"/remote.php/dav/vaultaire/t-a/c/%252e%252e", // a literal "%2e%2e" name
		"/remote.php/dav/vaultaire/t-a/c/a%5Cb",      // a literal backslash
		"/remote.php/dav/vaultaire/t-a/c/.%252E",     // davName("..")
	} {
		_, err := d.requestURL(ok)
		assert.NoError(t, err, ok)
	}
	for _, bad := range []string{
		"",
		"remote.php/dav/vaultaire/x", // not absolute
		"//evil.example/remote.php/dav/vaultaire/x",     // a network path
		"http://evil.example/remote.php/dav/vaultaire/", // an absolute URL
		"/remote.php/dav/vaultaire/../../etc/passwd",
		"/remote.php/dav/vaultaire/%2e%2e/%2e%2e/x",
		"/remote.php/dav/vaultaire/%2E%2E",
		"/remote.php/dav/vaultaire/./x",
		"/remote.php/dav/vaultaire/a//b",
		"/remote.php/dav/vaultaire/..%5C..%5Cx", // ..\..\x on a server that splits on '\'
		"/remote.php/dav/vaultaire/a%5C..%5Cb",
		"/remote.php/dav/other/x",
		"/remote.php/dav/vaultaire-sibling/x",
		"/remote.php/dav/vaultaire/x?q=1",
		"/remote.php/dav/vaultaire/x#frag",
		"/remote.php/dav/vaultaire/x%zz",
		"/remote.php/dav/vaultaire/x%2Fy", // an encoded '/' inside one segment
		"/remote.php/dav/vaultaire/x%00y", // a NUL
	} {
		_, err := d.requestURL(bad)
		require.Error(t, err, bad)
		assert.ErrorIs(t, err, engine.ErrInvalidInput, bad)
	}
}

// Keys built to escape the root never reach the server outside it, and no
// request ever goes to another host.
func TestWebDAVDriver_HostileKeysStayUnderTheRoot(t *testing.T) {
	var foreign atomic.Int32
	var seenHosts sync.Map
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seenHosts.Store(r.Host, struct{}{})
			p := r.URL.Path
			if p != "/" && p != "/vaultaire" && !strings.HasPrefix(p, "/vaultaire/") {
				foreign.Add(1)
			}
			h.ServeHTTP(w, r)
		})
	})
	ctx := davCtx("tenant-a")
	keys := []string{
		"../../../etc/passwd",
		"..%2F..%2Fescape",
		"%2e%2e/%2e%2e/escape",
		`..\..\escape`,
		`a\..\..\b`,
		"//evil.example/x",
		"http://evil.example/x",
		"@evil.example",
		"x?y#z",
	}
	stored := 0
	for _, k := range keys {
		err := f.drv.Put(ctx, "c", k, strings.NewReader(k), engine.WithContentLength(int64(len(k))))
		if err != nil {
			assert.ErrorIs(t, err, engine.ErrInvalidInput, k)
			continue
		}
		stored++
		assert.Equal(t, k, string(readAllClose(t, mustGet(ctx, t, f.drv, "c", k))), k)
	}
	assert.Positive(t, stored)
	_, err := f.drv.List(ctx, "c", "")
	require.NoError(t, err)
	require.NoError(t, f.drv.HealthCheck(ctx))

	assert.Zero(t, foreign.Load(), "a request left the root")
	want := strings.TrimPrefix(f.srv.URL, "http://")
	seenHosts.Range(func(k, _ any) bool {
		assert.Equal(t, want, k)
		return true
	})
	for _, p := range memFiles(t, f.fs, "/") {
		assert.True(t, strings.HasPrefix(p, "/vaultaire/t-tenant-a/c/"), p)
	}
}

// Concurrent PUTs into folders that do not exist yet race on MKCOL of the
// same collection; a server answers the loser 423 Locked (x/net/webdav
// does — the webdav-bench parity suite found it). Every PUT must succeed.
func TestWebDAVDriver_ConcurrentPutsIntoANewFolder(t *testing.T) {
	for round := 0; round < 5; round++ {
		f := newDAVFixture(t, nil)
		ctx := davCtx("tenant-a")
		var wg sync.WaitGroup
		errs := make([]error, 32)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				k := fmt.Sprintf("shared/sub%d/k%d", i%4, i)
				errs[i] = f.drv.Put(ctx, "c", k, strings.NewReader(k), engine.WithContentLength(int64(len(k))))
			}()
		}
		wg.Wait()
		for i, err := range errs {
			assert.NoError(t, err, i)
		}
		assert.Len(t, memFiles(t, f.fs, "/vaultaire/t-tenant-a/c/shared"), len(errs))
	}
}

// A 423 that is not ours (another process holds the folder's lock for a
// moment) is retried.
func TestWebDAVDriver_MkcolLockedIsRetried(t *testing.T) {
	var locked atomic.Int32
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "MKCOL" && locked.Add(1) <= 2 {
				http.Error(w, "Locked", http.StatusLocked)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	ctx := davCtx("tenant-a")
	require.NoError(t, f.drv.Put(ctx, "c", "d/k", strings.NewReader("x"), engine.WithContentLength(1)))
}
