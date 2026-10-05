package drivers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// Geyser's landing zone serves one stream at ~5 MB/s and scales with the
// number of concurrent ranges (bench 2026-10-04 §16.1: 16 ranges, 41.6 MB/s,
// hashes verified). Get therefore reads a whole object as ordered ranges
// (WP-VAULT-1 part 1):
//
//   - the FIRST range is the probe: one request, `bytes=0-<rangeSize-1>`.
//     Its Content-Range gives the object's size, its ETag the identity every
//     later range is checked against. An object on tape answers that one
//     request with InvalidObjectState — one ErrArchived for the customer,
//     never eight requests to Geyser. A backend that ignores Range answers
//     200 with the whole object, which is simply returned. An object that
//     fits the probe needs no second request.
//   - the remaining ranges are fetched by up to GEYSER_GET_CONCURRENCY
//     streams (the probe holds one slot while it is being read), each into
//     its own bounded buffer, and handed to the reader in order. A slot is
//     released when the reader has CONSUMED the range, so at most
//     concurrency × rangeSize bytes are ever buffered, and a reader that
//     leaves (Close, context cancel) stops every stream and starts no more.
//   - a range whose ETag differs from the probe's, whose Content-Range is not
//     the one asked for, or that ends short is an error to the reader, never
//     silently spliced bytes. A 200 for a later range (Range ignored on that
//     call) is sliced to the range: slow, but the bytes are right.
//
// GetRange keeps its native single request: the API's ranged GETs and the
// clients that parallelise themselves (aws-cli, R2 Sippy >199 MiB) already
// pick their own ranges.

const (
	// geyserGetConcurrencyDefault is the number of concurrent range
	// streams per Get; GEYSER_GET_CONCURRENCY overrides it (1–64; 1 is the
	// plain single-stream GET). 8 measured 21.7 MB/s raw against 5.4 for one
	// stream; 16 reached 41.6 (§16.1). Prod sets the value ([YOU]).
	geyserGetConcurrencyDefault = 8
	geyserGetConcurrencyMax     = 64
	// geyserGetRangeSize is the size of one range. 8 MiB keeps the per-Get
	// buffer at concurrency × 8 MiB = 64 MiB by default — the same RAM
	// budget as Put's spill threshold — while the transfer (1.5 s per range
	// at one stream's 5.4 MB/s) still dominates Geyser's ~0.8 s per-request
	// latency.
	geyserGetRangeSize = 8 << 20
	// geyserGetPiece is the unit a range is handed to the reader in while
	// the rest of it is still arriving: the first byte of a ranged Get is
	// the probe's first piece, not its last.
	geyserGetPiece = 256 << 10
)

var (
	geyserGetRanges = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vaultaire_geyser_get_ranges_total",
		Help: "Range requests the Geyser driver's Get issued (the probe, every parallel range, a tail); a plain single-stream GET counts one.",
	})
	geyserGetBytesPerSecond = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "vaultaire_geyser_get_bytes_per_second",
		Help:    "Throughput of a completed Geyser Get, whole object over wall time, bytes per second.",
		Buckets: prometheus.ExponentialBuckets(1<<20, 2, 10), // 1 MB/s … 512 MB/s
	})
)

// geyserGetConcurrencyFromEnv reads GEYSER_GET_CONCURRENCY: an integer in
// 1..geyserGetConcurrencyMax. Anything else is logged at Warn and the
// default kept (the R13-19 rule: a bad value never changes behaviour
// silently and never crashes the boot).
func geyserGetConcurrencyFromEnv(logger *zap.Logger) int {
	v := strings.TrimSpace(os.Getenv("GEYSER_GET_CONCURRENCY"))
	if v == "" {
		return geyserGetConcurrencyDefault
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > geyserGetConcurrencyMax {
		logger.Warn("invalid GEYSER_GET_CONCURRENCY (need an integer 1..64), keeping default",
			zap.String("value", v), zap.Int("default", geyserGetConcurrencyDefault))
		return geyserGetConcurrencyDefault
	}
	return n
}

// getSingle is the plain GET: one stream, no Range header.
func (d *GeyserDriver) getSingle(ctx context.Context, key string) (io.ReadCloser, error) {
	start := time.Now()
	resp, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("geyser get %s: %w", key, geyserWireErr(err))
	}
	geyserGetRanges.Inc()
	return &geyserTimedBody{ReadCloser: resp.Body, start: start}, nil
}

// getRanged is Get for concurrency > 1: the probe, then the window.
func (d *GeyserDriver) getRanged(ctx context.Context, key string) (io.ReadCloser, error) {
	rangeSize := d.getRangeSize
	if rangeSize <= 0 {
		rangeSize = geyserGetRangeSize
	}
	start := time.Now()
	probe, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=0-%d", rangeSize-1)),
	})
	if err != nil {
		return nil, fmt.Errorf("geyser get %s: %w", key, geyserWireErr(err))
	}
	geyserGetRanges.Inc()

	if probe.ContentRange == nil {
		// 200: the backend ignored Range and this body is the whole object.
		d.logger.Debug("geyser get: Range ignored by the backend, single stream", zap.String("key", key))
		return &geyserTimedBody{ReadCloser: probe.Body, start: start}, nil
	}
	rStart, rEnd, total, ok := parseContentRange(aws.ToString(probe.ContentRange))
	if !ok || rStart != 0 {
		// A 206 we cannot plan from (`bytes 0-N/*`, or not the range asked
		// for): the probe's bytes, then the rest as one open-ended stream.
		d.logger.Debug("geyser get: unplannable Content-Range, probe + tail",
			zap.String("key", key), zap.String("content_range", aws.ToString(probe.ContentRange)))
		if !ok {
			rEnd = rangeSize - 1
			if probe.ContentLength != nil && *probe.ContentLength > 0 {
				rEnd = *probe.ContentLength - 1
			}
		}
		return d.probeThenTail(ctx, key, probe.Body, rEnd+1, start), nil
	}
	if total <= rEnd+1 {
		// The probe holds the whole object.
		return &geyserTimedBody{ReadCloser: probe.Body, start: start, size: total}, nil
	}
	return newGeyserRangedReader(ctx, d, key, probe.Body, rEnd+1, total, aws.ToString(probe.ETag), rangeSize, start), nil
}

// parseContentRange reads `bytes <start>-<end>/<total>`; ok is false for
// `*` totals and anything else that is not that shape.
func parseContentRange(v string) (start, end, total int64, ok bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "bytes ") {
		return 0, 0, 0, false
	}
	spec, totalStr, found := strings.Cut(strings.TrimPrefix(v, "bytes "), "/")
	if !found {
		return 0, 0, 0, false
	}
	startStr, endStr, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, 0, false
	}
	var err error
	if start, err = strconv.ParseInt(startStr, 10, 64); err != nil {
		return 0, 0, 0, false
	}
	if end, err = strconv.ParseInt(endStr, 10, 64); err != nil {
		return 0, 0, 0, false
	}
	if total, err = strconv.ParseInt(totalStr, 10, 64); err != nil || total <= 0 || end < start || end >= total {
		return 0, 0, 0, false
	}
	return start, end, total, true
}

// geyserTimedBody observes the throughput histogram when a single-stream
// body reaches EOF.
type geyserTimedBody struct {
	io.ReadCloser
	start time.Time
	size  int64
	read  int64
	done  bool
}

func (b *geyserTimedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	if errors.Is(err, io.EOF) && !b.done {
		b.done = true
		observeGeyserThroughput(b.read, b.start)
	}
	return n, err
}

func observeGeyserThroughput(bytes int64, start time.Time) {
	if el := time.Since(start).Seconds(); el > 0 && bytes > 0 {
		geyserGetBytesPerSecond.Observe(float64(bytes) / el)
	}
}

// probeThenTail streams the probe's bytes and then one open-ended range
// from where it ended — the single-stream fallback when the size is unknown.
func (d *GeyserDriver) probeThenTail(ctx context.Context, key string, probe io.ReadCloser, tailFrom int64, start time.Time) io.ReadCloser {
	tail := &geyserLazyTail{ctx: ctx, d: d, key: key, from: tailFrom}
	return &geyserTimedBody{ReadCloser: struct {
		io.Reader
		io.Closer
	}{io.MultiReader(probe, tail), geyserCloserFunc(func() error {
		_ = probe.Close()
		return tail.Close()
	})}, start: start}
}

type geyserCloserFunc func() error

func (f geyserCloserFunc) Close() error { return f() }

type geyserLazyTail struct {
	ctx  context.Context
	d    *GeyserDriver
	key  string
	from int64
	body io.ReadCloser
	err  error
}

func (t *geyserLazyTail) Read(p []byte) (int, error) {
	if t.err != nil {
		return 0, t.err
	}
	if t.body == nil {
		resp, err := t.d.client.GetObject(t.ctx, &s3.GetObjectInput{
			Bucket: aws.String(t.d.bucket),
			Key:    aws.String(t.d.key(t.key)),
			Range:  aws.String(fmt.Sprintf("bytes=%d-", t.from)),
		})
		geyserGetRanges.Inc()
		if err != nil {
			t.err = fmt.Errorf("geyser get tail %s from %d: %w", t.key, t.from, geyserWireErr(err))
			return 0, t.err
		}
		if resp.ContentRange == nil {
			// Range ignored: skip what the probe already delivered.
			if _, err := io.CopyN(io.Discard, resp.Body, t.from); err != nil {
				_ = resp.Body.Close()
				t.err = fmt.Errorf("geyser get tail %s: skip %d: %w", t.key, t.from, err)
				return 0, t.err
			}
		}
		t.body = resp.Body
	}
	return t.body.Read(p)
}

func (t *geyserLazyTail) Close() error {
	if t.body != nil {
		return t.body.Close()
	}
	return nil
}

// key is the identity here: callers hand in the already-built object key.
func (d *GeyserDriver) key(k string) string { return k }

// geyserRange is one range of a ranged read: its bytes arrive in pieces on
// a channel sized to hold the whole range, so the fetcher never waits for
// the reader; err is set before pieces is closed.
type geyserRange struct {
	start, end int64 // inclusive
	pieces     chan []byte
	err        error
}

func (r *geyserRange) length() int64 { return r.end - r.start + 1 }

type geyserRangedReader struct {
	d      *GeyserDriver
	ctx    context.Context
	cancel context.CancelFunc
	key    string
	etag   string
	total  int64
	start  time.Time

	probe     io.ReadCloser
	probeLeft int64

	ranges []*geyserRange // index 0 is the probe; pieces nil
	sem    chan struct{}
	wg     sync.WaitGroup

	errMu    sync.Mutex
	firstErr error // the first failure of any stream; read through err()

	cur       int
	piece     []byte
	delivered int64
	finished  bool
	closeOnce sync.Once
}

func newGeyserRangedReader(ctx context.Context, d *GeyserDriver, key string, probe io.ReadCloser, probeLen, total int64, etag string, rangeSize int64, start time.Time) *geyserRangedReader {
	ctx, cancel := context.WithCancel(ctx)
	r := &geyserRangedReader{d: d, ctx: ctx, cancel: cancel, key: key, etag: etag, total: total, start: start,
		probe: probe, probeLeft: probeLen, sem: make(chan struct{}, d.getConcurrency)}
	r.ranges = append(r.ranges, &geyserRange{start: 0, end: probeLen - 1})
	for off := probeLen; off < total; off += rangeSize {
		end := off + rangeSize - 1
		if end > total-1 {
			end = total - 1
		}
		rg := &geyserRange{start: off, end: end}
		rg.pieces = make(chan []byte, int((rg.length()+geyserGetPiece-1)/geyserGetPiece)+1)
		r.ranges = append(r.ranges, rg)
	}
	r.sem <- struct{}{} // the probe's slot, released when it has been read
	r.wg.Add(1)
	go r.schedule()
	return r
}

// schedule starts the ranges in order, each when a slot is free; it stops
// the moment the reader is gone.
func (r *geyserRangedReader) schedule() {
	defer r.wg.Done()
	for i := 1; i < len(r.ranges); i++ {
		select {
		case r.sem <- struct{}{}:
		case <-r.ctx.Done():
			return
		}
		r.wg.Add(1)
		go r.fetch(r.ranges[i])
	}
}

// fail records the first failure of any stream and stops the rest; the
// reader reports that one, not the cancellations it causes.
func (r *geyserRangedReader) fail(err error) {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	if r.firstErr == nil {
		r.firstErr = err
		r.cancel()
	}
}

func (r *geyserRangedReader) err() error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.firstErr
}

// fetch downloads one range into its pieces channel.
func (r *geyserRangedReader) fetch(rg *geyserRange) {
	defer r.wg.Done()
	defer close(rg.pieces)
	resp, err := r.d.client.GetObject(r.ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.d.bucket),
		Key:    aws.String(r.key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", rg.start, rg.end)),
	})
	geyserGetRanges.Inc()
	if err != nil {
		rg.err = fmt.Errorf("geyser get %s range %d-%d: %w", r.key, rg.start, rg.end, geyserWireErr(err))
		r.fail(rg.err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if et := aws.ToString(resp.ETag); r.etag != "" && et != "" && et != r.etag {
		rg.err = fmt.Errorf("geyser get %s: object changed during read (etag %s, then %s)", r.key, r.etag, et)
		r.fail(rg.err)
		return
	}
	body := io.Reader(resp.Body)
	if resp.ContentRange == nil {
		// 200: Range ignored on this call. Slice the whole body to the range
		// rather than splicing it in whole.
		if _, err := io.CopyN(io.Discard, body, rg.start); err != nil {
			rg.err = fmt.Errorf("geyser get %s range %d-%d: backend ignored Range and the body ended at the skip: %w", r.key, rg.start, rg.end, err)
			r.fail(rg.err)
			return
		}
	} else if s, e, _, ok := parseContentRange(aws.ToString(resp.ContentRange)); !ok || s != rg.start || e != rg.end {
		rg.err = fmt.Errorf("geyser get %s: asked for range %d-%d, got Content-Range %q", r.key, rg.start, rg.end, aws.ToString(resp.ContentRange))
		r.fail(rg.err)
		return
	}
	left := rg.length()
	for left > 0 {
		n := int64(geyserGetPiece)
		if left < n {
			n = left
		}
		buf := make([]byte, n)
		got, err := io.ReadFull(body, buf)
		if got > 0 {
			select {
			case rg.pieces <- buf[:got]:
			case <-r.ctx.Done():
				rg.err = r.ctx.Err()
				return
			}
			left -= int64(got)
		}
		if err != nil {
			if left > 0 {
				rg.err = fmt.Errorf("geyser get %s range %d-%d ended short: %d of %d bytes: %w", r.key, rg.start, rg.end, rg.length()-left, rg.length(), err)
				r.fail(rg.err)
			}
			return
		}
	}
}

func (r *geyserRangedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if r.finished {
			return 0, io.EOF
		}
		if err := r.err(); err != nil && r.cur > 0 {
			return 0, err
		}
		if len(r.piece) > 0 {
			n := copy(p, r.piece)
			r.piece = r.piece[n:]
			r.delivered += int64(n)
			return n, nil
		}
		if r.cur == 0 {
			if r.probeLeft == 0 {
				_ = r.probe.Close()
				<-r.sem
				r.cur++
				continue
			}
			want := int64(len(p))
			if want > r.probeLeft {
				want = r.probeLeft
			}
			n, err := r.probe.Read(p[:want])
			r.probeLeft -= int64(n)
			r.delivered += int64(n)
			if err != nil && (!errors.Is(err, io.EOF) || r.probeLeft > 0) {
				if errors.Is(err, io.EOF) {
					err = fmt.Errorf("geyser get %s: probe range ended short, %d bytes missing: %w", r.key, r.probeLeft, io.ErrUnexpectedEOF)
				}
				r.fail(err)
				return n, r.err()
			}
			if n > 0 {
				return n, nil
			}
			continue
		}
		rg := r.ranges[r.cur]
		piece, ok := <-rg.pieces
		if !ok {
			if rg.err != nil {
				r.fail(rg.err)
				return 0, r.err()
			}
			<-r.sem
			r.cur++
			if r.cur == len(r.ranges) {
				r.finished = true
				observeGeyserThroughput(r.delivered, r.start)
				return 0, io.EOF
			}
			continue
		}
		r.piece = piece
	}
}

// Close stops every stream, starts no more, and waits for the fetchers to
// exit so nothing outlives the reader.
func (r *geyserRangedReader) Close() error {
	r.closeOnce.Do(func() {
		r.cancel()
		_ = r.probe.Close()
		r.wg.Wait()
	})
	return nil
}
