package dashboard

import "github.com/prometheus/client_golang/prometheus"

// Dashboard sign-in signal (pre-launch checklist item 2). The S3 path has
// vaultaire_auth_failures_total (R11-10); the dashboard had nothing, so a
// credential-stuffing run against the login form was a stream of 401 log
// lines. Registered on the server's registry by api.initMetrics; rules in
// deploy/monitoring/vaultaire-auth.yml.
var (
	// LoginFailures counts rejected dashboard sign-ins by reason:
	// bad_password | unknown_user | locked | bad_code | replayed_code.
	LoginFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_dashboard_login_failures_total",
		Help: "Rejected dashboard sign-ins by reason (password form, second factor).",
	}, []string{"reason"})

	// LoginLockouts counts accounts locked after repeated failures.
	LoginLockouts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vaultaire_dashboard_login_lockouts_total",
		Help: "Dashboard accounts locked after repeated failed sign-ins.",
	})
)

// Collectors returns the dashboard's Prometheus collectors for registration.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{LoginFailures, LoginLockouts}
}
