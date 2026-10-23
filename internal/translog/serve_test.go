package translog

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/sumdb/note"
)

type testKeys struct {
	signer   note.Signer
	verifier note.Verifier
}

func newTestKeys(t *testing.T) testKeys {
	t.Helper()
	skey, vkey, err := GenerateKey(rand.Reader)
	require.NoError(t, err)
	s, err := NewSigner(skey)
	require.NoError(t, err)
	v, err := NewVerifier(vkey)
	require.NoError(t, err)
	return testKeys{s, v}
}

// serve publishes m through NewHandler, mounted under /translog/ as the
// server mounts it.
func serve(t *testing.T, m *Memory, s note.Signer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/translog/", http.StripPrefix("/translog/", NewHandler(&MemorySource{Log: m, Signer: s})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url) // #nosec G107 -- a test server
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, b
}

func TestTranslogHandler_CheckpointTilesAndCacheHeaders(t *testing.T) {
	k := newTestKeys(t)
	m := memLog(t, 300)
	srv := serve(t, m, k.signer)

	resp, body := get(t, srv.URL+"/translog/checkpoint")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	cp, err := OpenCheckpoint(body, k.verifier)
	require.NoError(t, err)
	assert.Equal(t, int64(300), cp.Size)

	// A full tile is immutable; a partial one is not cached.
	resp, body = get(t, srv.URL+"/translog/tile/0/000")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "public, max-age=31536000, immutable", resp.Header.Get("Cache-Control"))
	assert.Len(t, body, 256*HashSize)
	resp, body = get(t, srv.URL+"/translog/tile/0/001.p/44")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Len(t, body, 44*HashSize)
	// An older checkpoint's partial tile is still served.
	resp, _ = get(t, srv.URL+"/translog/tile/0/001.p/10")
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Entry bundles.
	resp, body = get(t, srv.URL+"/translog/tile/entries/001.p/44")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	entries, err := ParseEntryBundle(body)
	require.NoError(t, err)
	require.Len(t, entries, 44)
	assert.Equal(t, "entry-299", string(entries[43]))
	resp, _ = get(t, srv.URL+"/translog/tile/entries/000")
	assert.Equal(t, "public, max-age=31536000, immutable", resp.Header.Get("Cache-Control"))

	// Beyond the checkpoint, or not in the layout: 404.
	for _, p := range []string{"tile/0/001", "tile/0/001.p/45", "tile/0/002.p/1", "tile/1/000.p/2", "tile/entries/001.p/45", "tile/0/x000/001", "nope", "tile/0/1"} {
		resp, _ := get(t, srv.URL+"/translog/"+p)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, p)
		// RP42-2: a tile beyond the checkpoint exists later — its 404 must
		// not be cached by the edge in front of the trust host.
		assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"), p)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/translog/checkpoint", nil)
	require.NoError(t, err)
	r, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = r.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, r.StatusCode)
}

func TestTranslogHandler_NoCheckpointIs404(t *testing.T) {
	m := memLog(t, 3)
	srv := serve(t, m, nil) // no key: nothing signed, nothing served
	for _, p := range []string{"checkpoint", "tile/0/000.p/3", "tile/entries/000.p/3"} {
		resp, _ := get(t, srv.URL+"/translog/"+p)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, p)
		assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"), "no checkpoint yet: a 404 that must not outlive the first signature (%s)", p)
	}
}

func TestTranslogClient_InclusionAndConsistency(t *testing.T) {
	k := newTestKeys(t)
	m := memLog(t, 40)
	srv := serve(t, m, k.signer)
	c := &Client{BaseURL: srv.URL + "/translog/", Verifier: k.verifier}
	ctx := context.Background()

	old, _, err := c.Checkpoint(ctx)
	require.NoError(t, err)
	for _, idx := range []int64{0, 5, 39} {
		body, err := c.VerifyInclusion(ctx, old, idx)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("entry-%d", idx), string(body))
	}
	_, err = c.VerifyInclusion(ctx, old, 40)
	assert.Error(t, err)

	for i := 40; i < 600; i++ {
		_, err := m.Append([]byte(fmt.Sprintf("entry-%d", i)))
		require.NoError(t, err)
	}
	cp, _, err := c.Checkpoint(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(600), cp.Size)
	require.NoError(t, c.VerifyConsistency(ctx, old, cp), "an honest log that grew is consistent")
	body, err := c.VerifyInclusion(ctx, cp, 513)
	require.NoError(t, err)
	assert.Equal(t, "entry-513", string(body))
	assert.Error(t, c.VerifyConsistency(ctx, cp, old), "a shrunk log is not")
}

// TestTranslogVerify_RewrittenHistoryFails: the witness role. A checkpoint
// saved earlier, then the log re-served with entry 5 rewritten (same key,
// a valid signature on the new checkpoint): the consistency check fails.
func TestTranslogVerify_RewrittenHistoryFails(t *testing.T) {
	k := newTestKeys(t)
	ctx := context.Background()
	honest := memLog(t, 40)
	old, _, err := (&Client{BaseURL: serve(t, honest, k.signer).URL + "/translog/", Verifier: k.verifier}).Checkpoint(ctx)
	require.NoError(t, err)

	forged := &Memory{}
	for i := 0; i < 60; i++ {
		body := fmt.Sprintf("entry-%d", i)
		if i == 5 {
			body = "entry-5-rewritten"
		}
		_, err := forged.Append([]byte(body))
		require.NoError(t, err)
	}
	c := &Client{BaseURL: serve(t, forged, k.signer).URL + "/translog/", Verifier: k.verifier}
	cp, _, err := c.Checkpoint(ctx)
	require.NoError(t, err, "the forged checkpoint is validly signed")
	err = c.VerifyConsistency(ctx, old, cp)
	require.Error(t, err, "a rewritten history verified")

	// And a server that lies about one entry's bytes under an honest tree
	// fails the inclusion check.
	liar := memLog(t, 40)
	mux := http.NewServeMux()
	inner := NewHandler(&MemorySource{Log: liar, Signer: k.signer})
	mux.Handle("/translog/", http.StripPrefix("/translog/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "tile/entries/000.p/40" {
			bodies := make([][]byte, 40)
			for i := range bodies {
				bodies[i] = []byte(fmt.Sprintf("entry-%d", i))
			}
			bodies[7] = []byte("entry-7-swapped")
			b, _ := EntryBundle(bodies)
			_, _ = w.Write(b)
			return
		}
		inner.ServeHTTP(w, r)
	})))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	lc := &Client{BaseURL: srv.URL + "/translog/", Verifier: k.verifier}
	lcp, _, err := lc.Checkpoint(ctx)
	require.NoError(t, err)
	_, err = lc.VerifyInclusion(ctx, lcp, 7)
	assert.Error(t, err)
	_, err = lc.VerifyInclusion(ctx, lcp, 8)
	assert.NoError(t, err)
}
