package api

import (
	"context"
	"net"
	"sort"
	"strings"
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
//   - sync            → the WebDAV driver's HealthCheck (an authenticated
//     PROPFIND of EVERY bridge: a wrong bridge password is a 401; healthy
//     while one bridge is up, each bridge in vaultaire_webdav_bridge_up)
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

	driverProbe := func(name string) func(ctx context.Context) error {
		return func(ctx context.Context) error { return eng.CheckDriver(ctx, name) }
	}

	for i := range checks {
		if checks[i].name != "lyve" {
			continue
		}
		// Console action (root-only) on the dedicated probe pair; otherwise the
		// driver's signed HeadBucket, which a scoped service user can do
		// (R7-02/R7-19). The TCP dial remains only when neither exists.
		if ak, sk, customer := lyveProbeCredentials(getenv); ak != "" && sk != "" {
			client := drivers.NewLyveConsoleClient(ak, sk)
			checks[i].probe = func(ctx context.Context) error { return client.CustomerDetails(ctx, customer) }
			checks[i].interval = lyveConsoleProbeInterval
			checks[i].timeout = lyveConsoleProbeTimeout
		} else if registered["lyve"] {
			checks[i].probe = driverProbe("lyve")
		}
	}

	// envKey "" = probe whenever the driver is registered. `local` is always
	// registered (hub disk); its HealthCheck is a stat of DATA_PATH, which is
	// what catches an unmounted or vanished data directory — and without a
	// probe state the admin backends page rendered it as "unhealthy" forever.
	for _, d := range []struct{ name, envKey string }{
		{"local", ""},
		{"idrive", "IDRIVE_ACCESS_KEY"},
		{"wasabi", "WASABI_ACCESS_KEY"},
		{"geyser", "GEYSER_ACCESS_KEY"},
		{"r2", "R2_ACCESS_KEY"},
		{"permafrost", "TENANT_1_ID"},
		{"sync", "SYNC_WEBDAV_PASSWORD|SYNC_WEBDAV_PASSWORDS"},
	} {
		if !anyEnvSet(getenv, d.envKey) || !registered[d.name] {
			continue
		}
		checks = append(checks, backendCheck{name: d.name, probe: driverProbe(d.name)})
	}

	// Region-pinned iDrive drivers (R1-12 / WP-R1-6): probed only when the
	// region has its own key pair — the reseller account mints one per
	// region, and a region left on the primary pair is a known 403 (R7-01)
	// that would page forever. Starts are staggered across one interval so
	// twelve HeadBuckets do not fire in the same second.
	var regions []string
	for name := range registered {
		if !strings.HasPrefix(name, "idrive-") {
			continue
		}
		region := strings.TrimPrefix(name, "idrive-")
		ak := getenv(drivers.IDriveRegionEnvKey(region, "ACCESS_KEY"))
		sk := getenv(drivers.IDriveRegionEnvKey(region, "SECRET_KEY"))
		if ak != "" && sk != "" {
			regions = append(regions, name)
		}
	}
	sort.Strings(regions)
	for i, name := range regions {
		checks = append(checks, backendCheck{
			name:         name,
			probe:        driverProbe(name),
			initialDelay: defaultProbeInterval * time.Duration(i+1) / time.Duration(len(regions)+1),
		})
	}

	return checks
}

// anyEnvSet: keys is "" (always) or env names separated by "|", one of
// which must be set.
func anyEnvSet(getenv func(string) string, keys string) bool {
	if keys == "" {
		return true
	}
	for _, k := range strings.Split(keys, "|") {
		if getenv(k) != "" {
			return true
		}
	}
	return false
}

// lyveProbeCredentials returns the key pair + customer id for the console
// probe. RSCustomerDetails is ROOT-only, so it runs on LYVE_PROBE_* alone:
// the data-plane LYVE_* pair is (or is about to be) a scoped service user
// that the console refuses, and using it here would turn the key rotation
// into a false alert. With no probe pair the Lyve probe is the driver's
// signed HeadBucket (see buildBackendProbes).
func lyveProbeCredentials(getenv func(string) string) (accessKey, secretKey, customer string) {
	accessKey = getenv("LYVE_PROBE_ACCESS_KEY")
	secretKey = getenv("LYVE_PROBE_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		accessKey, secretKey = "", ""
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
