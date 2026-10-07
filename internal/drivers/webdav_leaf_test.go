package drivers

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/webdav"
)

// memTree reads every folder and file of an in-memory file system.
func memTree(t *testing.T, fs webdav.FileSystem) (dirs []string, files map[string][]byte) {
	t.Helper()
	ctx := context.Background()
	files = map[string][]byte{}
	var walk func(dir string)
	walk = func(dir string) {
		f, err := fs.OpenFile(ctx, dir, os.O_RDONLY, 0)
		require.NoError(t, err)
		infos, err := f.Readdir(-1)
		mustClose(f)
		require.NoError(t, err)
		for _, fi := range infos {
			p := strings.TrimSuffix(dir, "/") + "/" + fi.Name()
			if fi.IsDir() {
				dirs = append(dirs, p)
				walk(p)
				continue
			}
			rf, err := fs.OpenFile(ctx, p, os.O_RDONLY, 0)
			require.NoError(t, err)
			b, err := io.ReadAll(rf)
			mustClose(rf)
			require.NoError(t, err)
			files[p] = b
		}
	}
	walk("/")
	return dirs, files
}

// cloudSync merges what every bridge holds into every bridge, the way
// Sync's cloud reconciles devices that could not see each other's writes
// (cross-bridge staleness 1.5 s – 5 min): a folder wins over a file of the
// same name — the file is lost (live on prod 2026-10-07: PUT `x`, PUT `x/y`
// on another bridge, GET `x` → gone).
func cloudSync(t *testing.T, bs []*bridgeServer) {
	t.Helper()
	ctx := context.Background()
	dirSet := map[string]bool{}
	files := map[string][]byte{}
	for _, b := range bs {
		dirs, fl := memTree(t, b.fs)
		for _, d := range dirs {
			dirSet[d] = true
		}
		for p, c := range fl {
			files[p] = c
		}
	}
	for p := range files {
		if dirSet[p] {
			delete(files, p)
		}
	}
	dirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, b := range bs {
		for _, d := range dirs {
			if fi, err := b.fs.Stat(ctx, d); err == nil && !fi.IsDir() {
				require.NoError(t, b.fs.RemoveAll(ctx, d))
			}
			if err := b.fs.Mkdir(ctx, d, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				require.NoError(t, err)
			}
		}
		for p, c := range files {
			if _, err := b.fs.Stat(ctx, path.Dir(p)); err != nil {
				continue
			}
			f, err := b.fs.OpenFile(ctx, p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			require.NoError(t, err)
			_, err = f.Write(c)
			require.NoError(t, err)
			mustClose(f)
		}
	}
}

// S3 lets `x` and `x/y` both exist. On the bridges they must never be the
// same name: `x` routed to one bridge, `x/y` to another that has not seen
// `x` yet, and Sync's cloud would keep the folder and lose the file. Object
// leaves carry a marker no folder name can, so every bridge can hold both.
func TestMultiWebDAV_FileAndFolderOfTheSameNameSurviveCrossBridgeSync(t *testing.T) {
	bs := newBridges(t, 2, false, nil)
	m := newMulti(t, bs, nil)
	ctx := davCtx("tenant-a")
	x := keyOnBridge(ctx, t, m, "c", "x", 0)
	xy := keyOnBridge(ctx, t, m, "c", x+"/y", 1)
	require.NotEqual(t, x, xy)

	require.NoError(t, m.Put(ctx, "c", x, strings.NewReader("file x"), engine.WithContentLength(6)))
	require.NoError(t, m.Put(ctx, "c", xy, strings.NewReader("file x/y"), engine.WithContentLength(8)))
	cloudSync(t, bs)

	assert.Equal(t, "file x", string(readAllClose(t, mustMultiGet(ctx, t, m, "c", x))))
	assert.Equal(t, "file x/y", string(readAllClose(t, mustMultiGet(ctx, t, m, "c", xy))))
	for i, b := range m.bridges {
		listed, err := b.drv.List(ctx, "c", "")
		require.NoError(t, err, "bridge %d", i)
		assert.Equal(t, []string{x, xy}, listed, "bridge %d", i)
	}
}

// One driver, one server: `x` then `x/y`, and `x/` (a folder marker) with
// `x` — every combination S3 allows is stored and read back.
func TestWebDAVDriver_FileAndFolderOfTheSameName(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := davCtx("tenant-a")
	keys := []string{"x", "x/y", "x/", "x/y/z", "a/b", "a"}
	for _, k := range keys {
		require.NoError(t, f.drv.Put(ctx, "c", k, strings.NewReader("v:"+k), engine.WithContentLength(int64(len("v:"+k)))), k)
	}
	for _, k := range keys {
		assert.Equal(t, "v:"+k, string(readAllClose(t, mustGet(ctx, t, f.drv, "c", k))), k)
		ok, err := f.drv.Exists(ctx, "c", k)
		require.NoError(t, err)
		assert.True(t, ok, k)
	}
	listed, err := f.drv.List(ctx, "c", "")
	require.NoError(t, err)
	want := append([]string(nil), keys...)
	sort.Strings(want)
	assert.Equal(t, want, listed)

	// Deleting `x` leaves `x/y` (and the folder) alone.
	require.NoError(t, f.drv.Delete(ctx, "c", "x"))
	ok, err := f.drv.Exists(ctx, "c", "x")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, "v:x/y", string(readAllClose(t, mustGet(ctx, t, f.drv, "c", "x/y"))))
}

// The layout on the server: folders plain, every object leaf `<name>%o`.
func TestWebDAVDriver_LeafMarkerLayout(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := davCtx("tenant-a")
	for _, k := range []string{"photos/cat.jpg", "CON", "photos/"} {
		require.NoError(t, f.drv.Put(ctx, "c", k, strings.NewReader("v"), engine.WithContentLength(1)))
	}
	got := memFiles(t, f.fs, "/vaultaire")
	sort.Strings(got)
	assert.Equal(t, []string{
		"/vaultaire/t-tenant-a/c/%43ON%o",
		"/vaultaire/t-tenant-a/c/photos/%%o",
		"/vaultaire/t-tenant-a/c/photos/cat.jpg%o",
	}, got)
}

// A file without the marker (written before the marker, or by something
// else) is no object: List skips it, Get does not see it; the erasure walk
// still finds it, so nothing of a tenant survives an erasure.
func TestWebDAVDriver_UnmarkedFilesAreNotObjects(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := davCtx("tenant-a")
	putFile(t, f.fs, "/vaultaire/t-tenant-a/c/legacy", "old")
	require.NoError(t, f.drv.Put(ctx, "c", "new", strings.NewReader("v"), engine.WithContentLength(1)))

	listed, err := f.drv.List(ctx, "c", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"new"}, listed)
	ok, err := f.drv.Exists(ctx, "c", "legacy")
	require.NoError(t, err)
	assert.False(t, ok)

	var walked []string
	require.NoError(t, f.drv.WalkTenant(ctx, "tenant-a", func(o engine.TenantObject) error {
		walked = append(walked, o.Container+"|"+o.Artifact)
		return o.Remove(ctx)
	}))
	sort.Strings(walked)
	assert.Equal(t, []string{"c|legacy", "c|new"}, walked)
	assert.Empty(t, memFiles(t, f.fs, "/vaultaire"))
}

// The 248-character cap counts the marker: a leaf of 246 characters is the
// longest storable one; a folder keeps the full 248.
func TestWebDAVDriver_CapCountsTheLeafMarker(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := davCtx("tenant-a")
	require.NoError(t, f.drv.Put(ctx, "c", strings.Repeat("L", 246), strings.NewReader("v"), engine.WithContentLength(1)))
	require.NoError(t, f.drv.Put(ctx, "c", strings.Repeat("D", 248)+"/f", strings.NewReader("v"), engine.WithContentLength(1)))
	err := f.drv.Put(ctx, "c", strings.Repeat("L", 247), strings.NewReader("v"), engine.WithContentLength(1))
	require.Error(t, err)
	assert.ErrorIs(t, err, engine.ErrInvalidInput)
}

// A key too long to have been stored can never exist: reads answer
// not-found, Exists false, Delete succeeds — never an error (the S3 layer
// turned it into a 500). Only PUT refuses it as invalid input.
func TestWebDAVDriver_UnstorableKeyReadsAsAMiss(t *testing.T) {
	long := strings.Repeat("L", 300)
	ctx := davCtx("tenant-a")
	f := newDAVFixture(t, nil)
	bs := newBridges(t, 2, false, nil)
	m := newMulti(t, bs, nil)
	for name, d := range map[string]interface {
		engine.Driver
		GetRange(context.Context, string, string, int64, int64) (io.ReadCloser, error)
	}{"single": f.drv, "multi": m} {
		_, err := d.Get(ctx, "c", long)
		var nf engine.NotFoundError
		assert.True(t, errors.As(err, &nf), "%s get: %v", name, err)
		_, err = d.GetRange(ctx, "c", long, 0, 1)
		assert.True(t, errors.As(err, &nf), "%s range: %v", name, err)
		ok, err := d.Exists(ctx, "c", long)
		assert.NoError(t, err, name)
		assert.False(t, ok, name)
		assert.NoError(t, d.Delete(ctx, "c", long), name)
		err = d.Put(ctx, "c", long, strings.NewReader("v"), engine.WithContentLength(1))
		assert.ErrorIs(t, err, engine.ErrInvalidInput, name)
	}
}
