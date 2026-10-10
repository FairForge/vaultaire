package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/parfetch"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hangEngine makes the first fetch of chosen chunk keys hang until its
// context ends — a backend's tail latency, which only a hedge gets past.
type hangEngine struct {
	engine.Engine
	hang     map[string]bool
	mu       sync.Mutex
	attempts map[string]int
	gets     atomic.Int32
	cur, max atomic.Int32
}

func (e *hangEngine) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	if container != chunkContainer || !strings.HasPrefix(artifact, "_chunks/") {
		return e.Engine.Get(ctx, container, artifact)
	}
	e.gets.Add(1)
	c := e.cur.Add(1)
	defer e.cur.Add(-1)
	for {
		m := e.max.Load()
		if c <= m || e.max.CompareAndSwap(m, c) {
			break
		}
	}
	e.mu.Lock()
	e.attempts[artifact]++
	n := e.attempts[artifact]
	e.mu.Unlock()
	if e.hang[artifact] && n == 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	time.Sleep(2 * time.Millisecond)
	return e.Engine.Get(ctx, container, artifact)
}

func chunkKeys(t *testing.T, f *adapterTestFixture, key string) []string {
	t.Helper()
	rows, err := f.db.Query(`SELECT plaintext_hash FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND object_key = $2 ORDER BY chunk_index`, f.tenantID, key)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var h string
		require.NoError(t, rows.Scan(&h))
		out = append(out, "_chunks/"+h)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestChunkedGet_SlowChunkIsHedged(t *testing.T) {
	// Arrange
	f := setupChunkingFixture(t)
	content := generateTestData(24 << 20)
	putChunkedObject(t, f, "hedge.bin", content, "application/octet-stream")
	keys := chunkKeys(t, f, "hedge.bin")
	require.GreaterOrEqual(t, len(keys), 3)
	he := &hangEngine{Engine: f.eng, hang: map[string]bool{keys[1]: true}, attempts: map[string]int{}}
	f.adapter.engine = he
	f.adapter.largeGet.chunkHedgeAfter = 50 * time.Millisecond
	hedges := testutil.ToFloat64(largeGetHedges.WithLabelValues("chunked", "slow"))

	// Act
	start := time.Now()
	w := getChunked(t, f, "hedge.bin")

	// Assert
	require.Equal(t, http.StatusOK, w.Code)
	body, _ := io.ReadAll(w.Body)
	require.True(t, bytes.Equal(content, body), "hedged chunks still stream in exact order")
	assert.Less(t, time.Since(start), 5*time.Second, "the hung fetch did not hold the stream")
	assert.GreaterOrEqual(t, testutil.ToFloat64(largeGetHedges.WithLabelValues("chunked", "slow")), hedges+1)
}

func TestChunkedGet_PrefetchCapsChunksInFlight(t *testing.T) {
	f := setupChunkingFixture(t)
	content := generateTestData(48 << 20)
	putChunkedObject(t, f, "cap.bin", content, "application/octet-stream")
	he := &hangEngine{Engine: f.eng, hang: map[string]bool{}, attempts: map[string]int{}}
	f.adapter.engine = he
	f.adapter.chunkGetPrefetch = 3
	f.adapter.largeGet.chunkHedgeAfter = 0 // no hedges: count only the window

	w := getChunked(t, f, "cap.bin")

	require.Equal(t, http.StatusOK, w.Code)
	body, _ := io.ReadAll(w.Body)
	require.True(t, bytes.Equal(content, body))
	assert.LessOrEqual(t, he.max.Load(), int32(3), "CHUNK_GET_PREFETCH caps the chunks in flight")
	assert.GreaterOrEqual(t, he.max.Load(), int32(2))
}

func TestChunkedGet_WindowBytesBoundTheReadAhead(t *testing.T) {
	// A window of one byte admits one chunk at a time (a part larger than
	// the window still runs alone): no overlap at all.
	f := setupChunkingFixture(t)
	content := generateTestData(16 << 20)
	putChunkedObject(t, f, "win.bin", content, "application/octet-stream")
	he := &hangEngine{Engine: f.eng, hang: map[string]bool{}, attempts: map[string]int{}}
	f.adapter.engine = he
	f.adapter.largeGet.chunkWindowBytes = 1
	f.adapter.largeGet.chunkHedgeAfter = 0

	w := getChunked(t, f, "win.bin")

	require.Equal(t, http.StatusOK, w.Code)
	body, _ := io.ReadAll(w.Body)
	require.True(t, bytes.Equal(content, body))
	assert.Equal(t, int32(1), he.max.Load())
}

func TestChunkedGet_SharedBudgetExhaustedKeepsTheFloor(t *testing.T) {
	f := setupChunkingFixture(t)
	content := generateTestData(32 << 20)
	putChunkedObject(t, f, "budget.bin", content, "application/octet-stream")
	he := &hangEngine{Engine: f.eng, hang: map[string]bool{}, attempts: map[string]int{}}
	f.adapter.engine = he
	f.adapter.largeGet.chunkHedgeAfter = 0
	b := parfetch.NewBudget(1 << 30)
	require.True(t, b.TryAcquire(1<<30)) // every byte taken by other streams
	f.adapter.largeGet.budget = b

	w := getChunked(t, f, "budget.bin")

	require.Equal(t, http.StatusOK, w.Code)
	body, _ := io.ReadAll(w.Body)
	require.True(t, bytes.Equal(content, body), "an exhausted budget slows a chunked GET, never fails it")
	assert.LessOrEqual(t, he.max.Load(), int32(chunkGetFreeParts), "the pre-budget depth is the floor")
	assert.Equal(t, int64(1<<30), b.InUse())
}

func TestChunkedGet_ClientGoneMidStreamLeaksNothing(t *testing.T) {
	f := setupChunkingFixture(t)
	content := generateTestData(32 << 20)
	putChunkedObject(t, f, "gone.bin", content, "application/octet-stream")
	keys := chunkKeys(t, f, "gone.bin")
	hang := map[string]bool{}
	for _, k := range keys[1:] {
		hang[k] = true
	}
	he := &hangEngine{Engine: f.eng, hang: hang, attempts: map[string]int{}}
	f.adapter.engine = he
	f.adapter.largeGet.chunkHedgeAfter = 20 * time.Millisecond
	runtime.GC()
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(s3Ctx(context.Background(), f.tenant))
	req := httptest.NewRequest("GET", "/test-bucket/gone.bin", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	f.adapter.HandleGet(rec, req, "test-bucket", "gone.bin")

	assert.Less(t, rec.Body.Len(), len(content))
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+1 && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+1 {
		buf := make([]byte, 1<<20)
		k := runtime.Stack(buf, true)
		t.Fatalf("goroutines %d -> %d:\n%s", before, n, buf[:k])
	}
}

func TestResolveChunkDescs_OneBatchPerScope(t *testing.T) {
	f := setupChunkingFixture(t)
	content := generateTestData(24 << 20)
	putChunkedObject(t, f, "batch.bin", content, "application/octet-stream")
	refs, err := f.adapter.gci.GetObjectChunks(context.Background(), f.tenantID, "test-bucket", "batch.bin")
	require.NoError(t, err)

	descs, err := f.adapter.resolveChunkDescs(context.Background(), refs)
	require.NoError(t, err)
	require.Len(t, descs, len(refs))
	var off int64
	for i, d := range descs {
		assert.Equal(t, refs[i].PlaintextHash, d.plaintextHash)
		assert.Equal(t, off, d.offset, "chunk %d", i)
		off += d.size
	}
	assert.Equal(t, int64(len(content)), off)

	// A hash the index does not have makes the manifest unresolvable.
	missing := append([]crypto.TenantChunkRef(nil), refs...)
	missing[1].PlaintextHash = strings.Repeat("ab", 32)
	_, err = f.adapter.resolveChunkDescs(context.Background(), missing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing from index")
}
