package api

import (
	"context"
	"net"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"go.uber.org/zap"
)

// Backend health probes.
//
// Before this file, backend health was a TCP dial of whichever endpoints had
// credentials in the environment — which covered Lyve and Quotaless only. The
// PRIMARY backend (iDrive) and Geyser were never probed, and a revoked access
// key is indistinguishable from a healthy backend to a TCP dial: in Sep 2026
// prod's iDrive key was dead for two weeks while /health said "healthy".
//
// Probes are now authenticated where we can sign:
//   - idrive / geyser → the driver's own HealthCheck (a signed HeadBucket)
//   - lyve            → console RSCustomerDetails with the root key
//     (catches auth/suspension; one 403 retried, see LyveConsoleClient)
//   - anything else   → TCP dial (backend-agnostic fallback)
//
// Results feed BackendHealthChecker (→ /health, /health/backends) and the
// vaultaire_backend_* Prometheus series (→ Alertmanager → ntfy).

const (
	// defaultProbeInterval matches the historical TCP-dial cadence.
	defaultProbeInterval = 30 * time.Second
	// lyveConsoleProbeInterval paces the console action slower than the S3
	// HEADs: it is a management-plane call and per-key throttling there is
	// suspected but unconfirmed.
	lyveConsoleProbeInterval = 60 * time.Second
	// defaultProbeTimeout bounds a single probe. Geyser's S3 client carries a
	// 5-minute response-header timeout; a hung backend must not wedge the loop.
	defaultProbeTimeout = 15 * time.Second
	// lyveConsoleProbeTimeout leaves room for the single 403 retry delay.
	lyveConsoleProbeTimeout = 45 * time.Second
	// tcpDialTimeout is the legacy TCP fallback budget.
	tcpDialTimeout = 3 * time.Second
	// defaultLyveCustomer is our Lyve customer id (RSCustomerDetails argument).
	defaultLyveCustomer = "v01"
)

// driverChecker is the slice of the engine the probes need. *engine.CoreEngine
// satisfies it; tests pass a fake.
type driverChecker interface {
	GetDriverNames() []string
	CheckDriver(ctx context.Context, name string) error
}

// buildBackendProbes decides what gets probed and how. It starts from the
// env-driven TCP list (configuredBackends) and upgrades entries to
// authenticated probes where credentials + a registered driver exist. A
// driver whose construction failed at boot is skipped — that failure is
// already logged loudly, and a probe could only ever say "not found".
func buildBackendProbes(getenv func(string) string, eng driverChecker) []backendCheck {
	checks := configuredBackends(getenv)

	registered := map[string]bool{}
	if eng != nil {
		for _, n := range eng.GetDriverNames() {
			registered[n] = true
		}
	}

	for i := range checks {
		if checks[i].name != "lyve" {
			continue
		}
		ak, sk, customer := lyveProbeCredentials(getenv)
		if ak == "" || sk == "" {
			continue // cannot sign → keep the TCP dial
		}
		client := drivers.NewLyveConsoleClient(ak, sk)
		checks[i].probe = func(ctx context.Context) error { return client.CustomerDetails(ctx, customer) }
		checks[i].interval = lyveConsoleProbeInterval
		checks[i].timeout = lyveConsoleProbeTimeout
	}

	for _, d := range []struct{ name, envKey string }{
		{"idrive", "IDRIVE_ACCESS_KEY"},
		{"geyser", "GEYSER_ACCESS_KEY"},
	} {
		if getenv(d.envKey) == "" || !registered[d.name] {
			continue
		}
		name := d.name
		checks = append(checks, backendCheck{
			name:  name,
			probe: func(ctx context.Context) error { return eng.CheckDriver(ctx, name) },
		})
	}

	return checks
}

// lyveProbeCredentials returns the key pair + customer id for the console
// probe. RSCustomerDetails is ROOT-only, so once prod's data-plane LYVE_* key
// becomes a scoped service user the root key lives in LYVE_PROBE_*; until
// then the data-plane key (which is root today) is used.
func lyveProbeCredentials(getenv func(string) string) (accessKey, secretKey, customer string) {
	accessKey = getenv("LYVE_PROBE_ACCESS_KEY")
	secretKey = getenv("LYVE_PROBE_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		accessKey = getenv("LYVE_ACCESS_KEY")
		secretKey = getenv("LYVE_SECRET_KEY")
	}
	customer = getenv("LYVE_PROBE_CUSTOMER")
	if customer == "" {
		customer = defaultLyveCustomer
	}
	return accessKey, secretKey, customer
}

// probeBackendOnce runs one probe (authenticated if available, else TCP dial)
// under a timeout and records the outcome in the health checker.
func (s *Server) probeBackendOnce(ctx context.Context, b backendCheck) {
	timeout := b.timeout
	if timeout == 0 {
		timeout = defaultProbeTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	var err error
	kind := "authenticated"
	if b.probe != nil {
		err = b.probe(pctx)
	} else {
		kind = "tcp"
		var conn net.Conn
		conn, err = (&net.Dialer{Timeout: tcpDialTimeout}).DialContext(pctx, "tcp", b.address)
		if err == nil {
			_ = conn.Close()
		}
	}
	latency := time.Since(start)

	if err != nil {
		s.logger.Warn("backend health check failed",
			zap.String("backend", b.name),
			zap.String("probe", kind),
			zap.String("address", b.address),
			zap.Error(err),
			zap.Duration("latency", latency))
		s.healthChecker.UpdateHealth(b.name, false, latency, err)
		return
	}
	s.healthChecker.UpdateHealth(b.name, true, latency, nil)
	s.logger.Debug("backend health check OK",
		zap.String("backend", b.name),
		zap.String("probe", kind),
		zap.Duration("latency", latency))
}
