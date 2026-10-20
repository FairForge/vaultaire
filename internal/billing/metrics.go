package billing

import "github.com/prometheus/client_golang/prometheus"

// houseInvalidTotal counts Stripe subscriptions whose items are no valid
// house (payments.ErrInvalidOrder — e.g. pin-hot with no Standard after an
// edit at Stripe): the event is acknowledged and the house left as it was
// (RP20-4). An unlabelled counter, so it is exported at 0 from boot.
var houseInvalidTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "vaultaire_stripe_house_invalid_total",
	Help: "Stripe webhook events whose subscription describes no valid house (pin-hot over Standard, a negative line, an unknown period): acknowledged, the house left as it was.",
})

// Collectors are the package's series, registered by
// internal/api/prom_metrics.go.
func Collectors() []prometheus.Collector { return []prometheus.Collector{houseInvalidTotal} }
