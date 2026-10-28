package api

// RP74-2: the edge-call gate must cost an unauthenticated caller nothing
// it can grow. A token that is not the registry's shape (64 lower-case hex
// characters) is the route's 404 before any lookup — no query, no cache
// entry — and the cache of the registry's rows is bounded (an LRU of
// dataPlaneHostCacheMax entries, negative entries included).

import (
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func randomTokenOfLen(t *testing.T, n int) string {
	t.Helper()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~+/="
	b := make([]byte, n)
	for i := range b {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		require.NoError(t, err)
		b[i] = alphabet[k.Int64()]
	}
	return string(b)
}

func TestEdgeCaller_AnUnknownTokenCostsNoQueryAndNoEntry(t *testing.T) {
	f := setupEdgeFixture(t)
	f.srv.edge.trustAnyPeer = false
	req := f.signed("/"+f.bucket+"/gate.bin", nil, testBytes(10), nil, "")
	cache := f.srv.dataPlaneHosts()
	cache.invalidate()
	before := cache.lookups.Load()

	// 10,000 tokens of 20–200 bytes that are not 64 lower-case hex.
	for i := range 10000 {
		n := 20 + i%181
		if n == 64 {
			n = 65
		}
		w := f.callFrom("/api/v1/edge/admit", req, randomTokenOfLen(t, n), "198.51.100.90:1")
		require.Equal(t, http.StatusNotFound, w.Code)
	}
	upper := "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"
	w := f.callFrom("/api/v1/edge/admit", req, upper, "198.51.100.90:1")
	require.Equal(t, http.StatusNotFound, w.Code, "upper-case hex is not the registry's shape")
	assert.Equal(t, int64(0), cache.lookups.Load()-before, "a token of another shape is never looked up")
	assert.Equal(t, 0, cache.size(), "and leaves no entry")

	// Tokens of the registry's shape are looked up once each, and the
	// cache never holds more than its bound.
	const hexTokens = 3 * dataPlaneHostCacheMax
	first := ""
	for i := range hexTokens {
		raw := make([]byte, 32)
		_, err := rand.Read(raw)
		require.NoError(t, err)
		tok := hex.EncodeToString(raw)
		if i == hexTokens-1 {
			first = tok
		}
		w := f.callFrom("/api/v1/edge/admit", req, tok, "198.51.100.90:1")
		require.Equal(t, http.StatusNotFound, w.Code)
		require.LessOrEqual(t, cache.size(), dataPlaneHostCacheMax)
	}
	assert.Equal(t, int64(hexTokens), cache.lookups.Load()-before, "one query per distinct well-formed token")
	assert.Equal(t, dataPlaneHostCacheMax, cache.size())
	w = f.callFrom("/api/v1/edge/admit", req, first, "198.51.100.90:1")
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, int64(hexTokens), cache.lookups.Load()-before, "a recent negative entry is a hit: no second query")
}

func TestDataPlaneHostCache_LRUEvictsTheLeastRecentlyUsedAndExpiredEntries(t *testing.T) {
	c := newDataPlaneHostCache(nil)
	now := c.now()
	c.now = func() time.Time { return now }
	for i := range dataPlaneHostCacheMax {
		c.put(hex.EncodeToString([]byte{byte(i >> 8), byte(i)}), cachedDataPlaneHost{at: now})
	}
	require.Equal(t, dataPlaneHostCacheMax, c.size())
	// Touch the oldest: it survives the next insert, the second-oldest goes.
	_, ok := c.get("0000")
	require.True(t, ok)
	c.put("ffff", cachedDataPlaneHost{at: now})
	_, ok = c.get("0000")
	assert.True(t, ok, "used recently: kept")
	_, ok = c.get("0001")
	assert.False(t, ok, "least recently used: evicted")
	assert.Equal(t, dataPlaneHostCacheMax, c.size())

	// Past the TTL every entry is dropped on the next insert.
	now = now.Add(dataPlaneHostCacheTTL + time.Second)
	c.put("aaaa", cachedDataPlaneHost{at: now})
	assert.Equal(t, 1, c.size(), "expired entries go on insert")
}

// RP74-11: a heartbeat changes no token — it updates its own cached row in
// place (health, last_seen) instead of flushing every node's entry.
func TestEdgeHeartbeat_UpdatesItsCachedRowAndFlushesNothing(t *testing.T) {
	f := setupEdgeFixture(t)
	_, tokA := f.registerNode("198.51.100.101", "", "down")
	_, tokB := f.registerNode("198.51.100.102", "", "up")
	cache := f.srv.dataPlaneHosts()
	req := f.signed("/"+f.bucket+"/hb.bin", nil, testBytes(10), nil, "")

	w := f.callFrom("/api/v1/edge/admit", req, tokB, "198.51.100.102:1")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = f.callFrom("/api/v1/edge/admit", req, tokA, "198.51.100.101:1")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "A is down until its heartbeat")
	lookups := cache.lookups.Load()

	hb := httptest.NewRequest(http.MethodPost, "/api/v1/edge/heartbeat", strings.NewReader(`{"version":"t"}`))
	hb.RemoteAddr = "198.51.100.101:1"
	hb.Header.Set(edgeTokenHeader, tokA)
	hw := httptest.NewRecorder()
	f.srv.handleEdgeHeartbeat(hw, hb)
	require.Equal(t, http.StatusOK, hw.Code, hw.Body.String())

	w = f.callFrom("/api/v1/edge/admit", req, tokA, "198.51.100.101:1")
	assert.Equal(t, http.StatusOK, w.Code, "up from its cached row at once")
	w = f.callFrom("/api/v1/edge/admit", req, tokB, "198.51.100.102:1")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, lookups, cache.lookups.Load(), "no registry read after a heartbeat")
}
