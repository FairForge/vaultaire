// internal/drivers/throttle.go
package drivers

import (
	"context"
	"io"

	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

type ThrottledDriver struct {
	backend engine.Driver
	limiter *rate.Limiter
	logger  *zap.Logger
}

// NewThrottledDriver creates a driver with bandwidth throttling
func NewThrottledDriver(backend engine.Driver, bytesPerSecond int, logger *zap.Logger) *ThrottledDriver {
	// Create limiter with bytes per second rate and burst size
	limiter := rate.NewLimiter(rate.Limit(bytesPerSecond), bytesPerSecond)

	return &ThrottledDriver{
		backend: backend,
		limiter: limiter,
		logger:  logger,
	}
}

// throttledReader wraps an io.Reader with rate limiting
type throttledReader struct {
	reader  io.Reader
	limiter *rate.Limiter
	ctx     context.Context
}

func (tr *throttledReader) Read(p []byte) (int, error) {
	n, err := tr.reader.Read(p)
	if n > 0 {
		// Wait for permission to read n bytes
		if waitErr := tr.limiter.WaitN(tr.ctx, n); waitErr != nil {
			return 0, waitErr
		}
	}
	return n, err
}

func (t *ThrottledDriver) Put(ctx context.Context, container, artifact string,
	data io.Reader, opts ...engine.PutOption) error {

	// Wrap the reader with throttling
	throttled := &throttledReader{
		reader:  data,
		limiter: t.limiter,
		ctx:     ctx,
	}

	return t.backend.Put(ctx, container, artifact, throttled, opts...)
}

// Delegate other required methods
func (t *ThrottledDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	return t.backend.Get(ctx, container, artifact)
}

func (t *ThrottledDriver) Name() string {
	return "throttled-" + t.backend.Name()
}

func (t *ThrottledDriver) Delete(ctx context.Context, container, artifact string) error {
	return t.backend.Delete(ctx, container, artifact)
}

func (t *ThrottledDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	return t.backend.List(ctx, container, prefix)
}

func (t *ThrottledDriver) HealthCheck(ctx context.Context) error {
	return t.backend.HealthCheck(ctx)
}
