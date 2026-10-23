package witness

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
)

// K4b (Stage 1a Prompt 32 PR 2): the witness co-signs the checkpoints it
// proved and keeps its own byte-exact copy of the log.

type logKeys struct {
	signer   note.Signer
	verifier note.Verifier
}

func newLogKeys(t *testing.T) logKeys {
	t.Helper()
	skey, vkey, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s, err := translog.NewSigner(skey)
	require.NoError(t, err)
	v, err := translog.NewVerifier(vkey)
	require.NoError(t, err)
	return logKeys{s, v}
}

func witnessKeys(t *testing.T) (note.Signer, note.Verifier) {
	t.Helper()
	skey, vkey, err := note.GenerateKey(rand.Reader, "witness.test/mac")
	require.NoError(t, err)
	s, err := note.NewSigner(skey)
	require.NoError(t, err)
	v, err := note.NewVerifier(vkey)
	require.NoError(t, err)
	return s, v
}

func memLog(t *testing.T, n int, tag string) *translog.Memory {
	t.Helper()
	m := &translog.Memory{}
	for i := 0; i < n; i++ {
		_, err := m.Append([]byte(fmt.Sprintf(`{"entry":%d,"tag":%q}`, i, tag)))
		require.NoError(t, err)
	}
	return m
}

// served is a log behind an httptest server that counts GETs by path.
type served struct {
	url  string
	mu   sync.Mutex
	hits map[string]int
	src  *translog.MemorySource
}

func serve(t *testing.T, m *translog.Memory, k logKeys) *served {
	t.Helper()
	s := &served{hits: map[string]int{}, src: &translog.MemorySource{Log: m, Signer: k.signer}}
	h := http.StripPrefix("/translog/", translog.NewHandler(s.src))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[strings.TrimPrefix(r.URL.Path, "/translog/")]++
		s.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL + "/translog/"
	return s
}

func (s *served) hitsOf(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func (s *served) reset() {
	s.mu.Lock()
	s.hits = map[string]int{}
	s.mu.Unlock()
}

func newWitness(t *testing.T, dir string, s *served, k logKeys, ws note.Signer) *Witness {
	t.Helper()
	return &Witness{Dir: dir, Client: &translog.Client{BaseURL: s.url, Verifier: k.verifier}, Signer: ws}
}

func TestWitness_CosignedNoteCarriesBothKeys(t *testing.T) {
	// Arrange
	k := newLogKeys(t)
	ws, wv := witnessKeys(t)
	dir := t.TempDir()
	s := serve(t, memLog(t, 300, "a"), k)

	// Act
	res, err := newWitness(t, dir, s, k, ws).Pass(context.Background())

	// Assert: the note opens with BOTH verifiers, each signature verified.
	require.NoError(t, err)
	assert.Equal(t, int64(300), res.Size)
	assert.Equal(t, int64(-1), res.PrevSize)
	cos, err := os.ReadFile(filepath.Join(dir, CosignFile))
	require.NoError(t, err)
	assert.Equal(t, res.Cosigned, cos)
	n, err := note.Open(cos, note.VerifierList(k.verifier, wv))
	require.NoError(t, err)
	require.Len(t, n.Sigs, 2, "the log's signature and the witness's")
	names := []string{n.Sigs[0].Name, n.Sigs[1].Name}
	assert.ElementsMatch(t, []string{translog.Origin, "witness.test/mac"}, names)
	// A reader with only the log's key still opens it (the witness's line is
	// an unknown, not a bad, signature).
	cp, err := translog.OpenCheckpoint(cos, k.verifier)
	require.NoError(t, err)
	assert.Equal(t, res.Root, cp.Root)
	// The state is saved and the history has one line.
	state, err := os.ReadFile(filepath.Join(dir, StateFile))
	require.NoError(t, err)
	_, err = translog.OpenCheckpoint(state, k.verifier)
	require.NoError(t, err)
	hist, err := os.ReadFile(filepath.Join(dir, HistoryFile))
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(hist), "\n"))

	// A second pass at the same checkpoint adds no history line.
	_, err = newWitness(t, dir, s, k, ws).Pass(context.Background())
	require.NoError(t, err)
	hist2, err := os.ReadFile(filepath.Join(dir, HistoryFile))
	require.NoError(t, err)
	assert.Equal(t, hist, hist2)
}

func TestWitness_ForkRefused_StateAndCacheUntouched(t *testing.T) {
	// Arrange: the witness accepted a tree of 300 entries.
	k := newLogKeys(t)
	ws, _ := witnessKeys(t)
	dir := t.TempDir()
	honest := serve(t, memLog(t, 300, "a"), k)
	_, err := newWitness(t, dir, honest, k, ws).Pass(context.Background())
	require.NoError(t, err)
	state := readAll(t, filepath.Join(dir, StateFile))
	cosigned := readAll(t, filepath.Join(dir, CosignFile))
	cache := snapshot(t, dir)

	// Act: the same key now serves a DIFFERENT tree of the same size — and
	// one that grew from a rewritten prefix.
	_, errSame := newWitness(t, dir, serve(t, memLog(t, 300, "b"), k), k, ws).Pass(context.Background())
	_, errGrown := newWitness(t, dir, serve(t, memLog(t, 600, "b"), k), k, ws).Pass(context.Background())

	// Assert
	require.Error(t, errSame)
	assert.Contains(t, errSame.Error(), "fork")
	require.Error(t, errGrown)
	assert.Contains(t, errGrown.Error(), "does not extend")
	assert.Equal(t, state, readAll(t, filepath.Join(dir, StateFile)), "a refused pass never moves the accepted checkpoint")
	assert.Equal(t, cosigned, readAll(t, filepath.Join(dir, CosignFile)), "nothing co-signed")
	assert.Equal(t, cache, snapshot(t, dir), "no byte of the forked tree is kept")
}

func TestWitness_CachesEntryBundlesByteExact(t *testing.T) {
	// Arrange: 300 entries = one full bundle (256) + a partial one (44).
	k := newLogKeys(t)
	ws, _ := witnessKeys(t)
	dir := t.TempDir()
	m := memLog(t, 300, "a")
	s := serve(t, m, k)
	ctx := context.Background()

	// Act
	res, err := newWitness(t, dir, s, k, ws).Pass(ctx)
	require.NoError(t, err)

	// Assert: byte for byte what the log serves.
	assert.Equal(t, 2, res.Bundles)
	full := translog.TilePath(translog.EntriesLevel, 0, translog.TileWidth)
	part := translog.TilePath(translog.EntriesLevel, 1, 44)
	tile0 := translog.TilePath(0, 0, translog.TileWidth)
	for _, p := range []string{full, part, tile0} {
		assert.Equal(t, get(t, s.url+p), readAll(t, filepath.Join(dir, "cache", filepath.FromSlash(p))), p)
	}

	// The log grows to 520: the immutable full bundle and tile are never
	// fetched again; the partial copy is replaced by the wider one.
	for i := 300; i < 520; i++ {
		_, err := m.Append([]byte(fmt.Sprintf(`{"entry":%d,"tag":"a"}`, i)))
		require.NoError(t, err)
	}
	s.reset()
	res, err = newWitness(t, dir, s, k, ws).Pass(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(300), res.PrevSize)
	assert.Zero(t, s.hitsOf(full), "a full bundle is immutable: never re-fetched")
	assert.Zero(t, s.hitsOf(tile0), "a full tile is immutable: read from the cache")
	assert.Equal(t, 1, s.hitsOf(translog.TilePath(translog.EntriesLevel, 1, translog.TileWidth)))
	assert.Equal(t, 1, s.hitsOf(translog.TilePath(translog.EntriesLevel, 2, 8)))
	_, err = os.Stat(filepath.Join(dir, "cache", filepath.FromSlash(part)))
	assert.ErrorIs(t, err, os.ErrNotExist, "the narrower partial copy is dropped once the full bundle is kept")

	// What the cache gives back is the log's entries, byte for byte.
	got, err := OpenCache(dir).Entries(250, 520)
	require.NoError(t, err)
	require.Len(t, got, 270)
	for i, b := range got {
		assert.Equal(t, m.Entry(int64(250+i)), b)
	}
	_, err = OpenCache(dir).Entries(0, 521)
	assert.ErrorContains(t, err, "bundle 2", "a range past what the witness saw names the bundle")

	// And a third pass at the same size fetches only the checkpoint.
	s.reset()
	res, err = newWitness(t, dir, s, k, ws).Pass(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Bundles)
	s.mu.Lock()
	assert.Equal(t, map[string]int{"checkpoint": 1}, s.hits)
	s.mu.Unlock()
}

func TestWitness_RefusesTheLogsOwnKey(t *testing.T) {
	k := newLogKeys(t)
	_, err := newWitness(t, t.TempDir(), serve(t, memLog(t, 3, "a"), k), k, k.signer).Pass(context.Background())
	assert.ErrorIs(t, err, ErrSameKey)
}

func readAll(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- test temp dir
	require.NoError(t, err)
	return b
}

func get(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url) // #nosec G107 -- the test's own server
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, url)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return b
}

// snapshot is every file under dir/cache with its bytes.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := filepath.Join(dir, "cache")
	require.NoError(t, filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p) // #nosec G304 -- test temp dir
		out[p] = string(b)
		return err
	}))
	return out
}

func TestWitness_ForgedEntryBundleRefused_NothingKept(t *testing.T) {
	// Arrange: an honest checkpoint and tiles, but the entry bundle served
	// has one byte changed (a mirror or a proxy lying about an entry).
	k := newLogKeys(t)
	ws, _ := witnessKeys(t)
	dir := t.TempDir()
	m := memLog(t, 20, "a")
	src := &translog.MemorySource{Log: m, Signer: k.signer}
	h := http.StripPrefix("/translog/", translog.NewHandler(src))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tile/entries/") {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			b := rec.Body.Bytes()
			b[len(b)-3] ^= 1
			_, _ = w.Write(b)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	w := &Witness{Dir: dir, Client: &translog.Client{BaseURL: srv.URL + "/translog/", Verifier: k.verifier}, Signer: ws}

	// Act
	_, err := w.Pass(context.Background())

	// Assert
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not the entry the checkpoint of 20 commits to")
	_, err = os.Stat(filepath.Join(dir, StateFile))
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, _, ok, err := OpenCache(dir).Bundle(0)
	require.NoError(t, err)
	assert.False(t, ok, "an entry that does not prove is never kept")
}
