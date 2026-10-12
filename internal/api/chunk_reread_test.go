package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R8-1 follow-up A2 (Stage 1a Prompt 2A): a chunked GET survives a
// re-encrypt (and a chunk move) that outlives its plan. The plan is resolved
// once; a chunk whose blob moved and whose old address was deleted after
// its grace used to cut the body at that chunk.

// serveChunkedGet is the adapter's GET on a real HTTP server: a recorder
// cannot show a cut.
func serveChunkedGet(t *testing.T, f *adapterTestFixture, key string) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(s3Ctx(r.Context(), f.tenant))
		f.adapter.HandleGet(w, r, "test-bucket", key)
	}))
	ts.Config.ErrorLog = nil
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

type getResult struct {
	status int
	body   []byte
	err    error
}

func startGet(t *testing.T, ts *httptest.Server, key string) <-chan getResult {
	t.Helper()
	out := make(chan getResult, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/test-bucket/"+key, nil)
		if err != nil {
			out <- getResult{err: err}
			return
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			out <- getResult{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, readErr := io.ReadAll(resp.Body)
		out <- getResult{status: resp.StatusCode, body: body, err: readErr}
	}()
	return out
}

func TestChunkedGet_ReencryptSwapAndRetiredDelete_DuringTheStream_BodyCompletes(t *testing.T) {
	f, keys := setupKeyedChunkingFixture(t)
	// No hedge: a hedged second fetch of the paused chunk (the hook holds
	// only the first) would read the old blob before the delete whenever the
	// swap outlasts CHUNK_GET_HEDGE_AFTER (CI), and the read under test is
	// the one that finds the address gone. A hedge goes through the same
	// fetchAndVerifyChunk.
	f.adapter.largeGet.chunkHedgeAfter = 0
	content, chunks := putMultiChunk(t, f, "long.bin")
	second := chunks[1]

	// The reader is paused between chunk 1 and chunk 2: the read of chunk
	// 2's resolved address waits until the swap AND the delete are done.
	paused, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	f.fixed.onGet = func(_, artifact string) {
		if artifact == second.storageKey && held.CompareAndSwap(false, true) {
			close(paused)
			<-release
		}
	}
	ts := serveChunkedGet(t, f, "long.bin")
	movedBefore := testutil.ToFloat64(chunkedGetRereads.WithLabelValues(rereadMoved))
	cutBefore := testutil.ToFloat64(chunkedGetTruncated.WithLabelValues(truncatedFetch))

	res := startGet(t, ts, "long.bin")
	select {
	case <-paused:
	case <-time.After(10 * time.Second):
		t.Fatal("the GET never reached chunk 2")
	}

	// Rotate, run the re-encrypt job (the swap), then a run past the grace
	// (the old addresses deleted) — all while the GET is still on its plan.
	_, err := keys.Rotate(context.Background(), f.tenantID)
	require.NoError(t, err)
	k := newKeyReencryptForTest(t, f)
	clock := time.Now()
	k.now = func() time.Time { return clock }
	swap, err := k.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, len(chunks), swap.Reencrypted, "every chunk swapped to v2")
	clock = clock.Add(k.RetireAfter + time.Minute)
	del, err := k.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, len(chunks), del.RetiredDeleted, "every old address deleted")
	_, statErr := os.Stat(f.fixed.path(engine.ChunkAddressTenant, chunkContainer, second.storageKey))
	require.True(t, os.IsNotExist(statErr), "chunk 2's resolved address is gone")

	close(release)
	var got getResult
	select {
	case got = <-res:
	case <-time.After(30 * time.Second):
		t.Fatal("the GET never finished")
	}

	t.Logf("GET across rotate + swap + retired delete: status %d, %d of %d bytes, read error %v", got.status, len(got.body), len(content), got.err)
	require.Equal(t, http.StatusOK, got.status)
	require.NoError(t, got.err, "the body completes — before: io.ErrUnexpectedEOF")
	assert.Equal(t, len(content), len(got.body))
	assert.True(t, string(content) == string(got.body), "exact bytes")
	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(chunkedGetRereads.WithLabelValues(rereadMoved)) >= movedBefore+1
	}, 2*time.Second, 10*time.Millisecond, "the re-read is counted as moved")
	assert.Equal(t, cutBefore, testutil.ToFloat64(chunkedGetTruncated.WithLabelValues(truncatedFetch)), "no cut")
}

// A chunk whose blob is really gone (its row unchanged) is still a cut —
// the re-read never turns a miss into a success it cannot back.
func TestChunkedGet_RealMiss_AfterTheReread_IsStillACut(t *testing.T) {
	f, _ := setupKeyedChunkingFixture(t)
	_, chunks := putMultiChunk(t, f, "gone.bin")
	last := chunks[len(chunks)-1]
	require.NoError(t, os.Remove(f.fixed.path(engine.ChunkAddressTenant, chunkContainer, last.storageKey)))
	ts := serveChunkedGet(t, f, "gone.bin")
	missingBefore := testutil.ToFloat64(chunkedGetRereads.WithLabelValues(rereadMissing))

	got := <-startGet(t, ts, "gone.bin")

	t.Logf("GET with its last chunk's blob deleted: status %d, %d bytes, read error %v", got.status, len(got.body), got.err)
	require.Equal(t, http.StatusOK, got.status, "the status line went out with chunk 1")
	require.Error(t, got.err)
	assert.True(t, errors.Is(got.err, io.ErrUnexpectedEOF) || isStreamReset(got.err), "a cut: %v", got.err)
	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(chunkedGetRereads.WithLabelValues(rereadMissing)) >= missingBefore+1
	}, 2*time.Second, 10*time.Millisecond, "the re-read is counted as missing")
}
