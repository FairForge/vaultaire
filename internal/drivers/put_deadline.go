package drivers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// The per-PUT deadline of the fixed-bucket driver (WP-VAULT-1 part 4).
//
// Wasabi stalled about 4 % of PUTs for 9–123 s in both regions (bench
// 2026-10-04, F4): the request is accepted and then nothing comes back. A
// stall is not an error, so the SDK's retryer never sees it, and the tuned
// transport sets no response timeout for PUT bodies. AWS's own guidance for
// stall-prone S3 endpoints is an aggressive per-request timeout and a retry
// on a NEW connection (§13.1).
//
// deadlinePutClient is the uploader's view of the S3 client. It gives every
// PutObject and every UploadPart — the two calls that carry bytes — a
// deadline of FIXED_BUCKET_PUT_TIMEOUT per 64 MiB of that request's body
// (default 60 s; a 16 MiB part gets 60 s), and when the deadline passes,
// ONE retry of the same request on a connection opened for it:
//
//   - the same request: an UploadPart goes again with the same part number
//     under the same upload id, which S3 defines as replacing that part. The
//     retry is never around the whole upload (a streamed body cannot be
//     replayed) and never mints a new part, so a part is never doubled.
//   - only for silence: an error answer is the SDK retryer's business, and a
//     caller that has gone (its own context is done) gets nothing replayed.
//   - only when the body can be rewound (the single-PUT path and the
//     manager's part buffers are seekable); otherwise the deadline error is
//     returned as it is.
//   - counted in vaultaire_driver_put_retries_total{driver}; a second
//     deadline is the caller's error ("after one retry on a fresh
//     connection") and the engine's failover takes it from there.

const (
	fixedBucketPutTimeoutDefault = 60 * time.Second
	// putDeadlineUnit is the body size the base timeout covers.
	putDeadlineUnit = 64 << 20
)

var driverPutRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_driver_put_retries_total",
	Help: "PutObject / UploadPart requests of a fixed-bucket driver that passed their deadline and were retried once on a fresh connection, by driver.",
}, []string{"driver"})

// fixedBucketPutTimeoutFromEnv reads FIXED_BUCKET_PUT_TIMEOUT: a Go
// duration between 1 s and 1 h (the time allowed per 64 MiB of one PUT's
// body), or `0` / `off` to disable the deadline. Anything else is logged at
// Warn and the default kept (the R13-19 rule).
func fixedBucketPutTimeoutFromEnv(logger *zap.Logger) time.Duration {
	v := strings.TrimSpace(os.Getenv("FIXED_BUCKET_PUT_TIMEOUT"))
	switch strings.ToLower(v) {
	case "":
		return fixedBucketPutTimeoutDefault
	case "0", "off":
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Second || d > time.Hour {
		logger.Warn("invalid FIXED_BUCKET_PUT_TIMEOUT (need a duration 1s..1h such as 60s, or 0/off), keeping default",
			zap.String("value", v), zap.Duration("default", fixedBucketPutTimeoutDefault))
		return fixedBucketPutTimeoutDefault
	}
	return d
}

// putDeadline scales the base by the body size: base per started 64 MiB, at
// least one unit (an unknown size gets the base).
func putDeadline(base time.Duration, size int64) time.Duration {
	if size <= putDeadlineUnit {
		return base
	}
	units := (size + putDeadlineUnit - 1) / putDeadlineUnit
	return base * time.Duration(units)
}

// deadlinePutClient implements manager.UploadAPIClient over the driver's
// client: the multipart bookkeeping calls pass through, the two calls that
// carry bytes get the deadline and the one retry.
type deadlinePutClient struct {
	*s3.Client
	driver string
	base   time.Duration // 0 = no deadline
	fresh  *http.Client  // a client that never reuses a connection
	logger *zap.Logger
}

// bodySize is the length of a request body: the declared one, else what a
// seekable body says, else 0 (unknown).
func bodySize(declared *int64, body io.Reader) int64 {
	if declared != nil && *declared > 0 {
		return *declared
	}
	if s, ok := body.(io.Seeker); ok {
		cur, err := s.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0
		}
		end, err := s.Seek(0, io.SeekEnd)
		if err != nil {
			return 0
		}
		if _, err := s.Seek(cur, io.SeekStart); err != nil {
			return 0
		}
		return end - cur
	}
	return 0
}

// attempt runs call under the deadline and, when the deadline (not the
// caller) ended it, once more on a fresh connection with the body rewound.
func (c *deadlinePutClient) attempt(ctx context.Context, what string, size int64, body io.Reader,
	call func(ctx context.Context, opts ...func(*s3.Options)) error) error {
	if c.base <= 0 {
		return call(ctx)
	}
	timeout := putDeadline(c.base, size)
	seeker, rewindable := body.(io.Seeker)
	var start int64
	if rewindable {
		off, err := seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			rewindable = false
		}
		start = off
	}

	actx, cancel := context.WithTimeout(ctx, timeout)
	err := call(actx)
	ours := err != nil && actx.Err() != nil && errors.Is(actx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancel()
	if err == nil || !ours {
		return err
	}
	if !rewindable {
		return fmt.Errorf("%s: no answer within %s and the body cannot be rewound for a retry: %w", what, timeout, err)
	}
	if _, serr := seeker.Seek(start, io.SeekStart); serr != nil {
		return fmt.Errorf("%s: no answer within %s and the rewind failed (%w): %w", what, timeout, serr, err)
	}
	driverPutRetries.WithLabelValues(c.driver).Inc()
	c.logger.Warn("put deadline passed, retrying once on a fresh connection",
		zap.String("driver", c.driver), zap.String("request", what), zap.Duration("deadline", timeout), zap.Int64("bytes", size))

	rctx, rcancel := context.WithTimeout(ctx, timeout)
	defer rcancel()
	if rerr := call(rctx, func(o *s3.Options) { o.HTTPClient = c.fresh }); rerr != nil {
		if rctx.Err() != nil && errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("%s: no answer within %s, twice (after one retry on a fresh connection): %w", what, timeout, context.DeadlineExceeded)
		}
		return fmt.Errorf("%s: after one retry on a fresh connection: %w", what, rerr)
	}
	return nil
}

// PutObject is a whole object in one request.
func (c *deadlinePutClient) PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	var out *s3.PutObjectOutput
	what := "PutObject " + aws.ToString(in.Key)
	err := c.attempt(ctx, what, bodySize(in.ContentLength, in.Body), in.Body, func(ctx context.Context, extra ...func(*s3.Options)) error {
		var cerr error
		out, cerr = c.Client.PutObject(ctx, in, append(append([]func(*s3.Options){}, opts...), extra...)...)
		return cerr
	})
	return out, err
}

// UploadPart is one part of a multipart upload: a retry sends the same part
// number under the same upload id.
func (c *deadlinePutClient) UploadPart(ctx context.Context, in *s3.UploadPartInput, opts ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	var out *s3.UploadPartOutput
	what := fmt.Sprintf("UploadPart %s part %d", aws.ToString(in.Key), aws.ToInt32(in.PartNumber))
	err := c.attempt(ctx, what, bodySize(in.ContentLength, in.Body), in.Body, func(ctx context.Context, extra ...func(*s3.Options)) error {
		var cerr error
		out, cerr = c.Client.UploadPart(ctx, in, append(append([]func(*s3.Options){}, opts...), extra...)...)
		return cerr
	})
	return out, err
}
