package api

// RP14-1 (WP-P13 post-merge review, P2): a chunk PUT cancelled after its
// request went out kept its claim for the takeover window (RP13-1, the
// right call for a PUT whose outcome is unknown) — but the pool cancelled
// every sibling PUT in flight when a chunked upload was cut, so a client's
// immediate retry waited up to 3 min on up to eight kept claims with every
// worker blocked. Now a chunk PUT that has started runs to its own end on a
// context detached from the request, under chunkPutBound, and its outcome is
// known: answered OK → the row is ready (at ref 0: the aborted write took no
// reference) and the retry dedups onto it within seconds; answered with an
// error → the claim is dropped; no answer → kept, as before. A chunk whose
// PUT had not started when the request ended is never attempted.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
)

// cutUpload starts a chunked PUT of body (a chunkyBody: it has the chunks)
// on a cancellable context, waits until at least two chunk PUTs are at the
// backend, cuts the client, and
// returns once the handler has answered (not 200) with the number of chunk
// PUTs at the backend — sampled AFTER the answer, once it has held still for
// 200 ms (RP42-4: sampled before cancel(), a worker already dispatched
// entered the gated driver afterwards — 2 sampled, 3 detached).
func (f *pendingFixture) cutUpload(t *testing.T, key string, body []byte) int32 {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan int, 1)
	go func() { first <- f.doCtx(ctx, t, "PUT", "/test-bucket/"+key, body, nil).Code }()
	waitFor(t, "chunk PUTs in flight", func() bool { return f.gated.inFlight.Load() >= 2 })
	cancel()
	select {
	case code := <-first:
		require.NotEqual(t, http.StatusOK, code)
	case <-time.After(10 * time.Second):
		t.Fatal("the cut upload's handler did not answer")
	}
	return stableInFlight(t, &f.gated.inFlight)
}

// stableInFlight is n once it has not moved for 200 ms (5 s at most).
func stableInFlight(t *testing.T, n interface{ Load() int32 }) int32 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	v := n.Load()
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		w := n.Load()
		if w == v {
			return v
		}
		v = w
	}
	t.Fatalf("the chunk PUTs in flight never settled (last %d)", v)
	return v
}

func TestChunkClaim_ARetryOfACutUploadDedupsOntoTheDetachedPutsWithinSeconds(t *testing.T) {
	// Arrange: a chunked PUT cut while its chunk PUTs wait on the backend.
	// The takeover window is long enough that a retry waiting on it would
	// time this test out.
	f := setupPendingFixture(t)
	f.server.chunkPendingTakeover = 60 * time.Second
	body := chunkyBody(t, 8<<20)
	inFlight := f.cutUpload(t, "cut.bin", body)

	// Act: the same body again at once; the backend answers the detached
	// PUTs 300 ms later.
	retry := make(chan int, 1)
	start := time.Now()
	go func() { retry <- f.do(t, "PUT", "/test-bucket/cut.bin", body, nil).Code }()
	time.Sleep(300 * time.Millisecond)
	close(f.gated.gate)
	select {
	case code := <-retry:
		require.Equal(t, http.StatusOK, code)
	case <-time.After(15 * time.Second):
		t.Fatalf("the retry is still waiting on the %d claims the cut upload kept (the takeover window is %s)", inFlight, f.server.chunkPendingTakeover)
	}
	assert.Less(t, time.Since(start), 10*time.Second, "the retry completes in seconds, not the window")

	// Assert: the object reads back; every chunk is ready, one manifest,
	// every chunk's references are exactly the retry's.
	assert.Equal(t, body, f.getObj(t, "cut.bin"))
	var notReady, manifests, wrongRefs int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index g
		JOIN tenant_chunk_refs r ON r.dedup_scope = g.dedup_scope AND r.plaintext_hash = g.plaintext_hash
		WHERE r.tenant_id = $1 AND g.state <> 'ready'`, f.tenantID).Scan(&notReady))
	assert.Zero(t, notReady, "every chunk the manifest names is ready")
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(DISTINCT object_key) FROM tenant_chunk_refs WHERE tenant_id = $1`, f.tenantID).Scan(&manifests))
	assert.Equal(t, 1, manifests, "one manifest")
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index g
		JOIN (SELECT dedup_scope, plaintext_hash, COUNT(*) AS n FROM tenant_chunk_refs WHERE tenant_id = $1 GROUP BY 1, 2) r
		  ON r.dedup_scope = g.dedup_scope AND r.plaintext_hash = g.plaintext_hash
		WHERE g.ref_count <> r.n`, f.tenantID).Scan(&wrongRefs))
	assert.Zero(t, wrongRefs, "a chunk's ref_count is exactly the retry's references (the aborted write's were given back)")
	var pending int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = $1 AND backend_id = 'fixed' AND state = 'pending'`,
		crypto.GlobalDedupScope).Scan(&pending))
	assert.Zero(t, pending, "no claim of the cut upload is left behind")
}

func TestChunkClaim_ADetachedPutTheBackendAnswersWithAnErrorDropsItsClaim(t *testing.T) {
	// Arrange: the request goes away while the backend holds the body; the
	// backend then answers 500 (nothing committed).
	o := setupOutcomeFixture(t, stallGateThenAnswer)
	o.server.chunkPutBound = 30 * time.Second
	o.server.chunkPendingTakeover = 60 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	o.cutAfterTheBodyWentOut(cancel)
	dropped0 := promtest.ToFloat64(chunkPutDetached.WithLabelValues("dropped"))
	require.NotEqual(t, http.StatusOK, o.put(ctx, t, "refused.bin"))
	found, _, _ := o.claim(t)
	require.True(t, found, "the claim is kept while the PUT runs")

	// Act: the backend answers.
	close(o.drv.gate)
	waitFor(t, "the claim to be dropped", func() bool { found, _, _ := o.claim(t); return !found })

	// Assert: dropped, counted; the retry stores at once.
	assert.Equal(t, dropped0+1, promtest.ToFloat64(chunkPutDetached.WithLabelValues("dropped")))
	o.drv.mode.Store(int32(stallOff))
	start := time.Now()
	require.Equal(t, http.StatusOK, o.put(context.Background(), t, "refused.bin"))
	assert.Less(t, time.Since(start), 5*time.Second, "no window waited")
	assert.Equal(t, 1, o.refCount(t))
}

func TestChunkClaim_ADetachedPutWithNoAnswerStillKeepsItsClaimForTheWindow(t *testing.T) {
	// Arrange: the request goes away while the backend holds the body, and
	// the backend never answers: the chunk bound ends the PUT. The RP13-1
	// guarantee stays — the claim is kept, the retry waits the window.
	o := setupOutcomeFixture(t, stallSilent) // bound 300 ms, window 1.5 s
	ctx, cancel := context.WithCancel(context.Background())
	o.cutAfterTheBodyWentOut(cancel)
	kept0 := promtest.ToFloat64(chunkPutDetached.WithLabelValues("kept"))
	require.NotEqual(t, http.StatusOK, o.put(ctx, t, "silent.bin"))

	// Act: the bound ends the detached PUT with no answer.
	waitFor(t, "the detached PUT to end at its bound", func() bool {
		return promtest.ToFloat64(chunkPutDetached.WithLabelValues("kept")) == kept0+1
	})
	found, state, since := o.claim(t)
	require.True(t, found, "a PUT with no answer may still land: its claim stays")
	assert.Equal(t, "pending", state)

	// Assert: the retry waits the window out, takes the claim over, stores.
	o.drv.mode.Store(int32(stallOff))
	require.Equal(t, http.StatusOK, o.put(context.Background(), t, "silent.bin"))
	assert.GreaterOrEqual(t, time.Since(since), o.server.chunkPendingTakeover-100*time.Millisecond, "the window was waited")
	_, state, _ = o.claim(t)
	assert.Equal(t, "ready", state)
	assert.Equal(t, 1, o.refCount(t))
}

func TestChunkClaim_AStoppingSlotWaitsForTheDetachedChunkPuts(t *testing.T) {
	// Arrange: an upload cut while its chunk PUTs wait on the backend — they
	// finish detached — and the stop sequence starts.
	f := setupPendingFixture(t)
	f.server.chunkPendingTakeover = 60 * time.Second
	if f.server.healthChecker == nil {
		f.server.healthChecker = NewBackendHealthChecker()
	}
	body := chunkyBody(t, 6<<20)
	inFlight := f.cutUpload(t, "deploy.bin", body)
	require.EqualValues(t, inFlight, f.server.chunkPuts().count(), "every detached PUT is accounted")
	rows, err := f.db.Query(`SELECT plaintext_hash FROM global_content_index WHERE dedup_scope = $1 AND backend_id = 'fixed' AND state = 'pending'`, crypto.GlobalDedupScope)
	require.NoError(t, err)
	var hashes []string
	for rows.Next() {
		var h string
		require.NoError(t, rows.Scan(&h))
		hashes = append(hashes, h)
	}
	require.NoError(t, rows.Err())
	_ = rows.Close()
	require.NotEmpty(t, hashes, "the detached PUTs hold their claims")

	// /health names them to the slot's own port (vaultaire-switch polls it
	// before stopping the old slot), never through HAProxy.
	health := func(xff string) map[string]any {
		r := httptest.NewRequest("GET", "/health", nil)
		r.RemoteAddr = "127.0.0.1:40000"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		f.server.handleHealthEnhanced(w, r)
		var m map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
		return m
	}
	assert.EqualValues(t, inFlight, health("")["chunk_puts_in_flight"])
	_, exposed := health("203.0.113.9")["chunk_puts_in_flight"]
	assert.False(t, exposed)

	// Act: the drain runs while the PUTs are in flight; the backend answers
	// 300 ms later.
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(f.gated.gate)
	}()
	start := time.Now()
	left := f.server.drainChunkPuts(10 * time.Second)
	waited := time.Since(start)

	// Assert: it returned only once every PUT had ended AND flipped its row
	// — the engine (which closes the database) runs after it — and the
	// chunks are ready at ref 0 for the next writer.
	assert.Zero(t, left)
	assert.GreaterOrEqual(t, waited, 250*time.Millisecond, "the drain waited for the PUTs")
	assert.Zero(t, f.server.chunkPuts().count())
	for _, h := range hashes {
		var state string
		var refs int
		require.NoError(t, f.db.QueryRow(`SELECT state, ref_count FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`,
			crypto.GlobalDedupScope, h).Scan(&state, &refs))
		assert.Equal(t, "ready", state, "chunk %s flipped before the drain returned", h[:16])
		assert.Zero(t, refs)
	}
}

func TestChunkClaim_AChunkNotStartedWhenTheRequestEndedIsNeverAttempted(t *testing.T) {
	// Arrange: one worker, so at most one chunk PUT is at the backend when
	// the client is cut; the chunks behind it are never attempted and leave
	// no claim.
	f := setupPendingFixture(t)
	f.server.chunkPutConcurrency = 1
	body := randomBody(t, 8<<20)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan int, 1)
	go func() { first <- f.doCtx(ctx, t, "PUT", "/test-bucket/behind.bin", body, nil).Code }()
	waitFor(t, "a chunk PUT in flight", func() bool { return f.gated.inFlight.Load() >= 1 })
	puts0 := f.gated.puts.Load()

	// Act
	cancel()
	require.NotEqual(t, http.StatusOK, <-first)
	close(f.gated.gate)
	waitFor(t, "the detached PUT to end", func() bool { return f.server.chunkPuts().count() == 0 })

	// Assert: exactly the PUT in flight at the cut ran; no claim is left.
	assert.Equal(t, puts0, f.gated.puts.Load(), "a chunk not started when the request ended was attempted")
	var pending int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = $1 AND backend_id = 'fixed' AND state = 'pending'`,
		crypto.GlobalDedupScope).Scan(&pending))
	assert.Zero(t, pending)
}
