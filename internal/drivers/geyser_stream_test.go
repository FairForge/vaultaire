package drivers

// WP-R7-2 (Stage 1a Prompt 14 Part B): GeyserDriver.Put materialised every
// body — into RAM up to 64 MiB, else a temp file — to learn its length for
// Vail's mandatory Content-Length, although every Vault write (a PUT, a
// Complete's whole branch, the parity shards, a Smart demotion's copy to
// tape, the re-encrypt, generated objects, the canary) passes
// engine.WithContentLength. N concurrent Vault PUTs held N × 64 MiB. Now a
// Put with a known length streams the reader through in one attempt;
// materialize stays for the unknown-length case and is counted.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// discardS3 answers every PUT 200 after discarding its body — it keeps no
// copy, so the process's allocations are the driver's and the SDK's.
type discardS3 struct {
	puts      atomic.Int32
	bytes     atomic.Int64
	declared  atomic.Int64 // the last PUT's Content-Length header
	chunked   atomic.Bool  // the last PUT came with Transfer-Encoding chunked / aws-chunked
	sha256Hdr atomic.Pointer[string]
}

func (f *discardS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusOK)
		return
	}
	n, _ := io.Copy(io.Discard, r.Body)
	f.puts.Add(1)
	f.bytes.Store(n)
	f.declared.Store(r.ContentLength)
	f.chunked.Store(len(r.TransferEncoding) > 0 || r.Header.Get("Content-Encoding") == "aws-chunked")
	h := r.Header.Get("X-Amz-Content-Sha256")
	f.sha256Hdr.Store(&h)
	w.Header().Set("ETag", `"etag"`)
	w.WriteHeader(http.StatusOK)
}

// onceReader hides the Seeker of its source and counts what was read.
type onceReader struct {
	r        io.Reader
	n        int64
	maxRead  int
	afterEOF int
	eof      bool
}

func (o *onceReader) Read(p []byte) (int, error) {
	if o.eof {
		o.afterEOF++
	}
	n, err := o.r.Read(p)
	o.n += int64(n)
	if n > o.maxRead {
		o.maxRead = n
	}
	if err == io.EOF {
		o.eof = true
	}
	return n, err
}

func newGeyserAgainst(t *testing.T, h http.Handler) *GeyserDriver {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d, err := NewGeyserDriver("ak", "sk", "tape", "tenant-x", zap.NewNop(), WithGeyserEndpoint(srv.URL))
	require.NoError(t, err)
	return d
}

func totalAlloc() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

func TestGeyserPut_AKnownLengthStreamsTheBodyThroughOnce(t *testing.T) {
	// Arrange: a 65 MiB body (over the 64 MiB RAM threshold: materialize
	// would spill it to a temp file) behind a reader that cannot be
	// re-read; TMPDIR is this test's own, empty directory.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	f := &discardS3{}
	d := newGeyserAgainst(t, f)
	body := randomBody(t, 65<<20)
	src := &onceReader{r: bytes.NewReader(body)}
	before := totalAlloc()

	// Act
	err := d.Put(tctx(), "c", "stream.bin", src, engine.WithContentLength(int64(len(body))))

	// Assert: read once, exactly its length, framed by Content-Length, no
	// temp file, and nothing like the body allocated on the way.
	require.NoError(t, err)
	assert.Equal(t, int64(len(body)), src.n, "every byte read")
	assert.Zero(t, src.afterEOF, "the body is read once")
	assert.Equal(t, int64(len(body)), f.bytes.Load(), "the backend got every byte")
	assert.Equal(t, int64(len(body)), f.declared.Load(), "Content-Length on the request (Vail answers 411 without it)")
	assert.False(t, f.chunked.Load(), "no chunked or aws-chunked framing: the body is sent as it is")
	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)
	assert.Empty(t, entries, "no temp file")
	alloc := totalAlloc() - before
	assert.Less(t, alloc, uint64(16<<20), "allocated %d bytes for a %d-byte body: the body was buffered", alloc, len(body))
	assert.Less(t, src.maxRead, 8<<20, "the SDK's own buffer, never the object")
}

func TestGeyserPut_AnUnknownLengthStillMaterializesAndIsCounted(t *testing.T) {
	// Arrange
	f := &discardS3{}
	d := newGeyserAgainst(t, f)
	body := randomBody(t, 1<<20)
	before := promtest.ToFloat64(geyserPutMaterialized.WithLabelValues("unknown_length"))

	// Act: no length.
	err := d.Put(tctx(), "c", "unknown.bin", &onceReader{r: bytes.NewReader(body)})

	// Assert
	require.NoError(t, err)
	assert.Equal(t, int64(len(body)), f.bytes.Load())
	assert.Equal(t, int64(len(body)), f.declared.Load(), "materialize learned the length")
	assert.Equal(t, before+1, promtest.ToFloat64(geyserPutMaterialized.WithLabelValues("unknown_length")))
}

// geyserEngine is the engine with Geyser as its one (primary) backend.
func geyserEngine(t *testing.T, d *GeyserDriver) *engine.CoreEngine {
	t.Helper()
	eng := engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "geyser"})
	require.NoError(t, eng.AddDriver("geyser", d))
	return eng
}

func TestGeyserPut_AStalledStreamedPutIsOneAttemptBoundedByGeysersPutAnswerBound(t *testing.T) {
	// Arrange: the backend takes the whole body and never answers; Geyser's
	// own PUT-answer bound (5 min on prod: tape) is lowered to 300 ms. The
	// per-size FIXED_BUCKET_PUT_TIMEOUT deadline and its fresh-connection
	// retry belong to the fixed-bucket and Lyve drivers, not to Geyser.
	prev := geyserOpTimeouts
	geyserOpTimeouts.Put = 300 * time.Millisecond
	t.Cleanup(func() { geyserOpTimeouts = prev })
	f := newStallS3()
	f.stall = func(putAttempt, int) bool { return true }
	eng := geyserEngine(t, newGeyserAgainst(t, f))
	body := randomBody(t, 256<<10)

	// Act: through the engine, as every Vault write goes — with a body the
	// engine cannot rewind (a request body, the staged parts, a parity
	// shard's pipe, a demotion's read stream).
	start := time.Now()
	o := &engine.PutOutcome{}
	_, err := eng.Put(tctx(), "c", "stalled.bin", &onceReader{r: bytes.NewReader(body)}, engine.WithContentLength(int64(len(body))), engine.WithPutOutcome(o))
	elapsed := time.Since(start)

	// Assert: one attempt (a streamed body cannot be replayed: neither the
	// SDK's retry nor a fresh-connection retry), ended by Geyser's bound,
	// reported as the engine's no-failover shape (bytes went out) and as an
	// attempt without an answer (the claim rule of RP13-1).
	require.Error(t, err)
	t.Logf("stalled streamed PUT, engine error after %s: %v", elapsed, err)
	assert.ErrorIs(t, err, engine.ErrNoFailover)
	assert.True(t, o.MayHaveLanded())
	assert.Len(t, f.attemptsFor("c/stalled.bin", 0), 1, "one attempt, no retry of a stream")
	assert.Less(t, elapsed, 5*time.Second, "ended by the PUT-answer bound, not the stall")
	assert.GreaterOrEqual(t, elapsed, 250*time.Millisecond)
}

func TestGeyserPut_AMaterializedBodyWasRetriedBefore_AStreamedOneIsNot(t *testing.T) {
	// Before WP-R7-2 every Geyser body was seekable (RAM or a temp file), so
	// the SDK's retryer re-sent a stalled PUT: two attempts. The
	// unknown-length path still does; the streamed one cannot.
	prev := geyserOpTimeouts
	geyserOpTimeouts.Put = 300 * time.Millisecond
	t.Cleanup(func() { geyserOpTimeouts = prev })
	f := newStallS3()
	f.stall = func(putAttempt, int) bool { return true }
	d := newGeyserAgainst(t, f)
	body := randomBody(t, 64<<10)

	// Act
	errUnknown := d.Put(tctx(), "c", "mat.bin", &onceReader{r: bytes.NewReader(body)})
	errStream := d.Put(tctx(), "c", "str.bin", &onceReader{r: bytes.NewReader(body)}, engine.WithContentLength(int64(len(body))))

	// Assert
	require.Error(t, errUnknown)
	require.Error(t, errStream)
	assert.Len(t, f.attemptsFor("c/mat.bin", 0), 2, "a materialized (seekable) body: the SDK's two attempts")
	assert.Len(t, f.attemptsFor("c/str.bin", 0), 1, "a streamed body: one attempt")
}

var _ = context.Background
