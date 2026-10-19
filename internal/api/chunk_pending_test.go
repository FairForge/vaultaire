package api

// WP-R8-2 B.1 (R8-09a): a new chunk's store held a pooled connection and the
// chunk's advisory lock across the backend PUT — 8 workers × 6 chunked PUTs
// pinned 48 of the pool's 50 connections to backend latency. The store is
// now claim (pending) → PUT with nothing held → ready; a pending row is
// referenced by nobody until it is ready, and a writer that died is taken
// over.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
)

// gatedChunkDriver is the fixture's fixed-bucket primary whose chunk PUTs
// (container _global) wait on a gate: a backend that is slow to answer.
type gatedChunkDriver struct {
	*fixedBucketDriver
	gate     chan struct{}
	inFlight atomic.Int32
	puts     atomic.Int32
}

func (d *gatedChunkDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	if container == chunkContainer {
		d.puts.Add(1)
		d.inFlight.Add(1)
		defer d.inFlight.Add(-1)
		select {
		case <-d.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return d.fixedBucketDriver.Put(ctx, container, artifact, data, opts...)
}

type pendingFixture struct {
	*mpChunkFixture
	gated *gatedChunkDriver
}

func setupPendingFixture(t *testing.T) *pendingFixture {
	t.Helper()
	f := setupMPChunkFixture(t, false)
	g := &gatedChunkDriver{fixedBucketDriver: f.fixed, gate: make(chan struct{})}
	require.NoError(t, f.server.engine.AddDriver("fixed", g))
	// Abandoned claims on this fixture's own backend name (a run that died
	// mid-test) hold no reference and belong to no tenant, so no tenant
	// cleanup reaches them: swept before and after, because the detached-
	// PUT tests count the pending rows on this backend.
	sweep := func() {
		_, _ = f.db.Exec(`DELETE FROM global_content_index WHERE backend_id = 'fixed' AND state = 'pending' AND ref_count = 0`)
	}
	sweep()
	t.Cleanup(sweep)
	return &pendingFixture{mpChunkFixture: f, gated: g}
}

func (f *pendingFixture) do(t *testing.T, method, target string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return f.doCtx(context.Background(), t, method, target, body, hdr)
}

// doCtx is do on the caller's context (a client that is cut: cancel it).
func (f *pendingFixture) doCtx(ctx context.Context, t *testing.T, method, target string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	if body != nil {
		r.ContentLength = int64(len(body))
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r = r.WithContext(s3Ctx(ctx, f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	return w
}

func (f *pendingFixture) getObj(t *testing.T, key string) []byte {
	t.Helper()
	w := f.do(t, "GET", "/test-bucket/"+key, nil, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return w.Body.Bytes()
}

func randomBody(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

// waitFor polls cond for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// chunkBlobs lists the blobs under the fixed driver's chunk container.
func (f *pendingFixture) chunkBlobs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, b := range f.fixed.blobs() {
		if strings.Contains(b, "/"+chunkContainer+"/") {
			out = append(out, b)
		}
	}
	return out
}

func TestChunkStore_NoConnectionHeldAcrossTheBackendPut(t *testing.T) {
	// Arrange: a chunked PUT whose chunk stores all wait on the backend.
	f := setupPendingFixture(t)
	body := randomBody(t, 8<<20)
	done := make(chan int, 1)
	go func() {
		w := f.do(t, "PUT", "/test-bucket/slow.bin", body, nil)
		done <- w.Code
	}()
	waitFor(t, "two chunk PUTs in flight", func() bool { return f.gated.inFlight.Load() >= 2 })
	// Every chunk the body has reaches the gate: the in-flight count stops
	// growing (a neighbouring chunk's claim is a millisecond transaction —
	// a single sample on a slow runner caught one, CI 2026-10-18).
	stable, last := time.Now(), f.gated.inFlight.Load()
	for time.Since(stable) < 300*time.Millisecond {
		time.Sleep(20 * time.Millisecond)
		if n := f.gated.inFlight.Load(); n != last {
			stable, last = time.Now(), n
		}
	}

	// Act: the fewest pooled connections in use while the PUTs wait. Before
	// 093 every waiting store held one for its whole PUT, so it never fell
	// below the number of PUTs in flight.
	minInUse := f.db.Stats().InUse
	for deadline := time.Now().Add(2 * time.Second); minInUse > 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		minInUse = min(minInUse, f.db.Stats().InUse)
	}
	inFlight := f.gated.inFlight.Load()

	// Assert: no pooled connection waits on the backend.
	close(f.gated.gate)
	require.Equal(t, http.StatusOK, <-done)
	assert.Equal(t, 0, minInUse, "%d pooled connections held throughout while %d chunk PUTs waited on the backend", minInUse, inFlight)
	assert.Equal(t, body, f.getObj(t, "slow.bin"))
	var pending int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index g
		JOIN tenant_chunk_refs r ON r.dedup_scope = g.dedup_scope AND r.plaintext_hash = g.plaintext_hash
		WHERE r.tenant_id = $1 AND g.state <> 'ready'`, f.tenantID).Scan(&pending))
	assert.Zero(t, pending, "every chunk a manifest names is ready")
}

func TestChunkStore_ConcurrentFirstStoresWithDifferentEncodings_OneBlob(t *testing.T) {
	// Arrange (R8-02): the same new chunks from two requests whose
	// Content-Types decide compression differently, both in flight at once.
	f := setupPendingFixture(t)
	text := bytes.Repeat([]byte("compressible chunk text, the same in both objects. "), (3<<20)/51)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, ct := range []string{"text/plain", "image/jpeg"} {
		wg.Add(1)
		go func(i int, ct string) {
			defer wg.Done()
			codes[i] = f.do(t, "PUT", "/test-bucket/obj-"+string(rune('a'+i)), text, map[string]string{"Content-Type": ct}).Code
		}(i, ct)
		if i == 0 {
			waitFor(t, "the first writer's PUT", func() bool { return f.gated.inFlight.Load() >= 1 })
		}
	}
	time.Sleep(200 * time.Millisecond) // the second writer reaches the chunk

	// Act
	close(f.gated.gate)
	wg.Wait()

	// Assert
	require.Equal(t, []int{http.StatusOK, http.StatusOK}, codes)
	assert.Equal(t, text, f.getObj(t, "obj-a"))
	assert.Equal(t, text, f.getObj(t, "obj-b"))
	var rows, blobsNeeded int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(DISTINCT (g.dedup_scope, g.plaintext_hash)), COALESCE(SUM(DISTINCT g.ref_count), 0)
		FROM global_content_index g JOIN tenant_chunk_refs r ON r.dedup_scope = g.dedup_scope AND r.plaintext_hash = g.plaintext_hash
		WHERE r.tenant_id = $1`, f.tenantID).Scan(&rows, &blobsNeeded))
	assert.Equal(t, rows, len(f.chunkBlobs(t)), "one blob per chunk")
}

func TestChunkStore_AWriterThatDiedLeavesARowTheNextWriterTakesOver(t *testing.T) {
	// Arrange: a pending claim of the object's only chunk whose writer died
	// (no blob, the row never flipped), recent enough that the next writer
	// first waits on it.
	f := setupPendingFixture(t)
	close(f.gated.gate)
	f.server.chunkPendingTakeover = 300 * time.Millisecond
	body := randomBody(t, 512<<10) // one chunk (under the 1 MiB minimum)
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	_, err := f.db.Exec(`INSERT INTO global_content_index
		(dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, ref_count, state, pending_since)
		VALUES ($1, $2, 'fixed', $3, $4, 0, 'pending', NOW())`,
		crypto.GlobalDedupScope, hash, "_chunks/"+hash, len(body))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`, crypto.GlobalDedupScope, hash)
	})

	// Act
	start := time.Now()
	w := f.do(t, "PUT", "/test-bucket/after-crash.bin", body, nil)

	// Assert: the claim was taken over after its window; the object reads back.
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond, "a young claim is waited on, not overwritten")
	assert.Equal(t, body, f.getObj(t, "after-crash.bin"))
	var state string
	var refs int
	require.NoError(t, f.db.QueryRow(`SELECT state, ref_count FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`,
		crypto.GlobalDedupScope, hash).Scan(&state, &refs))
	assert.Equal(t, "ready", state)
	assert.Equal(t, 1, refs)
}

func TestChunkStore_AFailedPutDropsItsClaim(t *testing.T) {
	// Arrange: the backend refuses every chunk PUT.
	f := setupPendingFixture(t)
	close(f.gated.gate)
	f.fixed.failPut.Store(true)
	body := randomBody(t, 512<<10)
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])

	// Act
	w := f.do(t, "PUT", "/test-bucket/refused.bin", body, nil)

	// Assert: no row is left for the next writer to wait on.
	assert.NotEqual(t, http.StatusOK, w.Code)
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`,
		crypto.GlobalDedupScope, hash).Scan(&n))
	assert.Zero(t, n)

	// The next writer stores it.
	f.fixed.failPut.Store(false)
	require.Equal(t, http.StatusOK, f.do(t, "PUT", "/test-bucket/refused.bin", body, nil).Code)
	assert.Equal(t, body, f.getObj(t, "refused.bin"))
}

func TestChunkReaders_NeverSeeAPendingRow(t *testing.T) {
	// Arrange: a pending row (its blob not yet there) in a scope and on a
	// backend name of this test's own, old and marked as dedup GC marks a
	// ref-0 row, sealed under key version 1 of a tenant now at version 2.
	f := setupPendingFixture(t)
	close(f.gated.gate)
	ctx := context.Background()
	scope := "b1-" + uuid.NewString()[:8]
	backend := "b1fixed-" + uuid.NewString()[:6]
	hash := strings.Repeat("ab", 32)
	_, err := f.db.Exec(`INSERT INTO global_content_index
		(dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, ref_count, state, pending_since,
		 marked_for_deletion, marked_at, last_accessed_at, encrypted, encryption_key_version)
		VALUES ($1, $2, $3, $4, 10, 0, 'pending', NOW(), TRUE, NOW() - INTERVAL '30 days', NOW() - INTERVAL '30 days', TRUE, 1)`,
		scope, hash, backend, "_chunks/"+scope+"/"+hash)
	require.NoError(t, err)
	for v := 1; v <= 2; v++ {
		_, err = f.db.Exec(`INSERT INTO tenant_key_versions (tenant_id, key_version, chunk_key_wrapped, kem_seed_wrapped, kem_public_key)
			VALUES ($1, $2, '\x00', '\x00', '\x00')`, scope, v)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM global_content_index WHERE dedup_scope = $1`, scope)
		_, _ = f.db.Exec(`DELETE FROM tenant_key_versions WHERE tenant_id = $1`, scope)
	})

	// Act + Assert: the dedup lookup and the GET plan.
	gci := crypto.NewGlobalContentIndex(f.db)
	look, err := gci.LookupChunk(ctx, scope, hash)
	require.NoError(t, err)
	assert.True(t, look.IsNewChunk, "a dedup hit on a pending row references a blob that is not there")
	batch, err := gci.LookupChunks(ctx, scope, []string{hash})
	require.NoError(t, err)
	assert.False(t, batch[hash].Exists, "the GET plan resolved a pending row")
	// IncrementRef's fast path.
	n, err := gci.IncrementRef(ctx, scope, hash)
	require.NoError(t, err)
	assert.Zero(t, n, "a reference taken on a pending row")

	// Dedup GC, on an engine that knows only this test's backend: the
	// pending row is never swept (its blob is the writer's, landing).
	eng := engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: backend})
	require.NoError(t, eng.AddDriver(backend, &fixedBucketDriver{dir: t.TempDir()}))
	runner := NewDedupGCRunner(f.db, eng, gci, zap.NewNop())
	runner.GracePeriod = 7 * 24 * time.Hour
	_, err = runner.RunOnce(ctx)
	require.NoError(t, err)
	var state string
	require.NoError(t, f.db.QueryRow(`SELECT state FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`, scope, hash).Scan(&state))
	assert.Equal(t, "pending", state, "dedup GC deleted a pending row")

	// Key re-encrypt: not a candidate.
	k := &KeyReencryptRunner{db: f.db, onlyScope: scope}
	cands, err := k.candidates(ctx)
	require.NoError(t, err)
	assert.Empty(t, cands, "key re-encrypt picked a pending row")

	// The chunk move: not a row to move.
	r := &chunkMoveRun{m: &ChunkMover{db: f.db, BatchSize: 100}, opts: ChunkMoveOptions{Backend: backend}}
	rows, err := r.rowBatch(ctx, "", "")
	require.NoError(t, err)
	assert.Empty(t, rows, "the chunk move picked a pending row")
}

func TestChunkStore_AClaimTakenOverWhileItsPutRanReferencesTheWinner(t *testing.T) {
	// Arrange: this writer's claim, then — while its PUT runs — another
	// writer takes the claim over, stores and makes the row ready.
	f := setupPendingFixture(t)
	close(f.gated.gate)
	ctx := context.Background()
	a := f.server.writeAdapter()
	scope := "b1t-" + uuid.NewString()[:8]
	hash := strings.Repeat("cd", 32)
	key := "_chunks/" + scope + "/" + hash
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM global_content_index WHERE dedup_scope = $1`, scope)
	})
	entry := &crypto.GCIEntry{DedupScope: scope, PlaintextHash: hash, StorageKey: key, SizeBytes: 10}
	_, kind, since, err := a.claimChunk(ctx, scope, key, entry)
	require.NoError(t, err)
	require.Equal(t, chunkClaimed, kind)
	_, err = f.db.Exec(`UPDATE global_content_index SET state = 'ready', pending_since = NULL, ref_count = 1
		WHERE dedup_scope = $1 AND plaintext_hash = $2`, scope, hash)
	require.NoError(t, err)

	// Act: this writer's PUT ends; its flip finds the claim gone.
	_, done, err := a.readyChunk(ctx, scope, key, "fixed", since, entry, true)
	require.NoError(t, err)
	res, kind, _, err := a.claimChunk(ctx, scope, key, entry)

	// Assert: no second reference from the flip; the loop references the winner.
	require.NoError(t, err)
	assert.False(t, done, "a taken-over claim does not flip")
	assert.Equal(t, chunkReferenced, kind)
	assert.True(t, res.reused)
	var refs int
	require.NoError(t, f.db.QueryRow(`SELECT ref_count FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`, scope, hash).Scan(&refs))
	assert.Equal(t, 2, refs, "the winner's reference and this writer's")
}
