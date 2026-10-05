package drivers

import (
	"context"
	"fmt"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// A fixed-bucket driver (iDrive, Lyve, Geyser, R2, permafrost) keys every
// object `t-<tenant>/<container>/<artifact>` with the tenant taken from the
// CONTEXT of the call. A call whose context carried no tenant used to fall
// back to the id "default": a read looked under `t-default/` and answered
// "not found" for an object that exists, a write landed where no tenant-keyed
// read would ever find it, and a delete "succeeded" on a key that was never
// there (on S3 a delete of a missing key is a 204). That is how dedup GC
// deleted index rows and left every blob in place (WP-R8-7), and how the CDN
// 404'd public objects (2026-07-31).
//
// Nothing in the product makes such a call any more (the audit is in
// docs/reviews/WP-R8-7.md), so it is refused: an error the caller sees, an
// Error line, and a counter that starts at 0 for every registered backend.

// ErrNoTenant is returned by a fixed-bucket driver for a call whose context
// names no tenant. It wraps engine.ErrInvalidInput: a caller's bug, never a
// miss and never a charge on the backend's circuit breaker.
var ErrNoTenant = fmt.Errorf("%w: the context of the call names no tenant (common.WithTenantID, or engine.ChunkContext for a chunk blob)", engine.ErrInvalidInput)

var callsWithoutTenant = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_driver_calls_without_tenant_total",
	Help: "Driver calls refused because their context named no tenant (they used to land under t-default/), by backend.",
}, []string{"backend"})

// Collectors are the package's Prometheus collectors, for the server's
// registry.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{callsWithoutTenant, geyserGetRanges, geyserGetBytesPerSecond, driverPutRetries}
}

// InitTenantlessSeries creates the refused-call series at 0 for a backend, so
// the first refusal is an increase a rule can see.
func InitTenantlessSeries(backend string) {
	callsWithoutTenant.WithLabelValues(backend)
}

// contextTenant is the tenant the context names ("" when none).
func contextTenant(ctx context.Context) string {
	tid, _ := ctx.Value(common.TenantIDKey).(string)
	return tid
}

// requireTenant returns the tenant a call is made for. configured is the
// driver's own default (tools build Lyve and Geyser drivers with one; the
// server passes none); with neither the call is refused.
func requireTenant(ctx context.Context, backend, op, configured string, logger *zap.Logger) (string, error) {
	if tid := contextTenant(ctx); tid != "" {
		return tid, nil
	}
	if configured != "" {
		return configured, nil
	}
	callsWithoutTenant.WithLabelValues(backend).Inc()
	if logger != nil {
		logger.Error("driver call refused: its context names no tenant — it would have addressed t-default/",
			zap.String("backend", backend), zap.String("op", op))
	}
	return "", fmt.Errorf("%s %s: %w", backend, op, ErrNoTenant)
}

// tenantKey is the key shape of every fixed-bucket driver.
func tenantKey(tenantID, container, artifact string) string {
	return fmt.Sprintf("t-%s/%s/%s", tenantID, container, artifact)
}
