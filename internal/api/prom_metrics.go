package api

import (
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/billing"
	"github.com/FairForge/vaultaire/internal/dashboard"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/packstore"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
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
		reg.MustRegister(accountDeletionTenants, accountDeletionObjects, accountDeletionSwept, accountDeletionRealms)
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
		// Detached long S3 operations (s3_long_op.go): what a process waits
		// for when it stops, and — from s3_long_op_incidents, written by the
		// slot that stopped — what it gave up on.
		reg.MustRegister(longOpsInFlight, longOpOutcomes, newLongOpIncidentsCollector(s.db, s.log()), deliveriesDropped, deliveriesPending,
			completeReplayLockUndetermined)
		// Webhook attempts with an outbox row (WP-R11-3): delivered on a
		// retry, rescheduled, exhausted — every outcome at 0 from boot.
		reg.MustRegister(webhookRetries)
		// Writes destroyed by the delete of a stale copy (R13-06: detected,
		// not preventable before WP-R2-1). Every source is there at 0.
		reg.MustRegister(staleCopyLostWrites)
		// Chunk blobs (WP-R8-7): reads served from a legacy address (0 after
		// the chunk move — then the fallback can go), and driver calls
		// refused because their context named no tenant (they used to land
		// under t-default/). The second starts at 0 for every backend.
		reg.MustRegister(chunkLegacyReads, chunkPutDetached, chunkPutsInFlightGauge, chunkPutBudgetRefusals, chunkPutBudgetInUse, chunkPutBudgetSize)
		// CopyObject sources whose bytes are not their head row's size
		// (Prompt 2b.3 D2.1): drift, or a concurrent overwrite. 503s.
		reg.MustRegister(copySourceSizeMismatch)
		// Tenant key versions (WP-R8-1): the boot re-wrap, rows no key
		// opens, the re-encrypt job, and the rotation backlog (from the
		// tables).
		reg.MustRegister(tenantKeysRewrapped, tenantKeysUnwrapFailures, keyReencryptChunks, keyReencryptObjects)
		// Wrapped S3 secrets that did not open (plan 43.7): each refused a
		// request with a 500; both tables' series exist from boot.
		reg.MustRegister(auth.CredentialUnwrapFailures)
		// SSE-S3 blobs opened, by format (WP-R8-5): whether any 0x01/0x02
		// blob is still read.
		reg.MustRegister(sseBlobsOpened)
		// Generated objects (export, access log, inventory) refused rather
		// than stored plaintext in an sse_enabled bucket (WP-R8-1 A1).
		reg.MustRegister(generatedObjectRefused)
		// Multipart staging free-space guard (WP-R10-8 Part C).
		reg.MustRegister(multipartStagingRefusals, multipartStagingFree)
		// The edge mover (WP-EDGE-1 Part A): admits by outcome, bytes by
		// data-plane host, the admit reaper.
		reg.MustRegister(dataPlaneAdmits, dataPlaneBytes, edgeAdmitReaperTotal)
		// The edge codec (WP-EDGE-2 Part A): chunks by outcome, bytes by
		// kind, index calls.
		reg.MustRegister(edgeCodecChunks, edgeCodecBytes, edgeCodecIndexCalls)
		// The retired_blobs collector (WP-R2-1 Part C): its outcomes, and
		// the backlog of due rows read from the table.
		reg.MustRegister(retiredBlobsTotal, newRetiredBlobsBacklogCollector(s.db, s.log()))
		// Hydrate-before-serve of large Sia objects (sia_promotion.go).
		reg.MustRegister(siaPromotions, siaPromotionSeconds, newSiaPromotedBytesCollector(s.db, s.log()))
		if s.tenantKeys != nil {
			reg.MustRegister(newKeyRotationPendingCollector(s.db, s.log()))
		}
		reg.MustRegister(largeGetCollectors()...)
		// Request bodies the client did not finish (WP-R2-3): 4xx, never a
		// breaker charge.
		reg.MustRegister(clientBodyErrors)
		// Bucket creations refused for their region (Prompt 10 Part A).
		reg.MustRegister(regionCreateRefused)
		// The entitlement record (WP-PAY-1): ledger writes, houses the lapse
		// job cleared, and — from the table — houses by when they expire.
		reg.MustRegister(payments.Collectors()...)
		// WP-PAY-3: the provider flags on scrape (PaymentProviderDown ANDs
		// readiness with it) and the lapse job's skipped_unready from
		// job_runs.result (EntitlementLapseSkippedUnready).
		if s.payments != nil {
			pr := s.payments
			reg.MustRegister(payments.NewEnabledCollector(pr.Names, func(provider string) bool {
				return s.flags != nil && s.flags.Enabled(payments.FlagPrefix+provider, "")
			}))
		}
		if s.entitlementLapse != nil && s.db != nil {
			reg.MustRegister(newLapseSkippedCollector(s.db, s.entitlementLapse.JobName, s.logger))
		}
		// WP-PAY-5: subscriptions a provider switch took the house from whose
		// stop is not confirmed (RenewalStopsPending).
		if s.db != nil {
			reg.MustRegister(payments.NewRenewalStopsCollector(s.db))
		}
		// WP-CAP-1: the vendor capacity gauges — the last backend_capacity
		// run from job_runs.result and the backend_plans rows, on scrape.
		if s.backendCapacity != nil && s.db != nil {
			reg.MustRegister(newCapacityCollector(s.db, s.backendCapacity.JobName, s.logger))
		}
		// Invalid houses from Stripe, acknowledged not retried (RP20-4).
		reg.MustRegister(billing.Collectors()...)
		reg.MustRegister(entitlementsLapsed)
		// The transparency log (K4): append failures by kind and the
		// signing key (process state), size and newest checkpoint (from the
		// tables on scrape — right after a restart).
		reg.MustRegister(logstore.Collectors()...)
		if s.db != nil {
			reg.MustRegister(logstore.NewStateCollector(s.db, s.log()))
			reg.MustRegister(logstore.NewAnchorStateCollector(s.db, s.log()))
		}
		if s.db != nil {
			reg.MustRegister(newEntitlementsExpiringCollector(s.db, s.log()))
		}
		if b := s.largeGet.budget; b != nil {
			reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
				Name: "vaultaire_large_get_budget_bytes_in_use",
				Help: "Bytes of the process-wide large-GET read-ahead budget (LARGE_GET_PARALLEL_BUDGET_BYTES) taken now.",
			}, func() float64 { return float64(b.InUse()) }))
			reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
				Name: "vaultaire_large_get_budget_bytes",
				Help: "Size of the process-wide large-GET read-ahead budget (LARGE_GET_PARALLEL_BUDGET_BYTES).",
			}, func() float64 { return float64(b.Limit()) }))
		}
		// The Vault parity copy (WP-VAULT-1): shards written, erased, and
		// reads a backend failure sent to the parity.
		reg.MustRegister(vaultParityObjects, vaultParityBytes, vaultParityFallbacks, vaultParityOrphans,
			vaultParityUnprotectedBytes, vaultParityUnprotectedObjects)
		// The pack store (Phase 37): packs sealed, GC, member reads.
		reg.MustRegister(packstore.Collectors()...)
		// Routing truth (WP-R7-5): the counters, and a collector that reads
		// the unknown-backend rows and the last run from the tables.
		reg.MustRegister(routingTruthChecks, routingChunkChecks, routingUnknownReads)
		if s.routingTruth != nil {
			reg.MustRegister(newRoutingCollector(s.routingTruth))
		}
		reg.MustRegister(drivers.Collectors()...)
		reg.MustRegister(engine.Collectors()...)
		if s.engine != nil {
			for _, name := range s.engine.GetDriverNames() {
				drivers.InitTenantlessSeries(name)
			}
		}
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
		if s.dedupGCRunner != nil {
			reg.MustRegister(newDedupGCCollector(s.dedupGCRunner))
		}
		// The chunk orphan sweep (WP-R8-2 B.2): what it did, and — from
		// job_runs.result — the orphan bytes it left.
		if s.chunkOrphans != nil {
			reg.MustRegister(chunkOrphanOutcomes, newChunkOrphanCollector(s.chunkOrphans))
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
	primary       *prometheus.Desc
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
		primary: prometheus.NewDesc("vaultaire_primary_backend",
			"1 for the engine's write primary (STORAGE_MODE); the rules join on it to page on the primary's breaker (Prompt 2b.5 F2.2).",
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
	ch <- c.primary
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
		ch <- prometheus.MustNewConstMetric(c.probeFailures, prometheus.CounterValue, float64(st.Failures), n)
		if !st.Probed {
			// Never probed: Healthy is RegisterBackend's seed — no sample,
			// never a false 1 (RP38-1).
			continue
		}
		healthy := 0.0
		if st.Healthy {
			healthy = 1
		}
		ch <- prometheus.MustNewConstMetric(c.health, prometheus.GaugeValue, healthy, n)
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
	if p, ok := c.eng.(interface{ GetPrimary() string }); ok && p.GetPrimary() != "" {
		ch <- prometheus.MustNewConstMetric(c.primary, prometheus.GaugeValue, 1, p.GetPrimary())
	}
	ch <- prometheus.MustNewConstMetric(c.writeFailures, prometheus.CounterValue, float64(c.eng.WriteFailures()))
}
