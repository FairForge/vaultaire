package drivers

// The sia.storage account behind s3d has ONE funding budget shared by
// uploads and reads (bench-results/SIA-S3D-2026-10.md): a fast ingest burst
// (~80 MB/s logical, ×2.3–3 on the wire) empties it and starves reads for
// minutes. SIA_READ_BUDGET (sia_read.go) paces what the driver delivers,
// SIA_WRITE_BUDGET (sia_write.go) what it takes in; both are one siaPacer —
// a token bucket in bytes per second, one per driver (one sia driver per
// process).

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

// siaBudgetMaxPerSec is the largest budget either variable accepts.
const siaBudgetMaxPerSec = int64(100 << 30)

// siaBudgetFromEnv reads a budget variable: bytes per second as a size such
// as 25MiB; unset, 0 or off = 0 (not paced). A rejected value is a Warn and
// 0 (the default).
func siaBudgetFromEnv(getenv func(string) string, name, example string, logger *zap.Logger) int64 {
	v := strings.TrimSpace(getenv(name))
	if v == "" || v == "0" || strings.EqualFold(v, "off") {
		return 0
	}
	n, err := parseByteSize(v)
	if err != nil || n <= 0 || n > siaBudgetMaxPerSec {
		logger.Warn("invalid "+name+" (need bytes per second such as "+example+", or off), keeping default (not paced)",
			zap.String("value", v))
		return 0
	}
	return n
}

// siaPacer is one token bucket of bytes. Its burst is a quarter of a second
// of the rate, 32 KiB..4 MiB: a Read never takes more than the bucket can
// ever hold, so every wait is short and a stream keeps moving.
type siaPacer struct {
	what  string // error prefix: "sia read budget" / "sia write budget"
	lim   *rate.Limiter
	burst int
	wait  prometheus.Counter
}

// newSiaPacer returns the pacer of bytesPerSec, nil when it is ≤ 0.
func newSiaPacer(what string, bytesPerSec int64, wait prometheus.Counter) *siaPacer {
	if bytesPerSec <= 0 {
		return nil
	}
	burst := int(min(max(bytesPerSec/4, 32<<10), 4<<20))
	return &siaPacer{what: what, lim: rate.NewLimiter(rate.Limit(bytesPerSec), burst), burst: burst, wait: wait}
}

// take waits for n ≤ burst tokens. Only the context ending stops the wait —
// never the expected wait passing the context's deadline, which
// rate.Limiter.WaitN refuses up front (a detached Complete has a deadline).
func (p *siaPacer) take(ctx context.Context, n int) error {
	start := time.Now()
	r := p.lim.ReserveN(start, n)
	if !r.OK() {
		return fmt.Errorf("%s: %d bytes exceed the burst %d", p.what, n, p.burst)
	}
	if d := r.DelayFrom(start); d > 0 {
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			r.Cancel()
			p.wait.Add(time.Since(start).Seconds())
			return fmt.Errorf("%s: %w", p.what, ctx.Err())
		case <-t.C:
		}
	}
	p.wait.Add(time.Since(start).Seconds())
	return nil
}

// reader paces r: each Read asks for at most burst bytes and waits for as
// many tokens as it got.
func (p *siaPacer) reader(ctx context.Context, r io.Reader) io.Reader {
	return &siaPacedReader{r: r, ctx: ctx, p: p}
}

// body paces a ReadCloser (Close passes through).
func (p *siaPacer) body(ctx context.Context, rc io.ReadCloser) io.ReadCloser {
	return &siaPacedBody{siaPacedReader: siaPacedReader{r: rc, ctx: ctx, p: p}, c: rc}
}

type siaPacedReader struct {
	r   io.Reader
	ctx context.Context
	p   *siaPacer
}

func (b *siaPacedReader) Read(buf []byte) (int, error) {
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

type siaPacedBody struct {
	siaPacedReader
	c io.Closer
}

func (b *siaPacedBody) Close() error { return b.c.Close() }
