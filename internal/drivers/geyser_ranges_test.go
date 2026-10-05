package drivers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// WP-VAULT-1 part 1: the Geyser driver's Get reads an object as ordered
// parallel ranges (bench §16.1: 1 stream 5.4 MB/s, 16 streams 41.6 MB/s on
// the landing zone). rangeS3 is a fake that honours Range the way Vail does
// (206 + Content-Range + the object's ETag), records every Range header and
// the peak number of requests in flight, and has switches for the cases the
// adversarial pass names: a backend that ignores Range (200 for the probe,
// or for a later range), an object on tape (403 InvalidObjectState), an
// ETag that changes under the reader, and a slow range.

type rangeS3 struct {
	mu        sync.Mutex
	data      []byte
	etag      string
	ranges    []string // every Range header seen, in arrival order
	gets      int32
	inFlight  int32
	peak      int32
	ignoreAll bool // 200 + whole body for every GET
	ignoreNth int  // 1-based: this GET (and only it) answers 200 + whole body
	archived  bool
	etagFlip  int // 1-based: from this GET on, a different ETag
	delay     time.Duration
	release   chan struct{} // when set, GETs block until closed
}

func newRangeS3(size int) *rangeS3 {
	data := make([]byte, size)
	_, _ = rand.Read(data)
	return &rangeS3{data: data, etag: `"etag-v1"`}
}

func (f *rangeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	n := atomic.AddInt32(&f.gets, 1)
	cur := atomic.AddInt32(&f.inFlight, 1)
	defer atomic.AddInt32(&f.inFlight, -1)
	for {
		p := atomic.LoadInt32(&f.peak)
		if cur <= p || atomic.CompareAndSwapInt32(&f.peak, p, cur) {
			break
		}
	}
	f.mu.Lock()
	f.ranges = append(f.ranges, r.Header.Get("Range"))
	f.mu.Unlock()
	if f.release != nil {
		<-f.release
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.archived {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidObjectState</Code><Message>on tape</Message></Error>`))
		return
	}
	etag := f.etag
	if f.etagFlip > 0 && int(n) >= f.etagFlip {
		etag = `"etag-v2"`
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Last-Modified", "Sun, 04 Oct 2026 21:00:00 GMT")
	rng := r.Header.Get("Range")
	if rng == "" || f.ignoreAll || int(n) == f.ignoreNth {
		w.Header().Set("Content-Length", strconv.Itoa(len(f.data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(f.data)
		return
	}
	var start, end int64
	spec := strings.TrimPrefix(rng, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	start, _ = strconv.ParseInt(parts[0], 10, 64)
	if parts[1] == "" {
		end = int64(len(f.data)) - 1
	} else {
		end, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	if end >= int64(len(f.data)) {
		end = int64(len(f.data)) - 1
	}
	if start > end {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(f.data)))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(f.data)))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(f.data[start : end+1])
}

func (f *rangeS3) rangeHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ranges...)
}

func newRangeGeyser(t *testing.T, f *rangeS3, concurrency int, rangeSize int64) *GeyserDriver {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		Region:       "us-west-2",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		UsePathStyle: true,
	})
	return &GeyserDriver{client: client, bucket: "b", tenantID: "t1", logger: zap.NewNop(), endpoint: srv.URL,
		getConcurrency: concurrency, getRangeSize: rangeSize}
}

func sum(b []byte) string { h := sha256.Sum256(b); return fmt.Sprintf("%x", h) }

func TestGeyserGet_ReadsAnObjectAsOrderedParallelRanges(t *testing.T) {
	const mib = 1 << 20
	f := newRangeS3(10*mib + 12345) // 11 ranges of 1 MiB, the last one partial
	d := newRangeGeyser(t, f, 4, mib)
	rangesBefore := promtest.ToFloat64(geyserGetRanges)

	rc, err := d.Get(context.Background(), "arc", "big.bin")
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())

	assert.Equal(t, sum(f.data), sum(got), "the bytes are the object's, in order")
	hdrs := f.rangeHeaders()
	require.Len(t, hdrs, 11, "one request per range: %v", hdrs)
	assert.Equal(t, "bytes=0-1048575", hdrs[0], "the first range is the probe")
	want := map[string]bool{}
	for i := 0; i < 11; i++ {
		end := int64(i+1)*mib - 1
		if end > int64(len(f.data))-1 {
			end = int64(len(f.data)) - 1
		}
		want[fmt.Sprintf("bytes=%d-%d", int64(i)*mib, end)] = true
	}
	for _, h := range hdrs {
		assert.True(t, want[h], "unexpected range %s", h)
	}
	assert.LessOrEqual(t, f.peak, int32(4), "never more than GEYSER_GET_CONCURRENCY streams in flight")
	assert.GreaterOrEqual(t, f.peak, int32(2), "the ranges were fetched in parallel")
	assert.Equal(t, float64(11), promtest.ToFloat64(geyserGetRanges)-rangesBefore)
}

func TestGeyserGet_SmallObjectIsOneRequest(t *testing.T) {
	f := newRangeS3(300_000)
	d := newRangeGeyser(t, f, 8, 1<<20)
	rc, err := d.Get(context.Background(), "arc", "small.bin")
	require.NoError(t, err)
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, sum(f.data), sum(got))
	assert.Len(t, f.rangeHeaders(), 1, "an object that fits the probe range needs no second request")
}

func TestGeyserGet_ConcurrencyOneIsTheSingleStream(t *testing.T) {
	f := newRangeS3(3 << 20)
	d := newRangeGeyser(t, f, 1, 1<<20)
	rc, err := d.Get(context.Background(), "arc", "x.bin")
	require.NoError(t, err)
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, sum(f.data), sum(got))
	hdrs := f.rangeHeaders()
	require.Len(t, hdrs, 1)
	assert.Equal(t, "", hdrs[0], "GEYSER_GET_CONCURRENCY=1 is a plain GET with no Range header")
}

func TestGeyserGet_BackendThatIgnoresRangeFallsBackWithoutCorruption(t *testing.T) {
	t.Run("on the probe", func(t *testing.T) {
		f := newRangeS3(5 << 20)
		f.ignoreAll = true
		d := newRangeGeyser(t, f, 4, 1<<20)
		rc, err := d.Get(context.Background(), "arc", "x.bin")
		require.NoError(t, err)
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		_ = rc.Close()
		assert.Equal(t, sum(f.data), sum(got))
		assert.Len(t, f.rangeHeaders(), 1, "a 200 to the probe IS the whole object: no further requests")
	})
	t.Run("on a later range", func(t *testing.T) {
		f := newRangeS3(5 << 20)
		f.ignoreNth = 3
		d := newRangeGeyser(t, f, 2, 1<<20)
		rc, err := d.Get(context.Background(), "arc", "x.bin")
		require.NoError(t, err)
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		_ = rc.Close()
		assert.Equal(t, sum(f.data), sum(got), "a 200 for one range is sliced to that range, never spliced whole")
	})
}

func TestGeyserGet_ObjectOnTapeIsOneRefusal(t *testing.T) {
	f := newRangeS3(5 << 20)
	f.archived = true
	d := newRangeGeyser(t, f, 8, 1<<20)
	_, err := d.Get(context.Background(), "arc", "x.bin")
	require.Error(t, err)
	assert.True(t, errors.Is(err, engine.ErrArchived), "got %v", err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&f.gets), "one 503 for the customer, not eight requests to Geyser")
}

func TestGeyserGet_ETagChangeUnderTheReaderIsAnError(t *testing.T) {
	f := newRangeS3(5 << 20)
	f.etagFlip = 3
	d := newRangeGeyser(t, f, 2, 1<<20)
	rc, err := d.Get(context.Background(), "arc", "x.bin")
	require.NoError(t, err)
	_, err = io.ReadAll(rc)
	_ = rc.Close()
	require.Error(t, err, "an overwrite between ranges must not splice two versions")
	assert.Contains(t, err.Error(), "changed")
}

func TestGeyserGet_CloseStopsTheRangesNotYetStarted(t *testing.T) {
	f := newRangeS3(64 << 20) // 64 ranges of 1 MiB
	f.delay = 30 * time.Millisecond
	d := newRangeGeyser(t, f, 4, 1<<20)
	rc, err := d.Get(context.Background(), "arc", "x.bin")
	require.NoError(t, err)
	buf := make([]byte, 1<<20)
	_, err = io.ReadFull(rc, buf) // the probe's bytes
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	started := atomic.LoadInt32(&f.gets)
	assert.Less(t, started, int32(16), "a reader that left after one range did not get 64 ranges fetched for it (%d started)", started)
	assert.Eventually(t, func() bool { return atomic.LoadInt32(&f.inFlight) == 0 }, 2*time.Second, 10*time.Millisecond,
		"every in-flight range request ended with the reader")
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, started, atomic.LoadInt32(&f.gets), "nothing started after Close")
}

func TestGeyserGet_ShortRangeIsAnErrorNotSilence(t *testing.T) {
	f := newRangeS3(3 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=1048576-2097151" {
			w.Header().Set("ETag", f.etag)
			w.Header().Set("Content-Range", "bytes 1048576-2097151/3145728")
			w.Header().Set("Content-Length", "1048576")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(f.data[1<<20 : (1<<20)+100]) // cut short
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{BaseEndpoint: aws.String(srv.URL), Region: "us-west-2",
		Credentials: credentials.NewStaticCredentialsProvider("t", "t", ""), UsePathStyle: true})
	d := &GeyserDriver{client: client, bucket: "b", tenantID: "t1", logger: zap.NewNop(), getConcurrency: 2, getRangeSize: 1 << 20}
	rc, err := d.Get(context.Background(), "arc", "x.bin")
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	require.Error(t, err)
	assert.Less(t, len(got), len(f.data))
}

func TestGeyserGetRange_KeepsItsNativeSingleRequest(t *testing.T) {
	f := newRangeS3(5 << 20)
	d := newRangeGeyser(t, f, 8, 1<<20)
	rc, err := d.GetRange(context.Background(), "arc", "x.bin", 1<<20, 3<<20)
	require.NoError(t, err)
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	assert.True(t, bytes.Equal(f.data[1<<20:4<<20], got))
	assert.Equal(t, []string{"bytes=1048576-4194303"}, f.rangeHeaders())
}

func TestGeyserGetConcurrency_EnvRule(t *testing.T) {
	for _, c := range []struct {
		val  string
		want int
		warn bool
	}{
		{"", 8, false}, {"16", 16, false}, {"1", 1, false}, {"64", 64, false},
		{"0", 8, true}, {"-2", 8, true}, {"abc", 8, true}, {"65", 8, true}, {"8.5", 8, true},
	} {
		t.Setenv("GEYSER_GET_CONCURRENCY", c.val)
		core, logs := observer.New(zap.WarnLevel)
		got := geyserGetConcurrencyFromEnv(zap.New(core))
		assert.Equal(t, c.want, got, "value %q", c.val)
		if c.warn {
			require.Equal(t, 1, logs.Len(), "value %q is logged at Warn and the default kept (R13-19)", c.val)
			assert.Contains(t, logs.All()[0].Message, "GEYSER_GET_CONCURRENCY")
		} else {
			assert.Equal(t, 0, logs.Len(), "value %q", c.val)
		}
	}
}

func TestNewGeyserDriver_ReadsTheConcurrencyEnv(t *testing.T) {
	t.Setenv("GEYSER_GET_CONCURRENCY", "12")
	d, err := NewGeyserDriver("ak", "sk", "bucket", "", zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, 12, d.getConcurrency)
	assert.Equal(t, int64(geyserGetRangeSize), d.getRangeSize)
}
