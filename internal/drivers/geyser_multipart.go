package drivers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

// Geyser's Vail gateway takes one PUT stream per HTTP/1.1 connection and
// the parts of a multipart upload in parallel. Measured on LA1 from SLC,
// 2026-10-22 (cmd/tools/geyser-put-bench; bench-results/GEYSER-MULTIPART-2026-10-22.md):
//
//   - the streamed PUT ramps with the object: 30 MB/s at 64 MiB, 63 at
//     256 MiB, 86 at 1 GiB, 92 at 4 GiB;
//   - a part streams at 30–65 MB/s per connection, faster for a larger
//     part, and the parts scale with concurrency (16 × 64 MiB: 553 MB/s);
//   - CompleteMultipartUpload costs per PART, not per byte: 5–10 s up to 16
//     parts (256 MiB or 1 GiB alike), 21–34 s at 32–64 parts.
//
// So a Put of a KNOWN length at or above GEYSER_MULTIPART_MIN (1 GiB; at
// 256 MiB the parts plus Complete's floor lost to the stream, 45 vs 63
// MB/s) is cut into about GEYSER_PUT_CONCURRENCY (16) parts of
// 64 MiB…GEYSER_PUT_PART_MAX (256 MiB) and uploaded that many at a time:
// 4 GiB as 16 × 256 MiB 205 MB/s against 92 for the stream, 1 GiB as
// 16 × 64 MiB 102 against 86. A smaller Put keeps putStreamed.
//
//   - Each part is read into a buffer first (pooled per size), under the
//     process-wide GEYSER_PUT_BUDGET — waited for, never refused — so the
//     SDK's retryer re-sends a failed part on its own; the streamed PUT is
//     one attempt for the whole body. Every part carries its Content-Length
//     (Vail answers 411 without one); checksums are WhenRequired (the client
//     option of putStreamed): Vail has never been shown to take the SDK's
//     default CRC32.
//   - A body that ends before its declared size is an error, never a
//     shorter object; nothing past the declared size is read.
//   - A failed upload is aborted on a context detached from the caller's (a
//     caller gone mid-upload is the common failure), bounded by
//     geyserAbortTimeout; Geyser's provisioning lifecycle rule aborts what is
//     left after 7 days.
//   - A re-sent Complete whose first copy committed answers NoSuchUpload
//     (Prompt 2b.5 F3.8): the Complete class's 10 min bound
//     (geyserOpTimeouts.Complete) keeps the SDK from re-sending it on a
//     header timeout.

const (
	geyserMultipartMinDefault   = 1 << 30
	geyserPartMinDefault        = 64 << 20
	geyserPartMaxDefault        = 256 << 20
	geyserPartMaxLimit          = 1 << 30
	geyserPutConcurrencyDefault = 16
	geyserPutConcurrencyMax     = 64
	// geyserPutBudgetDefault is one upload at full width: 16 × 256 MiB.
	geyserPutBudgetDefault = 4 << 30
	geyserMaxParts         = 10000
	// geyserAbortTimeout bounds the abort of a failed upload.
	geyserAbortTimeout = 30 * time.Second
)

// geyserPutConfig is the multipart shape of a Geyser Put.
type geyserPutConfig struct {
	// multipartMin is the smallest known length sent as a multipart
	// upload; 0 = never (GEYSER_MULTIPART_MIN=off).
	multipartMin int64
	// partMin / partMax bound the part size; partMin is fixed (64 MiB),
	// partMax is GEYSER_PUT_PART_MAX (256 MiB).
	partMin, partMax int64
	concurrency      int
	// budget bounds the part bytes every upload of the process holds.
	budget int64
}

func defaultGeyserPutConfig() geyserPutConfig {
	return geyserPutConfig{multipartMin: geyserMultipartMinDefault, partMin: geyserPartMinDefault,
		partMax: geyserPartMaxDefault, concurrency: geyserPutConcurrencyDefault, budget: geyserPutBudgetDefault}
}

var (
	geyserPutMultipart = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_geyser_put_multipart_total",
		Help: "Geyser Puts sent as a parallel multipart upload (a known length at or above GEYSER_MULTIPART_MIN), by outcome.",
	}, []string{"outcome"})
	geyserPutBytesPerSecond = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vaultaire_geyser_put_bytes_per_second",
		Help:    "Throughput of a completed Geyser Put of a known length, body over wall time, bytes per second, by path (streamed | multipart).",
		Buckets: prometheus.ExponentialBuckets(1<<20, 2, 10), // 1 MB/s … 512 MB/s
	}, []string{"path"})
)

func init() {
	geyserPutMultipart.WithLabelValues("ok")
	geyserPutMultipart.WithLabelValues("error")
}

// geyserPutConfigFromEnv reads GEYSER_MULTIPART_MIN (a size above 64 MiB,
// or `off`), GEYSER_PUT_PART_MAX (64 MiB–1 GiB), GEYSER_PUT_CONCURRENCY
// (1–64) and GEYSER_PUT_BUDGET (at least one part of GEYSER_PUT_PART_MAX).
// A rejected value is logged at Warn and the default kept (R13-19).
func geyserPutConfigFromEnv(logger *zap.Logger) geyserPutConfig {
	cfg := defaultGeyserPutConfig()
	if v := strings.TrimSpace(os.Getenv("GEYSER_PUT_PART_MAX")); v != "" {
		n, err := parseByteSize(v)
		if err != nil || n < cfg.partMin || n > geyserPartMaxLimit {
			logger.Warn("invalid GEYSER_PUT_PART_MAX (need a size 64MiB..1GiB), keeping default",
				zap.String("value", v), zap.Int64("default", geyserPartMaxDefault))
		} else {
			cfg.partMax = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("GEYSER_PUT_CONCURRENCY")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > geyserPutConcurrencyMax {
			logger.Warn("invalid GEYSER_PUT_CONCURRENCY (need an integer 1..64), keeping default",
				zap.String("value", v), zap.Int("default", geyserPutConcurrencyDefault))
		} else {
			cfg.concurrency = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("GEYSER_PUT_BUDGET")); v != "" {
		n, err := parseByteSize(v)
		if err != nil || n < cfg.partMax {
			logger.Warn("invalid GEYSER_PUT_BUDGET (need a size of at least GEYSER_PUT_PART_MAX), keeping default",
				zap.String("value", v), zap.Int64("default", geyserPutBudgetDefault))
		} else {
			cfg.budget = n
		}
	}
	if cfg.budget < cfg.partMax {
		cfg.budget = cfg.partMax
	}
	if v := strings.TrimSpace(os.Getenv("GEYSER_MULTIPART_MIN")); v != "" {
		if strings.EqualFold(v, "off") {
			cfg.multipartMin = 0
		} else if n, err := parseByteSize(v); err != nil || n <= cfg.partMin {
			logger.Warn("invalid GEYSER_MULTIPART_MIN (need a size above 64MiB, or off), keeping default",
				zap.String("value", v), zap.Int64("default", geyserMultipartMinDefault))
		} else {
			cfg.multipartMin = n
		}
	}
	return cfg
}

// partSizeFor is the part size of a body of n bytes: about concurrency
// parts (one wave: Complete's cost grows with the part count), a power of
// two between partMin and partMax (so buffers pool), grown past partMax
// only to stay within S3's 10,000 parts.
func (c geyserPutConfig) partSizeFor(n int64) int64 {
	want := (n + int64(c.concurrency) - 1) / int64(c.concurrency)
	size := c.partMin
	for size < want && size < c.partMax {
		size *= 2
	}
	size = min(size, max(c.partMax, c.partMin))
	if n > size*geyserMaxParts {
		need := (n + geyserMaxParts - 1) / geyserMaxParts
		size = (need + (1 << 20) - 1) &^ ((1 << 20) - 1)
	}
	return size
}

// useMultipart reports whether a Put of n known bytes goes as parts.
func (d *GeyserDriver) useMultipart(n int64) bool {
	return d.put.multipartMin > 0 && n >= d.put.multipartMin && n > d.put.partMin
}

// geyserParts is the driver's part memory: the budget every upload shares
// and one buffer pool per part size.
type geyserParts struct {
	budget *semaphore.Weighted
	limit  int64
	pools  sync.Map // int64 → *sync.Pool of *[]byte
}

func newGeyserParts(budget int64) *geyserParts {
	return &geyserParts{budget: semaphore.NewWeighted(budget), limit: budget}
}

// take waits for size bytes of the budget (all of it for a part larger
// than the budget) and returns a buffer of that size with its release.
func (p *geyserParts) take(ctx context.Context, size int64) ([]byte, func(), error) {
	weight := min(size, p.limit)
	if err := p.budget.Acquire(ctx, weight); err != nil {
		return nil, nil, err
	}
	pv, _ := p.pools.LoadOrStore(size, &sync.Pool{})
	pool := pv.(*sync.Pool)
	var buf []byte
	if b, ok := pool.Get().(*[]byte); ok {
		buf = *b
	} else {
		buf = make([]byte, size)
	}
	return buf, func() {
		b := buf[:cap(buf)]
		pool.Put(&b)
		p.budget.Release(weight)
	}, nil
}

// putMultipart sends n bytes of data as a parallel multipart upload.
func (d *GeyserDriver) putMultipart(ctx context.Context, tenantID, key string, data io.Reader, n int64) (err error) {
	partSize := d.put.partSizeFor(n)
	d.logger.Debug("geyser put (multipart)",
		zap.String("tenant_id", tenantID),
		zap.String("bucket", d.bucket),
		zap.String("key", key),
		zap.Int64("size", n),
		zap.Int64("part_size", partSize),
		zap.Int("concurrency", d.put.concurrency),
	)
	start := time.Now()
	defer func() {
		if err != nil {
			geyserPutMultipart.WithLabelValues("error").Inc()
			return
		}
		geyserPutMultipart.WithLabelValues("ok").Inc()
		observeGeyserPut("multipart", n, start)
	}()

	callOpts := append(sdkCallOptions(ctx), geyserChecksumWhenRequired)
	created, err := d.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	}, callOpts...)
	if err != nil {
		return fmt.Errorf("geyser multipart put %s: create: %w", key, err)
	}
	uploadID := created.UploadId
	defer func() {
		if err != nil {
			d.abortMultipart(ctx, key, uploadID)
		}
	}()

	g, gctx := errgroup.WithContext(ctx)
	slots := make(chan struct{}, d.put.concurrency)
	var (
		mu    sync.Mutex
		parts []types.CompletedPart
	)
	var total int64
	for num := int32(1); total < n; num++ {
		size := min(partSize, n-total)
		select {
		case slots <- struct{}{}:
		case <-gctx.Done():
			_ = g.Wait()
			return fmt.Errorf("geyser multipart put %s: %w", key, context.Cause(gctx))
		}
		buf, release, terr := d.parts.take(gctx, partSize)
		if terr != nil {
			<-slots
			_ = g.Wait()
			return fmt.Errorf("geyser multipart put %s: %w", key, terr)
		}
		got, rerr := io.ReadFull(data, buf[:size])
		if rerr != nil {
			release()
			<-slots
			if werr := g.Wait(); werr != nil {
				return fmt.Errorf("geyser multipart put %s: %w", key, werr)
			}
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				return fmt.Errorf("geyser multipart put %s: body ended after %d of %d declared bytes: %w",
					key, total+int64(got), n, io.ErrUnexpectedEOF)
			}
			return fmt.Errorf("geyser multipart put %s: read body: %w", key, rerr)
		}
		total += size
		part, pnum := buf[:size], num
		g.Go(func() error {
			defer func() { release(); <-slots }()
			out, err := d.client.UploadPart(gctx, &s3.UploadPartInput{
				Bucket:        aws.String(d.bucket),
				Key:           aws.String(key),
				UploadId:      uploadID,
				PartNumber:    aws.Int32(pnum),
				Body:          bytes.NewReader(part),
				ContentLength: aws.Int64(int64(len(part))),
			}, callOpts...)
			if err != nil {
				return fmt.Errorf("part %d: %w", pnum, err)
			}
			mu.Lock()
			parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(pnum), ETag: out.ETag})
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return fmt.Errorf("geyser multipart put %s: %w", key, err)
	}
	sort.Slice(parts, func(i, j int) bool { return *parts[i].PartNumber < *parts[j].PartNumber })
	if _, err := d.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(d.bucket),
		Key:             aws.String(key),
		UploadId:        uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}, callOpts...); err != nil {
		return fmt.Errorf("geyser multipart put %s: complete: %w", key, err)
	}
	return nil
}

// abortMultipart aborts a failed upload on a context of its own: the
// caller's is often the reason it failed.
func (d *GeyserDriver) abortMultipart(ctx context.Context, key string, uploadID *string) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), geyserAbortTimeout)
	defer cancel()
	if _, err := d.client.AbortMultipartUpload(actx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(d.bucket),
		Key:      aws.String(key),
		UploadId: uploadID,
	}); err != nil {
		d.logger.Warn("geyser multipart abort failed (the bucket's lifecycle rule aborts it after 7 days)",
			zap.String("key", key), zap.Error(err))
	}
}

// geyserChecksumWhenRequired turns off the SDK's default request checksums.
func geyserChecksumWhenRequired(o *s3.Options) {
	o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
}

func observeGeyserPut(path string, n int64, start time.Time) {
	if s := time.Since(start).Seconds(); s > 0 {
		geyserPutBytesPerSecond.WithLabelValues(path).Observe(float64(n) / s)
	}
}
