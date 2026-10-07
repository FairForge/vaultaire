package drivers

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/webdav"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Reads of a striped object pay a manifest GET (~0.7 s on Sync) before the
// first piece unless the manifest is cached. These tests count the requests
// the bridges see.

func countAll(bs []*bridgeServer, method, part string) int {
	n := 0
	for _, b := range bs {
		n += b.count(method, part)
	}
	return n
}

func resetAll(bs []*bridgeServer) {
	for _, b := range bs {
		b.reset()
	}
}

func TestStripeCache_ReadsAfterPutNeedNoManifestOrPlainGET(t *testing.T) {
	// Arrange
	bs := newBridges(t, 3, true, nil)
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	body := randBody(t, 3*testPiece+5)
	putBytes(ctx, t, m, "bucket", "k", body)
	resetAll(bs)

	// Act
	got1 := readAllClose(t, mustGetM(ctx, t, m, "bucket", "k"))
	rc, err := m.GetRange(ctx, "bucket", "k", testPiece-1, 2)
	require.NoError(t, err)
	got2 := readAllClose(t, rc)

	// Assert
	assert.Equal(t, body, got1)
	assert.Equal(t, body[testPiece-1:testPiece+1], got2)
	assert.Zero(t, countAll(bs, "GET", "k%s"), "the manifest came from the cache")
	assert.Zero(t, countAll(bs, "GET", "k%o"), "no plain-file probe for a cached striped object")
}

// Another app slot (another driver on the same bridges): its first read
// fetches the manifest once, the next ones none.
func TestStripeCache_ReadPopulatesTheCache(t *testing.T) {
	bs := newBridges(t, 3, true, nil)
	writer := newStriped(t, bs, nil)
	reader := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	body := randBody(t, 3*testPiece)
	putBytes(ctx, t, writer, "bucket", "k", body)
	resetAll(bs)

	assert.Equal(t, body, readAllClose(t, mustGetM(ctx, t, reader, "bucket", "k")))
	assert.Equal(t, 1, countAll(bs, "GET", "k%s"))
	assert.Equal(t, body, readAllClose(t, mustGetM(ctx, t, reader, "bucket", "k")))
	assert.Equal(t, 1, countAll(bs, "GET", "k%s"), "the second read used the cache")
}

// A cached manifest whose generation another writer replaced: the read
// sees the piece gone, reads the manifest again, serves the NEW bytes and
// caches the new manifest.
func TestStripeCache_ForeignOverwriteServesTheNewBytes(t *testing.T) {
	bs := newBridges(t, 3, true, nil)
	reader := newStriped(t, bs, nil)
	other := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	v1, v2 := randBody(t, 3*testPiece), randBody(t, 4*testPiece+3)
	putBytes(ctx, t, reader, "bucket", "k", v1)
	require.Equal(t, v1, readAllClose(t, mustGetM(ctx, t, reader, "bucket", "k")))

	putBytes(ctx, t, other, "bucket", "k", v2) // deletes v1's pieces
	resetAll(bs)

	assert.Equal(t, v2, readAllClose(t, mustGetM(ctx, t, reader, "bucket", "k")))
	n := countAll(bs, "GET", "k%s")
	assert.Equal(t, v2, readAllClose(t, mustGetM(ctx, t, reader, "bucket", "k")))
	assert.Equal(t, n, countAll(bs, "GET", "k%s"), "the refreshed manifest is cached")

	rc, err := reader.GetRange(ctx, "bucket", "k", 4*testPiece, 3)
	require.NoError(t, err)
	assert.Equal(t, v2[4*testPiece:], readAllClose(t, rc))
}

func TestStripeCache_ForeignOverwriteToPlainAndForeignDelete(t *testing.T) {
	bs := newBridges(t, 3, true, nil)
	reader := newStriped(t, bs, nil)
	other := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	putBytes(ctx, t, reader, "bucket", "k", randBody(t, 3*testPiece))
	putBytes(ctx, t, reader, "bucket", "gone", randBody(t, 3*testPiece))

	small := randBody(t, 50)
	putBytes(ctx, t, other, "bucket", "k", small)
	require.NoError(t, other.Delete(ctx, "bucket", "gone"))

	assert.Equal(t, small, readAllClose(t, mustGetM(ctx, t, reader, "bucket", "k")))
	_, err := reader.Get(ctx, "bucket", "gone")
	var nf engine.NotFoundError
	assert.True(t, errors.As(err, &nf), "a deleted object is a miss, not a 503: %v", err)
}

// This process's Put and Delete invalidate the entry.
func TestStripeCache_OwnWritesInvalidate(t *testing.T) {
	bs := newBridges(t, 3, true, nil)
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	putBytes(ctx, t, m, "bucket", "k", randBody(t, 3*testPiece))

	small := randBody(t, 9)
	putBytes(ctx, t, m, "bucket", "k", small)
	assert.Equal(t, small, readAllClose(t, mustGetM(ctx, t, m, "bucket", "k")))

	putBytes(ctx, t, m, "bucket", "k", randBody(t, 2*testPiece))
	require.NoError(t, m.Delete(ctx, "bucket", "k"))
	_, err := m.Get(ctx, "bucket", "k")
	assert.True(t, notFoundErr(err), "%v", err)
}

func TestManifestCache_LRUAndTTL(t *testing.T) {
	now := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	c := newManifestCache(2, time.Minute, func() time.Time { return now })
	a, b, d := &stripeManifest{Gen: "a"}, &stripeManifest{Gen: "b"}, &stripeManifest{Gen: "d"}
	c.put("a", a)
	c.put("b", b)
	_, ok := c.get("a") // a is now the most recent
	require.True(t, ok)
	c.put("d", d) // evicts b
	_, ok = c.get("b")
	assert.False(t, ok, "least recently used evicted")
	got, ok := c.get("a")
	assert.True(t, ok)
	assert.Same(t, a, got)
	c.drop("a")
	_, ok = c.get("a")
	assert.False(t, ok)
	now = now.Add(61 * time.Second)
	_, ok = c.get("d")
	assert.False(t, ok, "expired")
	assert.Zero(t, c.len())
}

// A range across a piece boundary opens both pieces at once, and a whole
// read starts the next piece while the first one is being opened.
func TestStripe_PiecesOfARangeOpenInParallel(t *testing.T) {
	var inflight, peak atomic.Int32
	bs := newBridges(t, 3, true, func(_ int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/p0") {
				n := inflight.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				time.Sleep(60 * time.Millisecond)
				inflight.Add(-1)
			}
			h.ServeHTTP(w, r)
		})
	})
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	body := randBody(t, 3*testPiece)
	putBytes(ctx, t, m, "bucket", "k", body)

	rc, err := m.GetRange(ctx, "bucket", "k", testPiece-1, 2)
	require.NoError(t, err)
	assert.Equal(t, body[testPiece-1:testPiece+1], readAllClose(t, rc))
	assert.Equal(t, int32(2), peak.Load(), "both pieces requested at once")
}

// fsDirs lists every folder under root of a memfs.
func fsDirs(t *testing.T, fs webdav.FileSystem, root string) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	var walk func(p string)
	walk = func(p string) {
		f, err := fs.OpenFile(ctx, p, os.O_RDONLY, 0)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		require.NoError(t, err)
		infos, err := f.Readdir(-1)
		_ = f.Close()
		require.NoError(t, err)
		for _, fi := range infos {
			if fi.IsDir() {
				q := p + "/" + fi.Name()
				out = append(out, q)
				walk(q)
			}
		}
	}
	walk(root)
	sort.Strings(out)
	return out
}

// Overwrites and deletes leave no folder of the key's pieces behind: the
// `<hh>` folders hold ≤ 50,000 entries on Sync and would fill one per key.
func TestStripe_DeleteAndOverwriteLeaveNoEmptyFolders(t *testing.T) {
	bs := newBridges(t, 3, true, nil)
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	putBytes(ctx, t, m, "bucket", "a", randBody(t, 3*testPiece))
	putBytes(ctx, t, m, "bucket", "a", randBody(t, 2*testPiece)) // striped → striped
	putBytes(ctx, t, m, "bucket", "b", randBody(t, 2*testPiece))
	putBytes(ctx, t, m, "bucket", "b", randBody(t, 7)) // striped → plain
	putBytes(ctx, t, m, "bucket", "c", randBody(t, 2*testPiece))
	require.NoError(t, m.Delete(ctx, "bucket", "c"))

	var deep []string
	for _, d := range fsDirs(t, bs[0].fs, "/vaultaire/t-tenant-a/bucket%p") {
		if strings.Count(strings.TrimPrefix(d, "/vaultaire/t-tenant-a/bucket%p/"), "/") >= 1 {
			deep = append(deep, d)
		}
	}
	assert.Len(t, deep, 1, "only the live generation of `a` has a folder below <hh>: %v", deep)
}

// The reaper removes empty folders past the grace — the generation folders
// of the current layout by their name's time, and legacy (#626) key folders
// `<hh>/<h>/` once nothing is left in them — and keeps young ones.
func TestStripe_ReaperRemovesEmptyFolders(t *testing.T) {
	// Arrange
	bs := newBridges(t, 2, true, nil)
	m := newStriped(t, bs, nil)
	t0 := time.Date(2026, 10, 7, 6, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return t0.Add(3 * time.Hour) }
	ctx := context.Background()
	fs := bs[0].fs
	root := "/vaultaire/t-tenant-a/bucket%p"
	mk := func(p string) {
		segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
		cur := ""
		for _, s := range segs {
			cur += "/" + s
			if err := fs.Mkdir(ctx, cur, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				require.NoError(t, err)
			}
		}
	}
	h := stripeKeyHash("x")
	emptyOld := root + "/" + h[:2] + "/" + h + "-" + newStripeGen(t0)
	emptyYoung := root + "/" + h[:2] + "/" + h + "-" + newStripeGen(t0.Add(150*time.Minute))
	legacyEmpty := root + "/" + h[:2] + "/" + h
	legacyOldGen := root + "/" + h[:2] + "/" + strings.Repeat("e", 32) + "/" + newStripeGen(t0)
	for _, p := range []string{emptyOld, emptyYoung, legacyEmpty, legacyOldGen} {
		mk(p)
	}

	// Act
	res, err := m.ReapOrphanStripes(ctx, time.Hour)

	// Assert
	require.NoError(t, err)
	dirs := fsDirs(t, fs, root)
	assert.NotContains(t, dirs, emptyOld)
	assert.Contains(t, dirs, emptyYoung)
	assert.NotContains(t, dirs, legacyEmpty)
	assert.NotContains(t, dirs, legacyOldGen)
	assert.NotContains(t, dirs, root+"/"+h[:2]+"/"+strings.Repeat("e", 32), "a legacy key folder emptied by the reaper goes too")
	assert.Positive(t, res.FoldersRemoved)
}

// A manifest written by #626 (generation folder under a key folder) still
// reads.
func TestStripe_LegacyLayoutManifestValidates(t *testing.T) {
	names := []string{"t-tenant-a", "bucket", "k%o"}
	gen := newStripeGen(time.Now())
	h := stripeKeyHash("k")
	legacy := &stripeManifest{Format: stripeManifestFormat, Size: 10, PieceSize: 16, Gen: gen,
		Dir: []string{"t-tenant-a", "bucket%p", h[:2], h, gen}, Pieces: []stripePiece{{Size: 10}}}
	require.NoError(t, legacy.validate(names))
	cur := *legacy
	cur.Dir = stripeDir("t-tenant-a", "bucket", "k", gen)
	require.NoError(t, cur.validate(names))
	assert.Len(t, cur.Dir, 4)
	bad := *legacy
	bad.Dir = []string{"t-tenant-a", "bucket%p", h[:2], h + "-" + newStripeGen(time.Now().Add(time.Hour))}
	assert.Error(t, bad.validate(names), "a folder of another generation")
}
