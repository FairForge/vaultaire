package drivers

// pacer is a token bucket a driver waits on — the one shape behind the sia
// budgets (sia_budget.go: bytes per second through s3d) and the permafrost
// fleet's per-account and per-tenant buckets (onedrive_pace.go: requests per
// second and bytes per hour, WP-FLEET-3 D-F3-5). Lifted from siaPacer so the
// two never drift.
//
// A caller waits; it is never refused. Only its context ending stops a wait
// — never the expected wait passing the context's deadline, which
// rate.Limiter.WaitN refuses up front (a detached Complete has a deadline;
// a fleet read under pacing may take minutes by design). A take of more
// than the burst is taken burst by burst.

import (
	"context"
	"fmt"
	"io"
	"time"

	"golang.org/x/time/rate"
)

// pacer is one token bucket. A nil *pacer paces nothing.
type pacer struct {
	what   string // error prefix
	lim    *rate.Limiter
	burst  int
	waited func(time.Duration) // the time one take waited (metrics); may be nil
}

// newPacer returns a bucket of perSec tokens per second holding burst at
// most, nil when perSec ≤ 0.
func newPacer(what string, perSec float64, burst int, waited func(time.Duration)) *pacer {
	if perSec <= 0 {
		return nil
	}
	burst = max(burst, 1)
	return &pacer{what: what, lim: rate.NewLimiter(rate.Limit(perSec), burst), burst: burst, waited: waited}
}

// take waits for n tokens, at most burst at a time.
func (p *pacer) take(ctx context.Context, n int) error {
	if p == nil || n <= 0 {
		return nil
	}
	start := time.Now()
	defer func() {
		if p.waited != nil {
			p.waited(time.Since(start))
		}
	}()
	for n > 0 {
		k := min(n, p.burst)
		now := time.Now()
		r := p.lim.ReserveN(now, k)
		if !r.OK() {
			return fmt.Errorf("%s: %d tokens exceed the burst %d", p.what, k, p.burst)
		}
		if d := r.DelayFrom(now); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				r.Cancel()
				return fmt.Errorf("%s: %w", p.what, ctx.Err())
			case <-t.C:
			}
		}
		n -= k
	}
	return nil
}

// drain empties the bucket: the next take waits for the refill (a 429's
// signal that the budget is spent, D-F3-6, is a pause on every caller of
// the bucket, not on the one request).
func (p *pacer) drain() {
	if p == nil {
		return
	}
	now := time.Now()
	if n := int(p.lim.TokensAt(now)); n > 0 {
		p.lim.ReserveN(now, min(n, p.burst))
	}
}

// reader paces r: each Read asks for at most burst bytes and waits for as
// many tokens as it got.
func (p *pacer) reader(ctx context.Context, r io.Reader) io.Reader {
	if p == nil {
		return r
	}
	return &pacedReader{r: r, ctx: ctx, p: p}
}

// body paces a ReadCloser (Close passes through).
func (p *pacer) body(ctx context.Context, rc io.ReadCloser) io.ReadCloser {
	if p == nil {
		return rc
	}
	return &pacedBody{pacedReader: pacedReader{r: rc, ctx: ctx, p: p}, c: rc}
}

type pacedReader struct {
	r   io.Reader
	ctx context.Context
	p   *pacer
}

func (b *pacedReader) Read(buf []byte) (int, error) {
	if len(buf) > b.p.burst {
		buf = buf[:b.p.burst]
	}
	n, err := b.r.Read(buf)
	if n > 0 {
		if werr := b.p.take(b.ctx, n); werr != nil && err == nil {
			err = werr
		}
	}
	return n, err
}

type pacedBody struct {
	pacedReader
	c io.Closer
}

func (b *pacedBody) Close() error { return b.c.Close() }
