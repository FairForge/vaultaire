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

// Reminder outcomes on vaultaire_payment_reminders_total.
const (
	OutcomeSent = "sent"
)

// remindersTotal counts renewal reminders by step and outcome (WP-PAY-3).
var remindersTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_payment_reminders_total",
	Help: "Renewal reminder e-mails by step (t-14d, t-3d, t+1d, last-day) and outcome (sent, failed). Every pair exists at 0 from boot.",
}, []string{"step", "outcome"})

// Webhook outcomes on vaultaire_payment_webhook_total.
const (
	WebhookOK        = "ok"
	WebhookDuplicate = "duplicate"
	WebhookRejected  = "rejected" // a bad signature, a body over the cap
	WebhookError     = "error"    // 500: left for the provider to retry
)

// webhookTotal counts provider webhook deliveries by outcome: the series
// PaymentWebhookFailing reads (no per-route HTTP series exists — D-P20-A2).
var webhookTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_payment_webhook_total",
	Help: "Payment-provider webhook deliveries by provider and outcome: ok (applied), duplicate (already applied), rejected (bad signature or body), error (500, the provider retries).",
}, []string{"provider", "outcome"})

// providerEnabledDesc: 1 while the provider's pay_<name> flag is on
// globally (its global row or default), read from the flags on scrape:
// PaymentProviderDown ANDs it with the readiness gauge so a provider nobody
// may use is never paged for.
var providerEnabledDesc = prometheus.NewDesc("vaultaire_payment_provider_enabled",
	"1 while new business with the payment provider is allowed globally (flag pay_<provider> on by its global row or default), else 0.",
	[]string{"provider"}, nil)

// EnabledCollector exports vaultaire_payment_provider_enabled for every
// registered provider from the flag source on scrape.
type EnabledCollector struct {
	names   func() []string
	enabled func(provider string) bool
}

// NewEnabledCollector builds one over the registry's names and the global
// flag state (flags.Enabled(pay_<name>, "")).
func NewEnabledCollector(names func() []string, enabled func(provider string) bool) *EnabledCollector {
	return &EnabledCollector{names: names, enabled: enabled}
}

// Describe implements prometheus.Collector.
func (c *EnabledCollector) Describe(ch chan<- *prometheus.Desc) { ch <- providerEnabledDesc }

// Collect implements prometheus.Collector.
func (c *EnabledCollector) Collect(ch chan<- prometheus.Metric) {
	for _, n := range c.names() {
		v := 0.0
		if c.enabled(n) {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(providerEnabledDesc, prometheus.GaugeValue, v, n)
	}
}

func init() {
	// Every Stripe charge outcome exists at 0 from boot, so a first failure
	// is an increase a rule can see (the RP14-9 lesson: a counter born at 1
	// is never an increase).
	for _, o := range []string{OutcomeSettled, OutcomeFailed, OutcomeReplayed} {
		paymentsTotal.WithLabelValues(ProviderStripe, KindCharge, o)
	}
	// A refund is labelled by the CHARGE's provider (RP24-3), so both
	// providers' refund series exist at 0 too.
	for _, p := range []string{ProviderStripe, ProviderManual} {
		paymentsTotal.WithLabelValues(p, KindRefund, OutcomeSettled)
	}
	for _, s := range ReminderSteps {
		for _, o := range []string{OutcomeSent, OutcomeFailed} {
			remindersTotal.WithLabelValues(string(s), o)
		}
	}
	for _, o := range []string{WebhookOK, WebhookDuplicate, WebhookRejected, WebhookError} {
		webhookTotal.WithLabelValues(ProviderStripe, o)
	}
}

// CountReminder counts one reminder outcome.
func CountReminder(step ReminderStep, outcome string) {
	remindersTotal.WithLabelValues(string(step), outcome).Inc()
}

// CountWebhook counts one webhook delivery outcome.
func CountWebhook(provider, outcome string) { webhookTotal.WithLabelValues(provider, outcome).Inc() }

// CountPayment counts one ledger write. Callers count AFTER their
// transaction committed — a rolled-back settle is not a payment.
func CountPayment(provider, kind, outcome string) {
	paymentsTotal.WithLabelValues(provider, kind, outcome).Inc()
}

// Collectors are the package's series, registered by
// internal/api/prom_metrics.go.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{paymentsTotal, providerReady, remindersTotal, webhookTotal}
}
