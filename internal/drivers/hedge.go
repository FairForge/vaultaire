package drivers

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
)

// The hedge (WP-R6-3). On 2026-10-17 Wasabi's edge lost ~2.5 % of flows per
// region per minute — the SYN never answered, or TLS done and then
// silence — on fresh and pooled connections alike, from SLC and New York at
// different minutes; Lyve us-east-1 answered nothing to ~1–3 % of small
// PUTs. A stalled call is not an error, so neither the SDK's retryer nor
// the breaker sees it until a header timeout passes. Sending the same small
// idempotent call again on a FRESH connection after DRIVER_HEDGE_AFTER
// (default 2 s), first answer wins and the other is cancelled, answered
// 200 of 200 Wasabi calls, max 2.4 s, where the probe's 15 s deadline had
// failed 199 times in 2.7 days (cmd/tools/wasabi-stall -mode hedged).
//
// Hedged: the probe (HeadBucket, and the canary's HeadObject through
// Exists), HeadObject/Exists, a listing page and DeleteObject — no body,
// idempotent. Never a PutObject or UploadPart (a hedge would send the bytes
// twice; deadlinePutClient's one retry on a fresh connection is the rule
// there), never a GetObject (a body), never a call whose caller has gone.
// The cost: a call that has not answered in 2 s — 4–5.7 % of first
// attempts on Wasabi in its bad minutes, 0 on iDrive's new account — is
// sent twice. A hedged DeleteObject on a VERSIONED bucket would leave two
// delete markers: no backend bucket of ours is versioned (R7-16).
//
// An answer is a response of any status (a 404 is the object's verdict, a
// 5xx the backend's): the first one is taken. A transport error or a
// header timeout is no answer: the other attempt, while it runs, may still
// bring one. Both without an answer = the first attempt's error.

const driverHedgeAfterDefault = 2 * time.Second

var (
	driverHedges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_driver_hedges_total",
		Help: "Small idempotent driver calls (head_bucket = the probe, head_object, list, delete; get = a Swift GET's headers) sent a second time on a fresh connection because the first had not answered after DRIVER_HEDGE_AFTER, by driver and op.",
	}, []string{"driver", "op"})
	driverHedgesWon = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_driver_hedges_won_total",
		Help: "Hedged driver calls the hedge (the second attempt, on a fresh connection) answered first, by driver and op. A rising share of hedges_total = that backend's edge is stalling.",
	}, []string{"driver", "op"})
)

// hedgeOps are the op labels a hedger counts under.
var hedgeOps = []string{"head_bucket", "head_object", "list", "delete"}

// driverHedgeAfterFromEnv reads DRIVER_HEDGE_AFTER: a Go duration between
// 200 ms and 30 s, or `0` / `off` (no hedge). Anything else is logged at
// Warn and the default kept (R13-19).
func driverHedgeAfterFromEnv(logger *zap.Logger) time.Duration {
	v := strings.TrimSpace(os.Getenv("DRIVER_HEDGE_AFTER"))
	switch strings.ToLower(v) {
	case "":
		return driverHedgeAfterDefault
	case "0", "off":
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 200*time.Millisecond || d > 30*time.Second {
		logger.Warn("invalid DRIVER_HEDGE_AFTER (need a duration 200ms..30s such as 2s, or 0/off), keeping default",
			zap.String("value", v), zap.Duration("default", driverHedgeAfterDefault))
		return driverHedgeAfterDefault
	}
	return d
}

// s3Hedger hedges one driver's small calls. nil = no hedge.
type s3Hedger struct {
	driver string        // the metrics label: the engine's backend name
	after  time.Duration // 0 = no hedge
	fresh  *http.Client  // the hedge's client: keep-alives off
}

// newS3Hedger builds a hedger and creates its series at 0.
func newS3Hedger(driver string, after time.Duration, fresh *http.Client) *s3Hedger {
	initHedgeSeries(driver)
	return &s3Hedger{driver: driver, after: after, fresh: fresh}
}

func initHedgeSeries(driver string) {
	for _, op := range hedgeOps {
		driverHedges.WithLabelValues(driver, op)
		driverHedgesWon.WithLabelValues(driver, op)
	}
}

// relabel moves the hedger's series to the engine's name for the driver
// (a regional iDrive driver calls itself "idrive").
func (h *s3Hedger) relabel(driver string) {
	if h == nil {
		return
	}
	h.driver = driver
	initHedgeSeries(driver)
}

// The SDK's attempts per call (WP-R6-3, R7-13): two on a request path — a
// stalled backend already costs a header bound per attempt, and the breaker
// plus the client's own retry are the policy — three on a background job.
const (
	requestRetryMaxAttempts = 2
	jobRetryMaxAttempts     = 3
)

// sdkCallOptions are the per-call options of an S3 call made under ctx: a
// job's call gets the job's attempts.
func sdkCallOptions(ctx context.Context) []func(*s3.Options) {
	if common.IsJob(ctx) {
		return []func(*s3.Options){func(o *s3.Options) { o.RetryMaxAttempts = jobRetryMaxAttempts }}
	}
	return nil
}

// isAnswer reports whether err carries a response from the backend.
func isAnswer(err error) bool {
	var re *smithyhttp.ResponseError
	return err == nil || errors.As(err, &re)
}

type hedgeResult[T any] struct {
	v     T
	err   error
	hedge bool
}

// hedged runs call (an S3 call taking per-call options) and, when it has not
// answered after h.after, the same call on a fresh connection; the first
// answer wins and the other attempt is cancelled.
func hedged[T any](ctx context.Context, h *s3Hedger, op string, call func(context.Context, ...func(*s3.Options)) (T, error)) (T, error) {
	opts := sdkCallOptions(ctx)
	if h == nil || h.after <= 0 || h.fresh == nil || ctx.Err() != nil {
		return call(ctx, opts...)
	}
	results := make(chan hedgeResult[T], 2)
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	go func() {
		v, err := call(pctx, opts...)
		results <- hedgeResult[T]{v: v, err: err}
	}()

	hctx, hcancel := context.WithCancel(ctx) // the hedge's, if it starts
	defer hcancel()
	timer := time.NewTimer(h.after)
	defer timer.Stop()

	running := 1
	var failed *hedgeResult[T]
	for {
		select {
		case <-timer.C:
			if ctx.Err() != nil {
				continue
			}
			running++
			driverHedges.WithLabelValues(h.driver, op).Inc()
			fresh := append(append([]func(*s3.Options){}, opts...), func(o *s3.Options) { o.HTTPClient = h.fresh })
			go func() {
				v, err := call(hctx, fresh...)
				results <- hedgeResult[T]{v: v, err: err, hedge: true}
			}()
		case r := <-results:
			running--
			if isAnswer(r.err) {
				if r.hedge {
					driverHedgesWon.WithLabelValues(h.driver, op).Inc()
				}
				return r.v, r.err
			}
			if failed == nil || !r.hedge {
				failed = &r
			}
			if running == 0 {
				return failed.v, failed.err
			}
		}
	}
}

// listPages walks every page of a listing, each page hedged (the paginator
// cannot be: it advances its token after each call).
func listPages(ctx context.Context, h *s3Hedger, client *s3.Client, in *s3.ListObjectsV2Input, page func(*s3.ListObjectsV2Output) error) error {
	input := *in
	var prev string
	for {
		out, err := hedged(ctx, h, "list", func(ctx context.Context, o ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
			return client.ListObjectsV2(ctx, &input, o...)
		})
		if err != nil {
			return err
		}
		if err := page(out); err != nil {
			return err
		}
		next := ""
		if out.NextContinuationToken != nil {
			next = *out.NextContinuationToken
		}
		// The paginator's rules: more pages while truncated with a token; a
		// token seen twice ends the walk (StopOnDuplicateToken).
		if out.IsTruncated == nil || !*out.IsTruncated || next == "" || next == prev {
			return nil
		}
		prev = next
		input.ContinuationToken = &next
	}
}
