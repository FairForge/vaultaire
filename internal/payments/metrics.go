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

// providerReady is 1 while a provider can take a payment (configured ∧
// verified), recorded every 60 s by Registry.StartReadiness and at
// Register, so every registered provider's series exists from boot.
var providerReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "vaultaire_payment_provider_ready",
	Help: "1 while the payment provider can take a payment (configured and verified — Stripe: key set and the house prices match prices.json), else 0 — also 0 while the provider is pending (configured, its verification not completed yet; a pending minute after a boot is not an outage).",
}, []string{"provider"})

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
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{paymentsTotal, providerReady}
}
