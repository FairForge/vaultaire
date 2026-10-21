package drivers

// Reads from the `sia` driver (s3d, the Sia Foundation's S3 gateway) — what
// SIA-BENCH-3 and SIA-BENCH-5 measured (bench-results/SIA-S3D-2026-10.md):
//
//   - A GET answers 200/206 and can then CUT its body: the sia.storage
//     account's per-host read balances ran out (hosts answer "not enough
//     funds"; the balances refill every 5 min, shared by every app of the
//     account). The client sees `unexpected EOF` part way through.
//   - A patched s3d waits for the refill instead of cutting, so the stream
//     can go idle for minutes and then continue.
//   - Reads are budget-limited (the free plan ≈ 25 MB/s sustained). A hedge
//     — a second request for a slow part — spends that budget twice.
//
// So, for the sia driver only (every other fixed-bucket driver is unchanged):
//
//   - **Resume on cut.** The body of Get, GetRange and GetRangeInfo is a
//     siaResumingBody: a read error or a short body part way re-issues the
//     GET from the received offset with `If-Match: <the first answer's
//     ETag>` and uses the answer only when it is a 206 of the same ETag whose
//     Content-Range starts at exactly that offset. A 412, or another ETag, is
//     engine.ErrObjectChanged: the body fails (never two objects spliced) and
//     no breaker is charged. At most SIA_READ_RESUMES re-requests (default 8)
//     within SIA_READ_RESUME_MAX_WAIT of time spent resuming (default 10m),
//     with jittered backoff 1 s, 2 s, 4 s … capped at 60 s; then
//     ErrSiaReadResumeExhausted (a backend failure: the parallel GET charges
//     its breaker once for the read, as for any failed read). A resume is a
//     direct SDK call — the engine's breaker never sees it.
//   - **No hedging.** IDriveDriver.NoHedge (engine.NoHedgeDeclarer) is true:
//     the API's large GETs never re-request a part from sia.
//   - **Read budget pacing** (optional): SIA_READ_BUDGET bytes/s — one token
//     bucket for the driver, i.e. the process (one sia driver per process) —
//     paces the bytes the driver delivers, so Vaultaire asks the account for
//     no more than it can refill instead of running the balances dry.
//
// Metrics: vaultaire_sia_read_resumes_total{outcome=resumed|attempt_failed|
// changed|exhausted}, vaultaire_sia_read_budget_bytes_per_second (0 = off),
// vaultaire_sia_read_budget_wait_seconds_total.

import (
	"context"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/FairForge/vaultaire/internal/engine"
)

const (
	siaReadResumesDefault  = 8
	siaReadResumesMax      = 64
	siaReadMaxWaitDefault  = 10 * time.Minute
	siaReadBackoffBase     = time.Second
	siaReadBackoffCap      = time.Minute
	siaReadBudgetMaxPerSec = int64(100 << 30)
)

// ErrSiaReadResumeExhausted: a sia body was cut and could not be resumed
// within SIA_READ_RESUMES re-requests or SIA_READ_RESUME_MAX_WAIT. It wraps
// the last cause; a backend failure.
var ErrSiaReadResumeExhausted = errors.New("sia read: the body was cut and could not be resumed")

var (
	siaReadResumes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_sia_read_resumes_total",
		Help: "Re-requests of a sia GET body s3d cut part way (drivers/sia_read.go): resumed (the body continued from the received offset), attempt_failed (a re-request failed or was not the asked range; retried), changed (412 / another ETag: the body failed, no breaker charge), exhausted (SIA_READ_RESUMES or SIA_READ_RESUME_MAX_WAIT spent: the body failed).",
	}, []string{"outcome"})
	siaReadBudgetRate = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_sia_read_budget_bytes_per_second",
		Help: "The pace of the bytes the sia driver delivers (SIA_READ_BUDGET); 0 = not paced.",
	})
	siaReadBudgetWait = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vaultaire_sia_read_budget_wait_seconds_total",
		Help: "Seconds sia reads waited on the SIA_READ_BUDGET token bucket.",
	})
)

func init() {
	for _, o := range []string{"resumed", "attempt_failed", "changed", "exhausted"} {
		siaReadResumes.WithLabelValues(o)
	}
}

// siaReadConfig is how the sia driver reads.
type siaReadConfig struct {
	Resumes           int           // re-requests per read; 0 = never resume
	MaxWait           time.Duration // time spent resuming, per read
	BackoffBase       time.Duration // first backoff (doubled per attempt)
	BackoffCap        time.Duration
	BudgetBytesPerSec int64 // 0 = not paced
}

// siaReadConfigFromEnv reads SIA_READ_RESUMES (0..64), SIA_READ_RESUME_MAX_WAIT
// (a duration 1s..1h) and SIA_READ_BUDGET (bytes per second such as 25MiB;
// 0/off = none). A rejected value is a Warn and the default.
func siaReadConfigFromEnv(getenv func(string) string, logger *zap.Logger) siaReadConfig {
	c := siaReadConfig{Resumes: siaReadResumesDefault, MaxWait: siaReadMaxWaitDefault,
		BackoffBase: siaReadBackoffBase, BackoffCap: siaReadBackoffCap}
	if v := strings.TrimSpace(getenv("SIA_READ_RESUMES")); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 0 || n > siaReadResumesMax {
			logger.Warn("invalid SIA_READ_RESUMES (need 0..64), keeping default",
				zap.String("value", v), zap.Int("default", siaReadResumesDefault))
		} else {
			c.Resumes = n
		}
	}
	if v := strings.TrimSpace(getenv("SIA_READ_RESUME_MAX_WAIT")); v != "" {
		if d, err := time.ParseDuration(v); err != nil || d < time.Second || d > time.Hour {
			logger.Warn("invalid SIA_READ_RESUME_MAX_WAIT (need a duration 1s..1h such as 10m), keeping default",
				zap.String("value", v), zap.Duration("default", siaReadMaxWaitDefault))
		} else {
			c.MaxWait = d
		}
	}
	if v := strings.TrimSpace(getenv("SIA_READ_BUDGET")); v != "" && v != "0" && !strings.EqualFold(v, "off") {
		if n, err := parseByteSize(v); err != nil || n <= 0 || n > siaReadBudgetMaxPerSec {
			logger.Warn("invalid SIA_READ_BUDGET (need bytes per second such as 25MiB, or off), keeping default (not paced)",
				zap.String("value", v))
		} else {
			c.BudgetBytesPerSec = n
		}
	}
	return c
}

// siaReads is the sia driver's read behaviour (IDriveDriver.sia; nil on
// every other fixed-bucket driver).
type siaReads struct {
	cfg    siaReadConfig
	budget *rate.Limiter // nil = not paced
	burst  int
}

// setSiaRead installs (or replaces) the sia read behaviour.
func (d *IDriveDriver) setSiaRead(cfg siaReadConfig) {
	s := &siaReads{cfg: cfg}
	if cfg.BudgetBytesPerSec > 0 {
		// A quarter of a second of burst, 32 KiB..4 MiB: a Read never asks
		// the bucket for more than it can ever hold.
		s.burst = int(min(max(cfg.BudgetBytesPerSec/4, 32<<10), 4<<20))
		s.budget = rate.NewLimiter(rate.Limit(cfg.BudgetBytesPerSec), s.burst)
	}
	siaReadBudgetRate.Set(float64(cfg.BudgetBytesPerSec))
	d.sia = s
}

// NoHedge implements engine.NoHedgeDeclarer: the sia driver's reads are
// never hedged by the API (a hedge spends the account's read budget twice).
func (d *IDriveDriver) NoHedge() bool { return d.sia != nil }

// wrapBody is the body a sia read hands out: resumable when the answer
// says where it is (an ETag and a length), then paced. off is the absolute
// offset of the answer's first byte; out is its GetObject output.
func (d *IDriveDriver) wrapBody(ctx context.Context, key string, off int64, out *s3.GetObjectOutput) io.ReadCloser {
	if d.sia == nil {
		return out.Body
	}
	body := out.Body
	if want := aws.ToInt64(out.ContentLength); d.sia.cfg.Resumes > 0 && want > 0 && aws.ToString(out.ETag) != "" {
		body = &siaResumingBody{d: d, ctx: ctx, key: key, body: out.Body, etag: aws.ToString(out.ETag), start: off, want: want}
	}
	if d.sia.budget != nil {
		body = &siaPacedBody{ReadCloser: body, ctx: ctx, s: d.sia}
	}
	return body
}

// siaResumingBody is one sia read that survives cuts (the file comment).
type siaResumingBody struct {
	d     *IDriveDriver
	ctx   context.Context
	key   string
	body  io.ReadCloser
	etag  string // as the first answer gave it (quoted)
	start int64  // absolute offset of the first byte
	want  int64  // bytes the read yields
	got   int64

	attempts int
	spent    time.Duration // time spent resuming
	pending  error         // a cut seen with bytes, handled at the next Read
	err      error         // sticky
}

func (r *siaResumingBody) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.got >= r.want {
		return 0, io.EOF
	}
	if rest := r.want - r.got; int64(len(p)) > rest {
		p = p[:rest]
	}
	for {
		if r.pending != nil {
			cause := r.pending
			r.pending = nil
			if err := r.resume(cause); err != nil {
				r.err = err
				return 0, err
			}
		}
		n, err := r.body.Read(p)
		r.got += int64(n)
		switch {
		case err == nil:
			return n, nil
		case errors.Is(err, io.EOF) && r.got >= r.want:
			return n, io.EOF
		}
		// Cut: a read error, or the body ended short.
		if errors.Is(err, io.EOF) {
			err = fmt.Errorf("%w: the body ended after %d of %d bytes", io.ErrUnexpectedEOF, r.got, r.want)
		}
		if cerr := r.ctx.Err(); cerr != nil {
			r.err = err
			return n, err
		}
		r.pending = err
		if n > 0 {
			return n, nil
		}
	}
}

func (r *siaResumingBody) Close() error {
	if r.body == nil {
		return nil
	}
	return r.body.Close()
}

// resume re-requests the rest of the read; nil = r.body continues it.
func (r *siaResumingBody) resume(cause error) error {
	cfg := r.d.sia.cfg
	began := time.Now()
	defer func() { r.spent += time.Since(began) }()
	_ = r.body.Close()
	for {
		if r.attempts >= cfg.Resumes {
			return r.exhausted(cause)
		}
		wait := siaBackoff(cfg, r.attempts)
		if r.spent+time.Since(began)+wait > cfg.MaxWait {
			return r.exhausted(cause)
		}
		t := time.NewTimer(wait)
		select {
		case <-r.ctx.Done():
			t.Stop()
			return fmt.Errorf("%s resume of %s at %d: %w", r.d.name, r.key, r.start+r.got, r.ctx.Err())
		case <-t.C:
		}
		r.attempts++
		off, last := r.start+r.got, r.start+r.want-1
		out, err := r.d.client.GetObject(r.ctx, &s3.GetObjectInput{
			Bucket:  aws.String(r.d.bucket),
			Key:     aws.String(r.key),
			Range:   aws.String(fmt.Sprintf("bytes=%d-%d", off, last)),
			IfMatch: aws.String(r.etag),
		}, sdkCallOptions(r.ctx)...)
		if err != nil {
			if r.ctx.Err() != nil {
				return fmt.Errorf("%s resume of %s at %d: %w", r.d.name, r.key, off, r.ctx.Err())
			}
			if siaPreconditionFailed(err) {
				return r.changed(off, "412 to If-Match "+r.etag)
			}
			siaReadResumes.WithLabelValues("attempt_failed").Inc()
			cause = err
			continue
		}
		if got := aws.ToString(out.ETag); got != r.etag {
			_ = out.Body.Close()
			return r.changed(off, fmt.Sprintf("ETag %s, the read began on %s", got, r.etag))
		}
		if bad := siaRangeMismatch(out, off, last); bad != "" {
			_ = out.Body.Close()
			siaReadResumes.WithLabelValues("attempt_failed").Inc()
			cause = fmt.Errorf("%s resume of %s at %d: %s", r.d.name, r.key, off, bad)
			continue
		}
		r.body = out.Body
		siaReadResumes.WithLabelValues("resumed").Inc()
		r.d.logger.Info("sia read resumed after a cut body",
			zap.String("key", r.key), zap.Int64("offset", off), zap.Int("attempt", r.attempts), zap.NamedError("cut", cause))
		return nil
	}
}

func (r *siaResumingBody) changed(off int64, why string) error {
	siaReadResumes.WithLabelValues("changed").Inc()
	r.d.logger.Warn("sia read: the object changed before its cut body could be resumed — the body fails",
		zap.String("key", r.key), zap.Int64("offset", off), zap.String("why", why))
	return fmt.Errorf("%s resume of %s at %d (%s): %w", r.d.name, r.key, off, why, engine.ErrObjectChanged)
}

func (r *siaResumingBody) exhausted(cause error) error {
	siaReadResumes.WithLabelValues("exhausted").Inc()
	r.d.logger.Warn("sia read: the body was cut and the resume budget is spent — the body fails",
		zap.String("key", r.key), zap.Int64("offset", r.start+r.got), zap.Int("attempts", r.attempts),
		zap.Duration("spent", r.spent), zap.NamedError("cut", cause))
	return fmt.Errorf("%w (%s at %d after %d re-requests): %w", ErrSiaReadResumeExhausted, r.key, r.start+r.got, r.attempts, cause)
}

// siaBackoff is the wait before re-request n (0-based): base × 2^n, capped,
// ±20 % jitter (the cap is never exceeded).
func siaBackoff(cfg siaReadConfig, n int) time.Duration {
	d := cfg.BackoffBase
	for i := 0; i < n && d < cfg.BackoffCap; i++ {
		d *= 2
	}
	d = min(d, cfg.BackoffCap)
	j := 0.8 + 0.4*mrand.Float64() // #nosec G404 -- jitter for backoff, not security
	return min(time.Duration(float64(d)*j), cfg.BackoffCap)
}

// siaPreconditionFailed: the answer to If-Match was 412.
func siaPreconditionFailed(err error) bool {
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusPreconditionFailed {
		return true
	}
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == "PreconditionFailed"
}

// siaRangeMismatch says why a resume's answer is not bytes [off, last] of
// the object ("" = it is): a 206 whose Content-Range starts at exactly off
// and ends at last.
func siaRangeMismatch(out *s3.GetObjectOutput, off, last int64) string {
	cr := aws.ToString(out.ContentRange)
	if cr == "" {
		return "answered without a Content-Range (not a 206)"
	}
	var a, b int64
	if _, err := fmt.Sscanf(cr, "bytes %d-%d/", &a, &b); err != nil {
		return "unreadable Content-Range " + strconv.Quote(cr)
	}
	if a != off || b != last {
		return fmt.Sprintf("Content-Range %q, asked bytes %d-%d", cr, off, last)
	}
	if n := aws.ToInt64(out.ContentLength); n != 0 && n != last-off+1 {
		return fmt.Sprintf("Content-Length %d, asked %d bytes", n, last-off+1)
	}
	return ""
}

// siaPacedBody paces the bytes the driver delivers on SIA_READ_BUDGET.
type siaPacedBody struct {
	io.ReadCloser
	ctx context.Context
	s   *siaReads
}

func (b *siaPacedBody) Read(p []byte) (int, error) {
	if len(p) > b.s.burst {
		p = p[:b.s.burst]
	}
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		start := time.Now()
		werr := b.s.budget.WaitN(b.ctx, n)
		siaReadBudgetWait.Add(time.Since(start).Seconds())
		if werr != nil && err == nil {
			err = fmt.Errorf("sia read budget: %w", werr)
		}
	}
	return n, err
}
