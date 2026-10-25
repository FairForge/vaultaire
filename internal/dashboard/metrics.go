package dashboard

import "github.com/prometheus/client_golang/prometheus"

// Sign-in signal (pre-launch checklist item 2; Stage 1a Prompt 41 item 1,
// RP57-2). The S3 path has vaultaire_auth_failures_total (R11-10); the
// dashboard had nothing, so a credential-stuffing run against the login
// form was a stream of 401 log lines — and until Prompt 41 the JSON sign-in
// POST /auth/login had nothing either: a second-factor guess from ~100 IPs
// at the per-IP limit was invisible to the auth rules. Both surfaces count
// here now, by surface. Registered on the server's registry by
// api.initMetrics; rules in deploy/monitoring/vaultaire-auth.yml.
var (
	// LoginFailures counts rejected sign-ins by surface and reason.
	LoginFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_dashboard_login_failures_total",
		Help: "Rejected sign-ins by surface (dashboard form, api = POST /auth/login) and reason (password step, second factor, a store failure of ours).",
	}, []string{"surface", "reason"})

	// LoginLockouts counts accounts locked after repeated failures, on
	// either surface (one lockout per account, shared).
	LoginLockouts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vaultaire_dashboard_login_lockouts_total",
		Help: "Accounts locked after repeated failed sign-ins (the dashboard form and POST /auth/login share one lockout).",
	})
)

// LoginSurfaces are the surface label's values: the dashboard's forms and
// the JSON sign-in.
var LoginSurfaces = []string{LoginSurfaceDashboard, LoginSurfaceAPI}

const (
	LoginSurfaceDashboard = "dashboard"
	LoginSurfaceAPI       = "api"
)

// LoginFailureReasons are every reason either surface counts: the password
// step (bad_password, unknown_user, locked), the second factor (code_required
// — the JSON sign-in only —, bad_code, replayed_code) and the two that are
// OURS, never the caller's (store_error: the users read or the password
// check failed; user_load_failed: the password validated and the row could
// not be loaded, RP57-3 / RP58-8 b). Every surface × reason pair exists at 0
// from boot: a rule on a series born at its first event never pages.
var LoginFailureReasons = []string{
	"bad_password", "unknown_user", "locked",
	"code_required", "bad_code", "replayed_code",
	"store_error", "user_load_failed",
}

func init() {
	for _, surface := range LoginSurfaces {
		for _, reason := range LoginFailureReasons {
			LoginFailures.WithLabelValues(surface, reason)
		}
	}
}

// Collectors returns the dashboard's Prometheus collectors for registration.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{LoginFailures, LoginLockouts}
}
