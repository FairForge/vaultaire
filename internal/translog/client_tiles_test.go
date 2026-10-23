package translog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// K4b (Stage 1a Prompt 32 PR 2): Client.Tiles — the witness's mirror. A
// full tile tlog authenticated is handed to the cache; the next read of it
// comes from the cache and is authenticated again; EntryBundle proves every
// entry of a bundle against the checkpoint.

type mapTiles struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (c *mapTiles) Tile(p string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.m[p]
	return b, ok
}

func (c *mapTiles) SaveTile(p string, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[p] = b
}

func TestTranslogClient_FullTilesFromTheCache(t *testing.T) {
	// Arrange: 300 entries, a client counting its tile GETs.
	k := newTestKeys(t)
	m := memLog(t, 300)
	inner := serve(t, m, k.signer)
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[strings.TrimPrefix(r.URL.Path, "/translog/")]++
		mu.Unlock()
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	cache := &mapTiles{m: map[string][]byte{}}
	c := &Client{BaseURL: srv.URL + "/translog/", Verifier: k.verifier, Tiles: cache}
	ctx := context.Background()
	cp, _, err := c.Checkpoint(ctx)
	require.NoError(t, err)
	tile0 := TilePath(0, 0, TileWidth)

	// Act: two proofs that need the full tile 0/0.
	_, err = c.VerifyInclusion(ctx, cp, 3)
	require.NoError(t, err)
	_, err = c.VerifyInclusion(ctx, cp, 7)
	require.NoError(t, err)

	// Assert: fetched once, kept, then read from the cache; no partial tile kept.
	assert.Equal(t, 1, hits[tile0])
	_, ok := cache.Tile(tile0)
	assert.True(t, ok)
	for p := range cache.m {
		assert.NotContains(t, p, ".p/", "only full tiles are immutable")
	}

	// A corrupted cached tile is an error, never a wrong hash.
	cache.m[tile0] = append([]byte{1}, cache.m[tile0][1:]...)
	_, err = c.VerifyInclusion(ctx, cp, 9)
	assert.Error(t, err)
}

func TestTranslogClient_EntryBundleProvesEveryEntry(t *testing.T) {
	k := newTestKeys(t)
	m := memLog(t, 300)
	srv := serve(t, m, k.signer)
	c := &Client{BaseURL: srv.URL + "/translog/", Verifier: k.verifier}
	ctx := context.Background()
	cp, _, err := c.Checkpoint(ctx)
	require.NoError(t, err)

	raw, bodies, err := c.EntryBundle(ctx, cp, 1)
	require.NoError(t, err)
	assert.Len(t, bodies, 44, "the partial bundle at the checkpoint's size")
	assert.Equal(t, "entry-256", string(bodies[0]))
	want, err := EntryBundle(bodies)
	require.NoError(t, err)
	assert.Equal(t, want, raw)
	_, _, err = c.EntryBundle(ctx, cp, 2)
	assert.Error(t, err, "no bundle 2 in a tree of 300")
}
