package drivers

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
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

// Striping: a known-length Put ≥ the stripe minimum becomes pieces routed
// by their own names + a manifest at the key's leaf. Small sizes here: a
// 16 KiB piece, a 32 KiB minimum.

const (
	testPiece     = 16 << 10
	testStripeMin = 32 << 10
)

func newStriped(t *testing.T, bs []*bridgeServer, mutate func(*WebDAVConfig), opts ...WebDAVOption) *MultiWebDAVDriver {
	t.Helper()
	dir := t.TempDir()
	return newMulti(t, bs, func(c *WebDAVConfig) {
		c.StripeMin, c.StripePiece, c.StagingDir = testStripeMin, testPiece, dir
		if mutate != nil {
			mutate(c)
		}
	}, opts...)
}

func randBody(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func putBytes(ctx context.Context, t *testing.T, m *MultiWebDAVDriver, c, a string, b []byte) {
	t.Helper()
	require.NoError(t, m.Put(ctx, c, a, onlyReader{bytes.NewReader(b)}, engine.WithContentLength(int64(len(b)))))
}

// fsFiles lists every file under root of a memfs (decoded resource paths).
func fsFiles(t *testing.T, fs webdav.FileSystem, root string) []string {
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
			q := p + "/" + fi.Name()
			if fi.IsDir() {
				walk(q)
			} else {
				out = append(out, q)
			}
		}
	}
	walk(root)
	sort.Strings(out)
	return out
}

func allFiles(t *testing.T, bs []*bridgeServer) []string {
	var out []string
	for _, b := range bs {
		out = append(out, fsFiles(t, b.fs, "/vaultaire")...)
	}
	sort.Strings(out)
	return out
}

func filesWith(files []string, part string) []string {
	var out []string
	for _, f := range files {
		if strings.Contains(f, part) {
			out = append(out, f)
		}
	}
	return out
}

// Every bridge on its OWN file system: nothing is shared, so a striped
// object reads back only if each piece is read from the bridge that wrote
// it and the manifest from the key's bridge (cross-bridge staleness never
// causes a false miss).
func TestStripe_RoundTripAndRangesAcrossBridges(t *testing.T) {
	// Arrange
	bs := newBridges(t, 5, false, nil)
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	body := randBody(t, 7*testPiece+123) // 8 pieces, the last short

	// Act
	putBytes(ctx, t, m, "bucket", "media/big.bin", body)

	// Assert: byte-exact, and the pieces are spread
	assert.Equal(t, body, readAllClose(t, mustGetM(ctx, t, m, "bucket", "media/big.bin")))
	used := 0
	pieces := 0
	for _, b := range bs {
		n := len(filesWith(fsFiles(t, b.fs, "/vaultaire"), "/p0"))
		pieces += n
		if n > 0 {
			used++
		}
	}
	assert.Equal(t, 8, pieces)
	assert.GreaterOrEqual(t, used, 2, "pieces spread over the bridges")
	assert.Empty(t, filesWith(allFiles(t, bs), "big.bin%o"), "no plain file for a striped object")
	assert.Len(t, filesWith(allFiles(t, bs), "big.bin%s"), 1, "one manifest")

	for _, c := range []struct{ off, n int64 }{
		{0, 10}, {testPiece - 5, 10}, {testPiece, testPiece}, {3*testPiece - 7, 2*testPiece + 14},
		{7 * testPiece, 123}, {7*testPiece + 100, 0}, {int64(len(body)) - 1, 0}, {5, 0},
	} {
		rc, err := m.GetRange(ctx, "bucket", "media/big.bin", c.off, c.n)
		require.NoError(t, err, c)
		want := body[c.off:]
		if c.n > 0 && c.off+c.n < int64(len(body)) {
			want = body[c.off : c.off+c.n]
		}
		assert.Equal(t, want, readAllClose(t, rc), "range %+v", c)
	}
	rc, err := m.GetRange(ctx, "bucket", "media/big.bin", int64(len(body))+10, 5)
	require.NoError(t, err)
	assert.Empty(t, readAllClose(t, rc), "a range past the end is empty")

	ok, err := m.Exists(ctx, "bucket", "media/big.bin")
	require.NoError(t, err)
	assert.True(t, ok)
}

func mustGetM(ctx context.Context, t *testing.T, m *MultiWebDAVDriver, c, a string) io.ReadCloser {
	t.Helper()
	rc, err := m.Get(ctx, c, a)
	require.NoError(t, err)
	return rc
}

func TestStripe_BelowTheMinimumIsOneFile(t *testing.T) {
	bs := newBridges(t, 3, true, nil)
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	body := randBody(t, testStripeMin-1)

	putBytes(ctx, t, m, "bucket", "small", body)

	assert.Equal(t, body, readAllClose(t, mustGetM(ctx, t, m, "bucket", "small")))
	assert.Equal(t, []string{"/vaultaire/t-tenant-a/bucket/small%o"}, allFiles(t, bs[:1]))
}

// Unknown length (ContentLength 0) is never striped: it streams to one bridge.
func TestStripe_UnknownLengthIsNotStriped(t *testing.T) {
	bs := newBridges(t, 3, true, nil)
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	body := randBody(t, 4*testPiece)

	require.NoError(t, m.Put(ctx, "bucket", "stream", onlyReader{bytes.NewReader(body)}))

	assert.Equal(t, []string{"/vaultaire/t-tenant-a/bucket/stream%o"}, allFiles(t, bs[:1]))
	assert.Equal(t, body, readAllClose(t, mustGetM(ctx, t, m, "bucket", "stream")))
}

// A piece that cannot be stored fails the Put before any manifest: the
// object does not exist, the pieces that landed are deleted, and the error
// says the body is spent (no failover to another backend).
func TestStripe_APieceFailingLeavesNoObject(t *testing.T) {
	// Arrange: bridge 1 refuses every piece upload
	bs := newBridges(t, 3, true, func(i int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if i == 1 && r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/p0") {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	body := randBody(t, 9*testPiece)

	// Act
	err := m.Put(ctx, "bucket", "fails", onlyReader{bytes.NewReader(body)}, engine.WithContentLength(int64(len(body))))

	// Assert
	require.Error(t, err)
	assert.ErrorIs(t, err, engine.ErrNoFailover)
	_, gerr := m.Get(ctx, "bucket", "fails")
	assert.True(t, notFoundErr(gerr), "no manifest, no object: %v", gerr)
	files := fsFiles(t, bs[0].fs, "/vaultaire")
	assert.Empty(t, filesWith(files, "%s"), "no manifest")
	assert.Empty(t, filesWith(files, "/p0"), "the pieces that landed are deleted")
	assert.Empty(t, filesWith(files, "key%o"), "the key file is deleted")
	assert.LessOrEqual(t, m.staged.Load(), int64(0), "no staging file left")
}

func TestStripe_OverwritesReplaceTheGeneration(t *testing.T) {
	bs := newBridges(t, 4, true, nil)
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	v1, v2 := randBody(t, 5*testPiece), randBody(t, 3*testPiece+9)
	small, v4 := randBody(t, 100), randBody(t, 2*testPiece+1)

	putBytes(ctx, t, m, "bucket", "k", v1)
	gen1 := filesWith(allFiles(t, bs[:1]), "key%o")
	require.Len(t, gen1, 1)
	putBytes(ctx, t, m, "bucket", "k", v2)

	assert.Equal(t, v2, readAllClose(t, mustGetM(ctx, t, m, "bucket", "k")))
	files := allFiles(t, bs[:1])
	assert.Len(t, filesWith(files, "/p0"), 4, "only the new generation's pieces")
	assert.Empty(t, filesWith(files, strings.TrimSuffix(gen1[0], "key%o")), "the old generation is gone")

	// striped → plain: the manifest and its pieces go
	putBytes(ctx, t, m, "bucket", "k", small)
	assert.Equal(t, small, readAllClose(t, mustGetM(ctx, t, m, "bucket", "k")))
	files = allFiles(t, bs[:1])
	assert.Empty(t, filesWith(files, "%p/"), "no pieces left")
	assert.Empty(t, filesWith(files, "k%s"))

	// plain → striped: the plain file goes (it would shadow the manifest)
	putBytes(ctx, t, m, "bucket", "k", v4)
	assert.Equal(t, v4, readAllClose(t, mustGetM(ctx, t, m, "bucket", "k")))
	assert.Empty(t, filesWith(allFiles(t, bs[:1]), "k%o"))

	list, err := m.List(ctx, "bucket", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"k"}, list, "the logical object, never a piece")
}

func TestStripe_DeleteRemovesManifestThenPieces(t *testing.T) {
	bs := newBridges(t, 3, true, nil)
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	putBytes(ctx, t, m, "bucket", "gone", randBody(t, 4*testPiece))
	require.Len(t, filesWith(allFiles(t, bs[:1]), "/p0"), 4)

	require.NoError(t, m.Delete(ctx, "bucket", "gone"))

	files := allFiles(t, bs[:1])
	assert.Empty(t, filesWith(files, "gone%s"))
	assert.Empty(t, filesWith(files, "/p0"))
	assert.Empty(t, filesWith(files, "key%o"))
	ok, err := m.Exists(ctx, "bucket", "gone")
	require.NoError(t, err)
	assert.False(t, ok)
}

// The erasure sweep walks the tenant: the striped object surfaces under its
// own name and every piece / key file as a file of the stripe folder, each
// with a Remove that deletes it — nothing of the tenant stays behind.
func TestStripe_WalkTenantRemovesEverything(t *testing.T) {
	bs := newBridges(t, 3, false, nil)
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	putBytes(ctx, t, m, "bucket", "dir/big", randBody(t, 3*testPiece))
	putBytes(ctx, t, m, "bucket", "plain", randBody(t, 10))

	var artifacts []string
	stripeFiles := 0
	require.NoError(t, m.WalkTenant(context.Background(), "tenant-a", func(o engine.TenantObject) error {
		switch o.Container {
		case "bucket":
			artifacts = append(artifacts, o.Artifact)
		case "bucket%p":
			stripeFiles++
		}
		return o.Remove(context.Background())
	}))

	sort.Strings(artifacts)
	assert.Equal(t, []string{"dir/big", "plain"}, artifacts)
	assert.Equal(t, 4, stripeFiles, "3 pieces + the key file, as files of the stripe folder")
	assert.Empty(t, allFiles(t, bs), "every file of the tenant removed on every bridge")
}

// The reaper deletes generations no manifest references once their name
// says they are older than the grace — never a live one, never a young one.
func TestStripe_ReaperDeletesOldOrphansOnly(t *testing.T) {
	// Arrange
	bs := newBridges(t, 3, true, nil)
	m := newStriped(t, bs, nil)
	t0 := time.Date(2026, 10, 7, 6, 0, 0, 0, time.UTC)
	clock := t0
	m.now = func() time.Time { return clock }
	ctx := davCtx("tenant-a")
	putBytes(ctx, t, m, "bucket", "live", randBody(t, 3*testPiece)) // gen at t0, referenced
	tf, cn := davName("t-tenant-a"), davName("bucket")
	orphan := func(at time.Time, artifact string, withKey bool) string {
		gen := newStripeGen(at)
		dir := stripeDir(tf, cn, artifact, gen)
		p := "/vaultaire/" + strings.Join(dir, "/")
		putFile(t, bs[0].fs, p+"/"+pieceName(0), "x")
		if withKey {
			putFile(t, bs[0].fs, p+"/"+stripeKeyFile, `{"container":"bucket","artifact":"`+artifact+`"}`)
		}
		return p
	}
	oldNoManifest := orphan(t0, "never-committed", true)
	oldNoKey := orphan(t0, "died-early", false)
	oldSuperseded := orphan(t0, "live", true) // a previous generation of "live"
	young := orphan(t0.Add(90*time.Minute), "running-upload", true)

	// Act
	clock = t0.Add(2 * time.Hour)
	res, err := m.ReapOrphanStripes(context.Background(), time.Hour)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, 3, res.Reaped, "%+v", res)
	assert.Equal(t, 1, res.Young)
	assert.Equal(t, 1, res.Live)
	files := allFiles(t, bs[:1])
	for _, gone := range []string{oldNoManifest, oldNoKey, oldSuperseded} {
		assert.Empty(t, filesWith(files, gone), gone)
	}
	assert.NotEmpty(t, filesWith(files, young))
	got := readAllClose(t, mustGetM(ctx, t, m, "bucket", "live"))
	assert.Len(t, got, 3*testPiece, "the live object is untouched")
}

// At most K = bridges × large slots pieces are staged on disk at once.
func TestStripe_StagingIsBounded(t *testing.T) {
	var inflight, peak atomic.Int32
	bs := newBridges(t, 2, true, func(_ int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/p0") {
				n := inflight.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				time.Sleep(20 * time.Millisecond)
				defer inflight.Add(-1)
			}
			h.ServeHTTP(w, r)
		})
	})
	m := newStriped(t, bs, func(c *WebDAVConfig) { c.LargeConcurrency = 1 })
	ctx := davCtx("tenant-a")
	body := randBody(t, 12*testPiece)

	putBytes(ctx, t, m, "bucket", "bounded", body)

	assert.Equal(t, int64(2), m.stagedPeak.Load(), "K = 2 bridges × 1 slot, and it was reached")
	assert.LessOrEqual(t, peak.Load(), int32(2), "never more than K pieces in flight")
	assert.Equal(t, int64(0), m.staged.Load())
	assert.Equal(t, body, readAllClose(t, mustGetM(ctx, t, m, "bucket", "bounded")))
}

// A reader that got the previous manifest (the overwrite's pieces replaced
// its pieces in between) reads the manifest once more and serves the new
// version — never a mix, never a miss.
func TestStripe_StaleManifestIsReadAgain(t *testing.T) {
	// Arrange
	var serveStale atomic.Bool
	var stale atomic.Value
	bs := newBridges(t, 3, true, func(_ int, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "%s") && serveStale.CompareAndSwap(true, false) {
				_, _ = w.Write(stale.Load().([]byte))
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	v1, v2 := randBody(t, 3*testPiece), randBody(t, 3*testPiece)
	putBytes(ctx, t, m, "bucket", "k", v1)
	f, err := bs[0].fs.OpenFile(context.Background(), "/vaultaire/t-tenant-a/bucket/k%s", os.O_RDONLY, 0)
	require.NoError(t, err)
	raw, err := io.ReadAll(f)
	require.NoError(t, err)
	_ = f.Close()
	stale.Store(raw)
	putBytes(ctx, t, m, "bucket", "k", v2)

	// Act: another process (no cached manifest) whose first manifest read
	// answers v1's (its pieces are gone)
	reader := newStriped(t, bs, nil)
	serveStale.Store(true)
	got := readAllClose(t, mustGetM(ctx, t, reader, "bucket", "k"))

	// Assert
	assert.Equal(t, v2, got)
	assert.False(t, serveStale.Load(), "the stale manifest was served")
}

// A piece whose bytes changed on the server fails the read (sha256).
func TestStripe_CorruptPieceFailsTheRead(t *testing.T) {
	bs := newBridges(t, 2, true, nil)
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	putBytes(ctx, t, m, "bucket", "k", randBody(t, 3*testPiece))
	p := filesWith(fsFiles(t, bs[0].fs, "/vaultaire"), "/p00001%o")
	require.Len(t, p, 1)
	putFile(t, bs[0].fs, p[0], strings.Repeat("z", testPiece))

	rc, err := m.Get(ctx, "bucket", "k")
	require.NoError(t, err)
	_, err = io.ReadAll(rc)
	_ = rc.Close()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sha256")
}

// A manifest's leaf can never be an ordinary object's: an object whose
// bytes are a manifest is read as bytes.
func TestStripe_AnObjectThatLooksLikeAManifestIsJustBytes(t *testing.T) {
	bs := newBridges(t, 2, true, nil)
	m := newStriped(t, bs, nil)
	ctx := davCtx("tenant-a")
	putBytes(ctx, t, m, "bucket", "real", randBody(t, 2*testPiece))
	f, err := bs[0].fs.OpenFile(context.Background(), "/vaultaire/t-tenant-a/bucket/real%s", os.O_RDONLY, 0)
	require.NoError(t, err)
	manifest, err := io.ReadAll(f)
	require.NoError(t, err)
	_ = f.Close()

	putBytes(ctx, t, m, "bucket", "lookalike", manifest)

	assert.Equal(t, manifest, readAllClose(t, mustGetM(ctx, t, m, "bucket", "lookalike")))
}

func TestStripe_Config(t *testing.T) {
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	base := map[string]string{"SYNC_WEBDAV_PASSWORD": "p"}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	c, ok, err := SyncWebDAVConfigFromEnv(env(with(map[string]string{
		"SYNC_WEBDAV_STRIPE_MIN": "1GiB", "SYNC_WEBDAV_STRIPE_PIECE": "128MiB", "SYNC_WEBDAV_STAGING_DIR": "/var/tmp/stripes"})))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(1<<30), c.StripeMin)
	assert.Equal(t, int64(128<<20), c.StripePiece)
	assert.Equal(t, "/var/tmp/stripes", c.StagingDir)
	assert.Empty(t, c.Warnings)

	c, _, _ = SyncWebDAVConfigFromEnv(env(with(map[string]string{"SYNC_WEBDAV_STRIPE_MIN": "off"})))
	assert.Equal(t, int64(-1), c.StripeMin)

	c, _, _ = SyncWebDAVConfigFromEnv(env(with(map[string]string{
		"SYNC_WEBDAV_STRIPE_MIN": "lots", "SYNC_WEBDAV_STRIPE_PIECE": "1MiB", "SYNC_WEBDAV_STAGING_DIR": "relative"})))
	assert.Zero(t, c.StripeMin)
	assert.Zero(t, c.StripePiece)
	assert.Empty(t, c.StagingDir)
	assert.Len(t, c.Warnings, 3)

	c, _, _ = SyncWebDAVConfigFromEnv(env(with(map[string]string{"SYNC_WEBDAV_STRIPE_MIN": "64MiB"})))
	assert.Equal(t, WebDAVDefaultStripePiece, c.StripeMin, "a minimum below the piece is the piece")
	assert.Len(t, c.Warnings, 1)
}

func TestStripe_GenerationNames(t *testing.T) {
	at := time.Date(2026, 10, 7, 5, 4, 3, 0, time.UTC)
	g := newStripeGen(at)
	got, ok := stripeGenTime(g)
	require.True(t, ok, g)
	assert.Equal(t, at, got)
	for _, bad := range []string{"", "g", "x20261007T050403Z-0123456789abcdef", "g20261007T050403Z-0123456789abcdeZ", "g2026-0123"} {
		_, ok := stripeGenTime(bad)
		assert.False(t, ok, bad)
	}
	assert.False(t, strings.Contains(davName("a%s"), "%s"), "davName never writes the manifest marker")
	assert.False(t, strings.Contains(davName("a%p"), "%p"), "davName never writes the stripe folder suffix")
}
