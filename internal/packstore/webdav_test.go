package packstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/net/webdav"
)

// The pack store on the real driver of its first backend: the WebDAV driver
// against an x/net/webdav server (the shape of Sync's bridge). One PUT per
// pack, ranged GETs for members, the layout on the server, and GC over
// PROPFIND listings.
func TestStore_OnTheWebDAVDriver(t *testing.T) {
	// Arrange
	db := openTestDB(t)
	fs := webdav.NewMemFS()
	var puts, gets atomic.Int32
	h := &webdav.Handler{FileSystem: fs, LockSystem: webdav.NewMemLS()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			puts.Add(1)
		case http.MethodGet:
			gets.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	drv, err := drivers.NewWebDAVDriver(uniq("davpack"), srv.URL, "u", "p", "vaultaire", zap.NewNop())
	require.NoError(t, err)
	s := newTestStore(t, db, drv, nil)
	ctx := context.Background()
	tenant := uniq("tenant")
	want := map[string][]byte{}
	w, err := s.NewWriter()
	require.NoError(t, err)

	// Act
	for i := 0; i < 50; i++ {
		k := fmt.Sprintf("p/%02d", i)
		want[k] = randBytes(t, 1000+37*i)
		_, err := w.Add(ctx, tenant, k, int64(len(want[k])), bytes.NewReader(want[k]))
		require.NoError(t, err)
	}
	sp, err := w.Flush(ctx)
	require.NoError(t, err)

	// Assert: one PUT, the file where the layout says
	assert.Equal(t, int32(1), puts.Load())
	path := "/vaultaire/t-_global/_packs/" + sp.Name + drivers.WebDAVLeafMarker
	fi, err := fs.Stat(ctx, path)
	require.NoError(t, err, path)
	assert.Equal(t, sp.Size, fi.Size())
	assert.Less(t, len(path), 200)

	gets.Store(0)
	for k, b := range want {
		got, err := readMember(t, s, tenant, k)
		require.NoError(t, err, k)
		require.Equal(t, b, got, k)
	}
	assert.Equal(t, int32(50), gets.Load(), "one ranged GET per member")

	// tombstone most, GC compacts through the driver
	for i := 0; i < 40; i++ {
		require.NoError(t, s.Delete(ctx, tenant, fmt.Sprintf("p/%02d", i)))
	}
	res, err := s.GC(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Errors)
	assert.Equal(t, 1, res.PacksDeleted)
	_, err = fs.Stat(ctx, path)
	assert.True(t, os.IsNotExist(err), "the old pack is gone from the server")
	for i := 40; i < 50; i++ {
		k := fmt.Sprintf("p/%02d", i)
		got, err := readMember(t, s, tenant, k)
		require.NoError(t, err, k)
		assert.Equal(t, want[k], got, k)
	}
	names, err := drv.List(bctx(ctx), DefaultContainer, "")
	require.NoError(t, err)
	require.Len(t, names, 1)
	assert.True(t, strings.HasSuffix(names[0], ".pack") && validPackName(names[0]))

	// Recover reads a pack back through the driver (its leaf-marked name
	// on the server); every member is already live, so it records none.
	n, err := s.Recover(ctx, names[0])
	require.NoError(t, err)
	assert.Zero(t, n)

	// A pack file no row names is found by the GC listing and deleted.
	stray := strings.Repeat("a", 64)
	strayName := "aa/" + stray + ".pack"
	require.NoError(t, drv.Put(bctx(ctx), DefaultContainer, strayName, strings.NewReader("x"), engine.WithContentLength(1)))
	res, err = s.GC(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.OrphansDeleted)
	_, err = fs.Stat(ctx, "/vaultaire/t-_global/_packs/"+strayName+drivers.WebDAVLeafMarker)
	assert.True(t, os.IsNotExist(err), "the orphan is gone from the server")
}

// A pack is uploaded from its staging file, so a transient answer after the
// body went out is retried by the driver (it rewinds a seekable body) —
// prod's bridges answer a 500 to roughly one PUT in a few hundred, and a
// non-rewindable body turned that into a failed seal (pack-bench on Sync,
// 2026-10-07).
func TestStore_PackUploadIsRetriedAfterATransientAnswer(t *testing.T) {
	// Arrange: the first PUT reads the whole body, then answers 500
	db := openTestDB(t)
	fs := webdav.NewMemFS()
	var puts atomic.Int32
	h := &webdav.Handler{FileSystem: fs, LockSystem: webdav.NewMemLS()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && puts.Add(1) == 1 {
			_, _ = io.Copy(io.Discard, r.Body)
			http.Error(w, "bridge hiccup", http.StatusInternalServerError)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	drv, err := drivers.NewWebDAVDriver(uniq("davretry"), srv.URL, "u", "p", "vaultaire", zap.NewNop(),
		drivers.WithWebDAVRetries(3, time.Millisecond))
	require.NoError(t, err)
	s := newTestStore(t, db, drv, nil)
	ctx := context.Background()
	tenant := uniq("tenant")
	body := randBytes(t, 9<<20) // above the driver's 8 MiB in-memory replay buffer
	w, err := s.NewWriter()
	require.NoError(t, err)
	_, err = w.Add(ctx, tenant, "k", int64(len(body)), bytes.NewReader(body))
	require.NoError(t, err)

	// Act
	sp, err := w.Flush(ctx)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, int32(2), puts.Load())
	got, err := readMember(t, s, tenant, "k")
	require.NoError(t, err)
	assert.Equal(t, body, got)
	fi, err := fs.Stat(ctx, "/vaultaire/t-_global/_packs/"+sp.Name+drivers.WebDAVLeafMarker)
	require.NoError(t, err)
	assert.Equal(t, sp.Size, fi.Size())
}
