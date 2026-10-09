package drivers

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/webdav"
)

// Prompt 2a.2 G3: Sync's five bridges share ONE account. A collection
// DELETE is recursive (RFC 4918 §9.6.1), so a folder removal decided on one
// bridge's view deletes whatever the account holds there — and a bridge
// lags another's writes by seconds to minutes. RemoveEmptyDir used to ask
// every bridge in turn and let each one that saw the folder empty send its
// own DELETE: a lagging bridge wiped a shard another bridge had just written.

// laggingBridges starts n bridges on one shared file system; bridge `stale`
// answers PROPFIND from its own (older) file system — what a bridge that has
// not seen the others' writes yet lists — while its writes and deletes go to
// the shared one (the one account).
func laggingBridges(t *testing.T, n, stale int) (bs []*bridgeServer, shared, old webdav.FileSystem) {
	t.Helper()
	old = webdav.NewMemFS()
	oldHandler := &webdav.Handler{FileSystem: old, LockSystem: webdav.NewMemLS()}
	bs = newBridges(t, n, true, func(i int, h http.Handler) http.Handler {
		if i != stale {
			return h
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "PROPFIND" {
				oldHandler.ServeHTTP(w, r)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	return bs, bs[0].fs, old
}

func fsHas(fs webdav.FileSystem, p string) bool {
	_, err := fs.Stat(context.Background(), p)
	return err == nil
}

func deletes(bs []*bridgeServer) int {
	n := 0
	for _, b := range bs {
		n += b.count(http.MethodDelete, "")
	}
	return n
}

func TestMultiWebDAV_RemoveEmptyDirNeverDeletesWhatAnotherBridgeStillHolds(t *testing.T) {
	// Arrange: the account holds a shard in <digest>/<etag>/ (written through
	// bridge 2); bridge 0 still lists the folder empty.
	bs, shared, old := laggingBridges(t, 5, 0)
	m := newMulti(t, bs, nil)
	putFile(t, shared, "/vaultaire/t-tenant-a/c__parity/d1/e2/p0%o", "shard")
	require.NoError(t, old.Mkdir(context.Background(), "/vaultaire", 0o755))
	for _, p := range []string{"/vaultaire/t-tenant-a", "/vaultaire/t-tenant-a/c__parity", "/vaultaire/t-tenant-a/c__parity/d1", "/vaultaire/t-tenant-a/c__parity/d1/e2"} {
		_ = old.Mkdir(context.Background(), p, 0o755)
	}

	// Act
	err := m.RemoveEmptyDir(davCtx("tenant-a"), "c__parity", "d1/e2")

	// Assert: refused, the shard is still there, nothing was deleted.
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDirNotEmpty), "%v", err)
	assert.True(t, fsHas(shared, "/vaultaire/t-tenant-a/c__parity/d1/e2/p0%o"), "a lagging bridge's view must never cost a shard")
	assert.Equal(t, 0, deletes(bs))
}

func TestMultiWebDAV_RemoveEmptyDirOneDeleteWhenEveryBridgeAgrees(t *testing.T) {
	// Arrange: an empty folder every bridge sees.
	bs := newBridges(t, 5, true, nil)
	m := newMulti(t, bs, nil)
	putFile(t, bs[0].fs, "/vaultaire/t-tenant-a/c__parity/d1/e2/p0%o", "x")
	require.NoError(t, bs[0].fs.RemoveAll(context.Background(), "/vaultaire/t-tenant-a/c__parity/d1/e2/p0%o"))

	// Act
	err := m.RemoveEmptyDir(davCtx("tenant-a"), "c__parity", "d1/e2")

	// Assert: gone, through exactly one DELETE; the parent stays.
	require.NoError(t, err)
	assert.False(t, fsHas(bs[0].fs, "/vaultaire/t-tenant-a/c__parity/d1/e2"))
	assert.True(t, fsHas(bs[0].fs, "/vaultaire/t-tenant-a/c__parity/d1"))
	assert.Equal(t, 1, deletes(bs))
}

func TestMultiWebDAV_RemoveEmptyDirStopsAtTheFirstNotEmpty(t *testing.T) {
	bs := newBridges(t, 5, true, nil)
	m := newMulti(t, bs, nil)
	putFile(t, bs[0].fs, "/vaultaire/t-tenant-a/c__parity/d1/e2/p0%o", "x")
	for _, b := range bs {
		b.reset()
	}
	err := m.RemoveEmptyDir(davCtx("tenant-a"), "c__parity", "d1")
	require.ErrorIs(t, err, ErrDirNotEmpty)
	total := 0
	for _, b := range bs {
		total += b.count("PROPFIND", "")
	}
	assert.Equal(t, 1, total, "one bridge said not empty: no other is asked")
	assert.Equal(t, 0, deletes(bs))
}

func TestMultiWebDAV_RemoveEmptyDirMissingIsFineAndNeverTheContainer(t *testing.T) {
	bs := newBridges(t, 3, true, nil)
	m := newMulti(t, bs, nil)
	require.NoError(t, m.RemoveEmptyDir(davCtx("tenant-a"), "c__parity", "nope/none"))
	assert.Error(t, m.RemoveEmptyDir(davCtx("tenant-a"), "c__parity", ""), "the container itself is never a folder to remove")
	assert.Error(t, m.RemoveEmptyDir(davCtx("tenant-a"), "c__parity", "/"))
}

func TestWebDAV_RemoveEmptyDirMapsNamesLikeFiles(t *testing.T) {
	// A folder segment is davName-mapped exactly like an object's folders
	// (a raw segment addressed another folder, or an invalid path).
	bs := newBridges(t, 1, true, nil)
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")
	require.NoError(t, m.Put(ctx, "c", "a:b/CON/x", strings.NewReader("x")))
	require.NoError(t, m.Delete(ctx, "c", "a:b/CON/x"))
	require.NoError(t, m.RemoveEmptyDir(ctx, "c", "a:b/CON"))
	assert.False(t, fsHas(bs[0].fs, "/vaultaire/t-tenant-a/c/a%3Ab/%43ON"))
	assert.True(t, fsHas(bs[0].fs, "/vaultaire/t-tenant-a/c/a%3Ab"))
}

func TestMultiWebDAV_ListDirNamesSubfoldersAndFiles(t *testing.T) {
	bs := newBridges(t, 3, true, nil)
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")
	putFile(t, bs[0].fs, "/vaultaire/t-tenant-a/c__parity/d1/e1/p0%o", "x")
	require.NoError(t, bs[0].fs.Mkdir(context.Background(), "/vaultaire/t-tenant-a/c__parity/d1/e2", 0o755))
	dirs, files, err := m.ListDir(ctx, "c__parity", "d1")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"e1", "e2"}, dirs)
	assert.Empty(t, files)
	dirs, files, err = m.ListDir(ctx, "c__parity", "d1/e1")
	require.NoError(t, err)
	assert.Empty(t, dirs)
	assert.Equal(t, []string{"p0"}, files)
	dirs, files, err = m.ListDir(ctx, "c__parity", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"d1"}, dirs)
	assert.Empty(t, files)
	dirs, files, err = m.ListDir(ctx, "c__parity", "missing")
	require.NoError(t, err)
	assert.Empty(t, dirs)
	assert.Empty(t, files)
	_ = os.ErrNotExist
}
