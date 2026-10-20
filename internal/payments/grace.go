package payments

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// DefaultGrace is how long a house is kept after its paid_through date
// before the lapse job clears it (F-26: 14 days; Stripe's dunning window
// must fit inside it).
const DefaultGrace = 14 * 24 * time.Hour

// Grace bounds (ENTITLEMENT_GRACE).
const (
	minGrace = time.Hour
	maxGrace = 60 * 24 * time.Hour
)

// GraceFromEnv reads ENTITLEMENT_GRACE (a Go duration, 1h–60d — 336h is the
// default 14 days). A rejected value is logged at Warn and the default kept,
// the house rule for knobs.
func GraceFromEnv(getenv func(string) string, logger *zap.Logger) time.Duration {
	v := getenv("ENTITLEMENT_GRACE")
	if v == "" {
		return DefaultGrace
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < minGrace || d > maxGrace {
		if logger != nil {
			logger.Warn("ENTITLEMENT_GRACE rejected — keeping the default",
				zap.String("value", v), zap.Duration("default", DefaultGrace),
				zap.Duration("min", minGrace), zap.Duration("max", maxGrace))
		}
		return DefaultGrace
	}
	return d
}

// ReadyHistory answers whether a provider was unready (could not take a
// payment — its readiness gauge down, docs/PAYMENTS.md § 2.3) at any time in
// the window before now. The lapse job skips a tenant whose provider was:
// a house is never lost because our processor was down.
type ReadyHistory interface {
	UnreadyWithin(ctx context.Context, provider string, window time.Duration, now time.Time) bool
}

// AlwaysReady is the history until WP-PAY-2's providers feed one: every
// provider counts as ready.
type AlwaysReady struct{}

// UnreadyWithin is always false.
func (AlwaysReady) UnreadyWithin(context.Context, string, time.Duration, time.Time) bool {
	return false
}
