package payments

import "github.com/prometheus/client_golang/prometheus"

// Payment outcomes counted on vaultaire_payments_total.
const (
	OutcomeSettled  = "settled"
	OutcomeFailed   = "failed"
	OutcomeReplayed = "replayed"
)

var paymentsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_payments_total",
	Help: "Money events written to the payments ledger, by provider, kind and outcome (settled, failed, replayed — a notification for a row already settled).",
}, []string{"provider", "kind", "outcome"})

func init() {
	// Every Stripe charge outcome exists at 0 from boot, so a first failure
	// is an increase a rule can see.
	for _, o := range []string{OutcomeSettled, OutcomeFailed, OutcomeReplayed} {
		paymentsTotal.WithLabelValues(ProviderStripe, KindCharge, o)
	}
}

// CountPayment counts one ledger write. Callers count AFTER their
// transaction committed — a rolled-back settle is not a payment.
func CountPayment(provider, kind, outcome string) {
	paymentsTotal.WithLabelValues(provider, kind, outcome).Inc()
}

// Collectors are the package's series, registered by
// internal/api/prom_metrics.go.
func Collectors() []prometheus.Collector { return []prometheus.Collector{paymentsTotal} }
