package api

import (
	"github.com/FairForge/vaultaire/internal/dashboard"
	"net/http"
	"sort"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus exposition for /metrics.
//
// The endpoint used to be two hand-written lines, and vaultaire_errors_total
// never incremented — the prod VaultaireHighErrorRate rule could not fire.
// This is a real registry that keeps those two series (same names, same
// meaning; the live rules reference them) and adds the backend series the
// outage alerts are built on. Rules: deploy/monitoring/vaultaire-backends.yml.

// engineMetricsSource is the slice of the engine the collector reads.
type engineMetricsSource interface {
	GetFailoverStatus() map[string]string
	WriteFailures() int64
}

// initMetrics builds the registry once. Safe to call on a bare &Server{}.
func (s *Server) initMetrics() {
	s.metricsOnce.Do(func() {
		reg := prometheus.NewRegistry()
		reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "vaultaire_requests_total",
			Help: "HTTP requests served (all routes).",
		}, func() float64 { return float64(atomic.LoadInt64(&s.requestCount)) }))
		reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "vaultaire_errors_total",
			Help: "HTTP responses with a 5xx status.",
		}, func() float64 { return float64(atomic.LoadInt64(&s.errorCount)) }))

		reg.MustRegister(authFailures, authFailuresByKey)
		reg.MustRegister(dashboard.Collectors()...)
		reg.MustRegister(retentionDeletedRows)
		reg.MustRegister(accountDeletionTenants, accountDeletionObjects, accountDeletionSwept)
		// Background jobs (WP-R13-3): runs by outcome, and the last success of
		// every registered job READ FROM job_runs — a process that has just
		// started reports the success of the process before it. The per-job
		// "last run" gauges this replaces were Set only after a run in this
		// process: they read 0 after every deploy and a staleness rule on them
		// was true until the next nightly run.
		reg.MustRegister(jobRuns)
		if s.jobs != nil {
			reg.MustRegister(newJobsCollector(s.jobs))
		}
		// Egress throttle (WP-R10-9). No tenant label on any of them.
		reg.MustRegister(egressEngaged, egressWouldThrottle, egressRejected, egressThrottledBytes)
		// Reads that waited on an automatic restore (WP-R13-1): their 503 is
		// kept out of vaultaire_errors_total, this is where it shows.
		reg.MustRegister(smartRestoreWaits)
		// Writes destroyed by the delete of a stale copy (R13-06: detected,
		// not preventable before WP-R2-1). Every source is there at 0.
		reg.MustRegister(staleCopyLostWrites)
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "vaultaire_egress_throttled_tenants",
			Help: "Tenants past their monthly egress allowance with the egress_throttle flag on (their downloads are paced).",
		}, func() float64 {
			if s.egress == nil {
				return 0
			}
			return float64(s.egress.throttledTenants())
		}))
		if s.synthetic != nil {
			reg.MustRegister(newSyntheticCollector(s.synthetic))
		}

		var src engineMetricsSource
		if s.engine != nil {
			src = s.engine
		}
		if s.healthChecker != nil {
			reg.MustRegister(newBackendCollector(s.healthChecker, src))
		}
		if s.certMonitor != nil {
			reg.MustRegister(newCertExpiryCollector(s.certMonitor))
		}
		reg.MustRegister(collectors.NewGoCollector())
		reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

		s.promHandler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.initMetrics()
	s.promHandler.ServeHTTP(w, r)
}

// backendCollector exports backend probe state + circuit breakers + durable
// write failures. It reads live state on every scrape, so nothing has to be
// kept in sync with the health loop.
type backendCollector struct {
	hc  *BackendHealthChecker
	eng engineMetricsSource

	health        *prometheus.Desc
	probeFailures *prometheus.Desc
	probeLatency  *prometheus.Desc
	circuitOpen   *prometheus.Desc
	writeFailures *prometheus.Desc
}

func newBackendCollector(hc *BackendHealthChecker, eng engineMetricsSource) *backendCollector {
	return &backendCollector{
		hc:  hc,
		eng: eng,
		health: prometheus.NewDesc("vaultaire_backend_health",
			"1 if the backend's last probe succeeded (authenticated where possible), else 0.",
			[]string{"backend"}, nil),
		probeFailures: prometheus.NewDesc("vaultaire_backend_probe_failures_total",
			"Failed backend probes since process start.",
			[]string{"backend"}, nil),
		probeLatency: prometheus.NewDesc("vaultaire_backend_probe_latency_seconds",
			"Latency of the backend's last probe.",
			[]string{"backend"}, nil),
		circuitOpen: prometheus.NewDesc("vaultaire_backend_circuit_open",
			"1 if the engine's circuit breaker for the backend is not closed (open or half-open).",
			[]string{"backend"}, nil),
		writeFailures: prometheus.NewDesc("vaultaire_backend_write_failures_total",
			"PUTs rejected because every eligible durable backend failed (fail-loudly path).",
			nil, nil),
	}
}

func (c *backendCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.health
	ch <- c.probeFailures
	ch <- c.probeLatency
	ch <- c.circuitOpen
	ch <- c.writeFailures
}

func (c *backendCollector) Collect(ch chan<- prometheus.Metric) {
	states := c.hc.GetBackendStates()
	names := make([]string, 0, len(states))
	for n := range states {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		st := states[n]
		healthy := 0.0
		if st.Healthy {
			healthy = 1
		}
		ch <- prometheus.MustNewConstMetric(c.health, prometheus.GaugeValue, healthy, n)
		ch <- prometheus.MustNewConstMetric(c.probeFailures, prometheus.CounterValue, float64(st.Failures), n)
		ch <- prometheus.MustNewConstMetric(c.probeLatency, prometheus.GaugeValue, st.Latency.Seconds(), n)
	}

	if c.eng == nil {
		return
	}
	statuses := c.eng.GetFailoverStatus()
	bnames := make([]string, 0, len(statuses))
	for n := range statuses {
		bnames = append(bnames, n)
	}
	sort.Strings(bnames)
	for _, n := range bnames {
		open := 0.0
		if statuses[n] != "closed" {
			open = 1
		}
		ch <- prometheus.MustNewConstMetric(c.circuitOpen, prometheus.GaugeValue, open, n)
	}
	ch <- prometheus.MustNewConstMetric(c.writeFailures, prometheus.CounterValue, float64(c.eng.WriteFailures()))
}
