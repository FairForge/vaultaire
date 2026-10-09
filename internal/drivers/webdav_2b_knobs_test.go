package drivers

import (
	"context"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
)

// setStripeHeartbeat sets how often a running striped upload rewrites its
// key file's heartbeat (Prompt 2b B1).
func setStripeHeartbeat(m *MultiWebDAVDriver, every time.Duration) { m.stripeHeartbeat = every }

func withLen(n int) engine.PutOption { return engine.WithContentLength(int64(n)) }

// expectSize is the API's GET context: the head row's size.
func expectSize(ctx context.Context, n int64) context.Context { return engine.WithExpectedSize(ctx, n) }
