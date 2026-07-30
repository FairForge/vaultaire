package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getInstrumentedEngine wraps the fixture engine to observe chunk fetches on
// GET: concurrency high-water mark, injected latency so overlap is
// observable, and injected failures after N successful fetches.
type getInstrumentedEngine struct {
	engine.Engine
	delay        time.Duration
	failGetAfter int64           // fail chunk Gets once count exceeds this; -1 = never
	allowKeys    map[string]bool // when set, chunk Gets for keys NOT in here fail
	gets         int64
	cur          int64
	max          int64
}

func (e *getInstrumentedEngine) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	if container != chunkContainer || !strings.HasPrefix(artifact, "_chunks/") {
		return e.Engine.Get(ctx, container, artifact)
	}
	cur := atomic.AddInt64(&e.cur, 1)
	defer atomic.AddInt64(&e.cur, -1)
	for {
		old := atomic.LoadInt64(&e.max)
		if cur <= old || atomic.CompareAndSwapInt64(&e.max, old, cur) {
			break
		}
	}
	if e.delay > 0 {
		time.Sleep(e.delay)
	}
	if e.allowKeys != nil && !e.allowKeys[artifact] {
		return nil, fmt.Errorf("injected chunk fetch failure for %s", artifact)
	}
	n := atomic.AddInt64(&e.gets, 1)
	if e.failGetAfter >= 0 && n > e.failGetAfter {
		return nil, fmt.Errorf("injected chunk fetch failure (get %d)", n)
	}
	return e.Engine.Get(ctx, container, artifact)
}

// TestChunkedGet_PrefetchesConcurrently proves chunk fetches overlap — the
// sequential loop paid one full backend round-trip per chunk, the same shape
// as the upload ceiling fixed by the parallel chunk-store pool.
func TestChunkedGet_PrefetchesConcurrently(t *testing.T) {
	f := setupChunkingFixture(t)
	content := generateTestData(48 << 20) // ~12 chunks at the 4 MB average
	putChunkedObject(t, f, "prefetch.bin", content, "application/octet-stream")

	ie := &getInstrumentedEngine{Engine: f.eng, delay: 20 * time.Millisecond, failGetAfter: -1}
	f.adapter.engine = ie

	w := getChunked(t, f, "prefetch.bin")
	require.Equal(t, http.StatusOK, w.Code)
	body, _ := io.ReadAll(w.Body)
	require.True(t, bytes.Equal(content, body),
		"prefetched chunks must still stream in exact chunk-index order")

	assert.GreaterOrEqual(t, atomic.LoadInt64(&ie.max), int64(2),
		"chunk fetches must overlap, not run one at a time")
}

// TestChunkedGet_PrefetchRangeRequest: a range spanning chunk boundaries must
// return the exact slice with prefetch in play.
func TestChunkedGet_PrefetchRangeRequest(t *testing.T) {
	f := setupChunkingFixture(t)
	content := generateTestData(24 << 20)
	putChunkedObject(t, f, "prefetch-range.bin", content, "application/octet-stream")

	ie := &getInstrumentedEngine{Engine: f.eng, delay: 10 * time.Millisecond, failGetAfter: -1}
	f.adapter.engine = ie

	// Span from inside the first chunk deep into the object (crosses at least
	// one boundary at the 1 MB minimum chunk size).
	start, end := int64(512*1024), int64(8<<20)
	req := httptest.NewRequest("GET", "/test-bucket/prefetch-range.bin", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", "prefetch-range.bin")

	require.Equal(t, http.StatusPartialContent, w.Code)
	body, _ := io.ReadAll(w.Body)
	assert.True(t, bytes.Equal(content[start:end+1], body), "range slice must be exact")
}

// TestChunkedGet_MidStreamFailureAborts: a fetch failure after the first
// chunk has streamed must abort the body in order — everything before the
// failed chunk written, nothing after, never scrambled. Only chunk 0's
// storage key is allowed to succeed (deterministic under prefetch, where
// fetch completion order is racy).
func TestChunkedGet_MidStreamFailureAborts(t *testing.T) {
	f := setupChunkingFixture(t)
	content := generateTestData(48 << 20)
	putChunkedObject(t, f, "abort.bin", content, "application/octet-stream")

	var firstHash string
	require.NoError(t, f.db.QueryRow(`
		SELECT plaintext_hash FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND object_key = 'abort.bin' AND chunk_index = 0`,
		f.tenantID).Scan(&firstHash))

	ie := &getInstrumentedEngine{Engine: f.eng, failGetAfter: -1,
		allowKeys: map[string]bool{"_chunks/" + firstHash: true}}
	f.adapter.engine = ie

	w := getChunked(t, f, "abort.bin")
	require.Equal(t, http.StatusOK, w.Code, "status was already committed when the failure hit")
	body, _ := io.ReadAll(w.Body)
	require.Less(t, len(body), len(content), "body must be truncated, not complete")
	require.NotEmpty(t, body, "the successfully fetched first chunk must have streamed")
	assert.True(t, bytes.Equal(content[:len(body)], body),
		"aborted body must be an exact prefix — out-of-order writes would corrupt it")
}

// TestChunkedGet_FirstChunkFailureCleanError: when the very first fetch
// fails, no bytes and no 200 may be committed.
func TestChunkedGet_FirstChunkFailureCleanError(t *testing.T) {
	f := setupChunkingFixture(t)
	content := generateTestData(24 << 20)
	putChunkedObject(t, f, "firstfail.bin", content, "application/octet-stream")

	ie := &getInstrumentedEngine{Engine: f.eng, failGetAfter: 0}
	f.adapter.engine = ie

	w := getChunked(t, f, "firstfail.bin")
	assert.GreaterOrEqual(t, w.Code, 500,
		"first-chunk failure must be a clean 5xx, never a partial 200 or a fallthrough")
}
