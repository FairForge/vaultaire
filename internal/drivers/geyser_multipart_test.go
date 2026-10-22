package drivers

// Geyser's Vail gateway serves one PUT stream at 24–35 MB/s and 16 parallel
// parts of a multipart upload over HTTP/1.1 at 100–276 MB/s (geyser_README).
// A Put of a known length at or above GEYSER_MULTIPART_MIN therefore goes as
// a parallel multipart upload; a smaller one keeps the streamed PUT.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/FairForge/vaultaire/internal/engine"
)

// mpuS3 is a fake Vail that speaks PutObject and the multipart calls and
// keeps the assembled object.
type mpuS3 struct {
	mu        sync.Mutex
	parts     map[int][]byte
	partLen   map[int]int64 // the Content-Length header of each part
	objects   map[string][]byte
	puts      int
	creates   int
	completes int
	aborts    int
	badHdr    []string // checksum / aws-chunked headers seen on a part

	inFlight      atomic.Int32
	maxInFlight   atomic.Int32
	partDelay     time.Duration
	failPartsOnce map[int]bool // part number → answer 500 the first time
	failComplete  bool
	partAttempts  map[int]int
}

func newMPUS3() *mpuS3 {
	return &mpuS3{parts: map[int][]byte{}, partLen: map[int]int64{}, objects: map[string][]byte{},
		failPartsOnce: map[int]bool{}, partAttempts: map[int]int{}}
}

func (f *mpuS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.mu.Lock()
		f.creates++
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>tape</Bucket><Key>%s</Key><UploadId>up-1</UploadId></InitiateMultipartUploadResult>`, r.URL.Path)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		cur := f.inFlight.Add(1)
		for {
			m := f.maxInFlight.Load()
			if cur <= m || f.maxInFlight.CompareAndSwap(m, cur) {
				break
			}
		}
		defer f.inFlight.Add(-1)
		body, _ := io.ReadAll(r.Body)
		if f.partDelay > 0 {
			time.Sleep(f.partDelay)
		}
		f.mu.Lock()
		f.partAttempts[n]++
		for _, h := range []string{"X-Amz-Sdk-Checksum-Algorithm", "X-Amz-Checksum-Crc32", "X-Amz-Trailer", "Content-Encoding"} {
			if r.Header.Get(h) != "" {
				f.badHdr = append(f.badHdr, h+": "+r.Header.Get(h))
			}
		}
		if len(r.TransferEncoding) > 0 {
			f.badHdr = append(f.badHdr, "Transfer-Encoding")
		}
		if f.failPartsOnce[n] {
			delete(f.failPartsOnce, n)
			f.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `<Error><Code>InternalError</Code><Message>once</Message></Error>`)
			return
		}
		f.parts[n] = body
		f.partLen[n] = r.ContentLength
		f.mu.Unlock()
		w.Header().Set("ETag", fmt.Sprintf(`"p%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		_, _ = io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.completes++
		if f.failComplete {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `<Error><Code>InvalidPart</Code><Message>no</Message></Error>`)
			return
		}
		nums := make([]int, 0, len(f.parts))
		for n := range f.parts {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		var all bytes.Buffer
		for _, n := range nums {
			all.Write(f.parts[n])
		}
		f.objects[r.URL.Path] = all.Bytes()
		_, _ = fmt.Fprintf(w, `<CompleteMultipartUploadResult><Key>%s</Key><ETag>"x-%d"</ETag></CompleteMultipartUploadResult>`, r.URL.Path, len(nums))
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		f.mu.Lock()
		f.aborts++
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.puts++
		f.objects[r.URL.Path] = body
		f.mu.Unlock()
		w.Header().Set("ETag", `"single"`)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// geyserWithPut is a Geyser driver against h with a small multipart shape:
// 5 MiB parts (S3's minimum), multipart from 10 MiB, 4 parts at once.
func geyserWithPut(t *testing.T, h http.Handler) *GeyserDriver {
	t.Helper()
	d := newGeyserAgainst(t, h)
	d.put = geyserPutConfig{multipartMin: 10 << 20, partMin: 5 << 20, partMax: 5 << 20, concurrency: 4, budget: 1 << 30}
	d.parts = newGeyserParts(d.put.budget)
	return d
}

func TestGeyserPut_AKnownLengthAtTheThresholdGoesAsParallelParts(t *testing.T) {
	// Arrange: 23 MiB = four 5 MiB parts and a 3 MiB tail, behind a reader
	// that cannot be re-read; each part takes 50 ms so parallelism shows.
	f := newMPUS3()
	f.partDelay = 50 * time.Millisecond
	d := geyserWithPut(t, f)
	body := randomBody(t, 23<<20)
	src := &onceReader{r: bytes.NewReader(body)}
	before := promtest.ToFloat64(geyserPutMultipart.WithLabelValues("ok"))

	// Act
	err := d.Put(tctx(), "c", "big.bin", src, engine.WithContentLength(int64(len(body))))

	// Assert: one multipart upload, five parts each framed by its own
	// Content-Length, no checksum trailer, several in flight, byte-equal.
	require.NoError(t, err)
	assert.Equal(t, 1, f.creates)
	assert.Equal(t, 1, f.completes)
	assert.Zero(t, f.puts, "no single PUT")
	assert.Len(t, f.parts, 5)
	for n, l := range f.partLen {
		assert.Equal(t, int64(len(f.parts[n])), l, "part %d: Content-Length on the request (Vail answers 411 without it)", n)
	}
	assert.Empty(t, f.badHdr, "no checksum header, trailer or chunked framing on a part")
	assert.Greater(t, f.maxInFlight.Load(), int32(1), "parts in parallel")
	assert.LessOrEqual(t, f.maxInFlight.Load(), int32(4), "never more than the concurrency")
	got := f.objects["/tape/t-t1/c/big.bin"]
	assert.Equal(t, sha256.Sum256(body), sha256.Sum256(got), "the assembled object is the body")
	assert.Zero(t, src.afterEOF, "the body is read once")
	assert.Equal(t, before+1, promtest.ToFloat64(geyserPutMultipart.WithLabelValues("ok")))
}

func TestGeyserPut_BelowTheThresholdStaysOneStreamedPut(t *testing.T) {
	f := newMPUS3()
	d := geyserWithPut(t, f)
	body := randomBody(t, 9<<20)

	err := d.Put(tctx(), "c", "small.bin", &onceReader{r: bytes.NewReader(body)}, engine.WithContentLength(int64(len(body))))

	require.NoError(t, err)
	assert.Equal(t, 1, f.puts)
	assert.Zero(t, f.creates, "no multipart upload")
	assert.Equal(t, body, f.objects["/tape/t-t1/c/small.bin"])
}

func TestGeyserPut_MultipartOffStreamsEvenALargeBody(t *testing.T) {
	f := newMPUS3()
	d := geyserWithPut(t, f)
	d.put.multipartMin = 0 // GEYSER_MULTIPART_MIN=off
	body := randomBody(t, 23<<20)

	err := d.Put(tctx(), "c", "off.bin", &onceReader{r: bytes.NewReader(body)}, engine.WithContentLength(int64(len(body))))

	require.NoError(t, err)
	assert.Equal(t, 1, f.puts)
	assert.Zero(t, f.creates)
}

func TestGeyserPut_AMultipartBodyThatEndsShortIsAbortedNeverCompleted(t *testing.T) {
	// Arrange: 23 MiB declared, 17 MiB delivered, then a clean EOF (a client
	// gone, a proxy that stripped the length).
	f := newMPUS3()
	d := geyserWithPut(t, f)
	body := randomBody(t, 17<<20)
	before := promtest.ToFloat64(geyserPutMultipart.WithLabelValues("error"))

	// Act
	err := d.Put(tctx(), "c", "short.bin", &onceReader{r: bytes.NewReader(body)}, engine.WithContentLength(23<<20))

	// Assert: never stored truncated under the declared size.
	require.Error(t, err)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Zero(t, f.completes, "a short body is never completed")
	assert.Equal(t, 1, f.aborts, "the upload is aborted: no parts left billed")
	assert.NotContains(t, f.objects, "/tape/t-t1/c/short.bin")
	assert.Equal(t, before+1, promtest.ToFloat64(geyserPutMultipart.WithLabelValues("error")))
}

func TestGeyserPut_AFailedPartIsRetriedAloneNotTheWholeObject(t *testing.T) {
	// A part is a buffer the SDK can re-send; the streamed PUT is one attempt.
	f := newMPUS3()
	f.failPartsOnce[3] = true
	d := geyserWithPut(t, f)
	body := randomBody(t, 23<<20)

	err := d.Put(tctx(), "c", "retry.bin", &onceReader{r: bytes.NewReader(body)}, engine.WithContentLength(int64(len(body))))

	require.NoError(t, err)
	assert.Equal(t, 2, f.partAttempts[3], "part 3 sent twice")
	assert.Equal(t, 1, f.partAttempts[1], "the others once")
	assert.Equal(t, 1, f.creates, "one upload, not a restart")
	assert.Equal(t, sha256.Sum256(body), sha256.Sum256(f.objects["/tape/t-t1/c/retry.bin"]))
}

func TestGeyserPut_AFailedCompleteIsAnErrorAndAborts(t *testing.T) {
	f := newMPUS3()
	f.failComplete = true
	d := geyserWithPut(t, f)
	body := randomBody(t, 12<<20)

	err := d.Put(tctx(), "c", "nocomplete.bin", &onceReader{r: bytes.NewReader(body)}, engine.WithContentLength(int64(len(body))))

	require.Error(t, err)
	assert.Equal(t, 1, f.aborts)
}

func TestGeyserPut_TheBudgetBoundsThePartsHeldAcrossUploads(t *testing.T) {
	// Arrange: concurrency 4, but a budget of two parts.
	f := newMPUS3()
	f.partDelay = 50 * time.Millisecond
	d := geyserWithPut(t, f)
	d.put.budget = 10 << 20
	d.parts = newGeyserParts(d.put.budget)
	body := randomBody(t, 23<<20)

	// Act
	err := d.Put(tctx(), "c", "budget.bin", &onceReader{r: bytes.NewReader(body)}, engine.WithContentLength(int64(len(body))))

	// Assert
	require.NoError(t, err)
	assert.LessOrEqual(t, f.maxInFlight.Load(), int32(2), "never more parts in memory than the budget holds")
	assert.Equal(t, sha256.Sum256(body), sha256.Sum256(f.objects["/tape/t-t1/c/budget.bin"]))
}

func TestGeyserPut_ACallerGoneMidUploadStillAbortsIt(t *testing.T) {
	// Arrange: parts take 200 ms; the caller leaves after 100 ms.
	f := newMPUS3()
	f.partDelay = 200 * time.Millisecond
	d := geyserWithPut(t, f)
	body := randomBody(t, 23<<20)
	ctx, cancel := context.WithTimeout(tctx(), 100*time.Millisecond)
	defer cancel()

	// Act
	err := d.Put(ctx, "c", "gone.bin", &onceReader{r: bytes.NewReader(body)}, engine.WithContentLength(int64(len(body))))

	// Assert: the abort is sent on a context of its own.
	require.Error(t, err)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Zero(t, f.completes)
	assert.Equal(t, 1, f.aborts, "aborted although the caller's context is done")
}

func TestGeyserPartSizeFor(t *testing.T) {
	c := defaultGeyserPutConfig() // 64 MiB … 256 MiB, 16 at once
	cases := []struct {
		n     int64
		want  int64
		parts int64
	}{
		{1 << 30, 64 << 20, 16},    // at the threshold: one wave of 16 (measured 102 MB/s)
		{4 << 30, 256 << 20, 16},   // the ceiling (measured 205 MB/s)
		{3 << 30, 256 << 20, 12},   // a power of two: the buffers pool
		{50 << 30, 256 << 20, 200}, // past one wave: more parts, never bigger buffers
	}
	for _, tc := range cases {
		got := c.partSizeFor(tc.n)
		assert.Equal(t, tc.want, got, "%d bytes", tc.n)
		assert.Equal(t, tc.parts, (tc.n+got-1)/got, "%d bytes: parts", tc.n)
	}
	huge := c.partSizeFor(6 << 40)
	assert.GreaterOrEqual(t, huge*geyserMaxParts, int64(6<<40), "6 TiB fits in 10,000 parts")
	assert.Zero(t, huge%(1<<20), "a whole number of MiB")
}

func TestGeyserPutConfigFromEnv(t *testing.T) {
	def := defaultGeyserPutConfig()
	with := func(f func(*geyserPutConfig)) geyserPutConfig { c := def; f(&c); return c }
	cases := []struct {
		name                 string
		min, max, conc, budg string
		want                 geyserPutConfig
		warns                int
	}{
		{"defaults", "", "", "", "", def, 0},
		{"set", "2GiB", "512MiB", "8", "8GiB", with(func(c *geyserPutConfig) {
			c.multipartMin, c.partMax, c.concurrency, c.budget = 2<<30, 512<<20, 8, 8<<30
		}), 0},
		{"off", "off", "", "", "", with(func(c *geyserPutConfig) { c.multipartMin = 0 }), 0},
		{"rejected", "lots", "1MiB", "0", "1MiB", def, 4},
		{"min at the part floor", "64MiB", "", "", "", def, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GEYSER_MULTIPART_MIN", tc.min)
			t.Setenv("GEYSER_PUT_PART_MAX", tc.max)
			t.Setenv("GEYSER_PUT_CONCURRENCY", tc.conc)
			t.Setenv("GEYSER_PUT_BUDGET", tc.budg)
			core, logs := observer.New(zap.WarnLevel)

			got := geyserPutConfigFromEnv(zap.New(core))

			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.warns, logs.Len())
		})
	}
}
