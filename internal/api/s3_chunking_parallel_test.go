package api

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- S3 ETags are MD5 by protocol
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// instrumentedEngine wraps the fixture engine to observe chunk-store Puts:
// it tracks the maximum number of concurrent Puts, injects latency so
// overlap is observable, and can inject failures after N successful stores.
// Non-chunk traffic passes through untouched.
type instrumentedEngine struct {
	engine.Engine
	delay     time.Duration
	failAfter int64 // fail chunk Puts once count exceeds this; -1 = never
	puts      int64
	cur       int64
	max       int64

	mu         sync.Mutex
	storedKeys []string
}

func (e *instrumentedEngine) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) (string, error) {
	if container != chunkContainer || !strings.HasPrefix(artifact, "_chunks/") {
		return e.Engine.Put(ctx, container, artifact, data, opts...)
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
	n := atomic.AddInt64(&e.puts, 1)
	if e.failAfter >= 0 && n > e.failAfter {
		return "", fmt.Errorf("injected chunk store failure (put %d)", n)
	}
	bn, err := e.Engine.Put(ctx, container, artifact, data, opts...)
	if err == nil {
		e.mu.Lock()
		e.storedKeys = append(e.storedKeys, artifact)
		e.mu.Unlock()
	}
	return bn, err
}

func getChunked(t *testing.T, f *adapterTestFixture, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/test-bucket/"+key, nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", key)
	return w
}

// TestChunkedPut_ParallelChunkStores proves chunk stores overlap (the
// sequential loop capped engine-path uploads at chunk_size ÷ backend RTT)
// and that an out-of-order store still installs a byte-correct manifest.
func TestChunkedPut_ParallelChunkStores(t *testing.T) {
	f := setupChunkingFixture(t)
	ie := &instrumentedEngine{Engine: f.eng, delay: 20 * time.Millisecond, failAfter: -1}
	f.adapter.engine = ie

	content := generateTestData(48 << 20) // ~12 chunks at the 4 MB average
	etag := putChunkedObject(t, f, "parallel.bin", content, "application/octet-stream")

	sum := md5.Sum(content) // #nosec G401 -- S3 ETag semantics
	assert.Equal(t, fmt.Sprintf("%q", hex.EncodeToString(sum[:])), etag,
		"ETag must be the plaintext MD5 regardless of store order")

	assert.GreaterOrEqual(t, atomic.LoadInt64(&ie.max), int64(2),
		"chunk stores must run concurrently, not one at a time")

	w := getChunked(t, f, "parallel.bin")
	require.Equal(t, http.StatusOK, w.Code)
	body, _ := io.ReadAll(w.Body)
	assert.True(t, bytes.Equal(content, body),
		"manifest must reassemble in chunk-index order even when stores complete out of order")
}

// TestChunkedPut_DuplicateChunksSingleStore uploads content whose chunks are
// all identical (constant bytes never trigger a FastCDC boundary, so the
// chunker emits max-size chunks with one shared hash). Concurrent workers
// must not race duplicate hashes into double stores: the first occurrence
// stores, later occurrences wait and take dedup references.
func TestChunkedPut_DuplicateChunksSingleStore(t *testing.T) {
	f := setupChunkingFixture(t)
	ie := &instrumentedEngine{Engine: f.eng, delay: 30 * time.Millisecond, failAfter: -1}
	f.adapter.engine = ie

	content := bytes.Repeat([]byte{0xAB}, 64<<20) // 4 identical 16 MB chunks
	putChunkedObject(t, f, "dup.bin", content, "application/octet-stream")

	assert.LessOrEqual(t, len(ie.storedKeys), 1,
		"identical in-flight chunks must be stored at most once")

	var refRows, distinctHashes int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*), COUNT(DISTINCT plaintext_hash) FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND object_key = 'dup.bin'`, f.tenantID).
		Scan(&refRows, &distinctHashes))
	assert.Equal(t, 4, refRows, "every occurrence needs its own manifest ref")
	assert.Equal(t, 1, distinctHashes)

	w := getChunked(t, f, "dup.bin")
	require.Equal(t, http.StatusOK, w.Code)
	body, _ := io.ReadAll(w.Body)
	assert.True(t, bytes.Equal(content, body))
}

// TestChunkedPut_WorkerFailureReleasesRefs injects a store failure after one
// success: the PUT must 5xx, install nothing, and compensate every reference
// taken by the chunks that did succeed (F10 semantics under concurrency).
func TestChunkedPut_WorkerFailureReleasesRefs(t *testing.T) {
	f := setupChunkingFixture(t)
	ie := &instrumentedEngine{Engine: f.eng, delay: 5 * time.Millisecond, failAfter: 1}
	f.adapter.engine = ie

	content := generateTestData(48 << 20)
	req := httptest.NewRequest("PUT", "/test-bucket/fail.bin", bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "fail.bin")
	require.GreaterOrEqual(t, w.Code, 500, "failed chunk store must fail the PUT")

	var headRows int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'fail.bin'`,
		f.tenantID).Scan(&headRows))
	assert.Zero(t, headRows, "no head row may survive a failed chunked PUT")

	var refRows int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND object_key = 'fail.bin'`, f.tenantID).Scan(&refRows))
	assert.Zero(t, refRows, "no manifest rows may survive a failed chunked PUT")

	// Chunks that stored successfully before the failure must have had their
	// references compensated back to zero (rows may also be absent if the
	// store transaction rolled back after the blob write).
	ie.mu.Lock()
	stored := append([]string(nil), ie.storedKeys...)
	ie.mu.Unlock()
	require.NotEmpty(t, stored, "test needs at least one successful store to be meaningful")
	for _, key := range stored {
		hash := strings.TrimPrefix(key, "_chunks/")
		var refCount int
		err := f.db.QueryRow(`
			SELECT ref_count FROM global_content_index
			WHERE dedup_scope = $1 AND plaintext_hash = $2`,
			"_global", hash).Scan(&refCount)
		if err == sql.ErrNoRows {
			continue
		}
		require.NoError(t, err)
		assert.Zero(t, refCount, "aborted PUT must release the ref on stored chunk %s", hash[:16])
	}
}
