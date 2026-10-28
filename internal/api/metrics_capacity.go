package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// The capacity gauges (WP-CAP-1 B): read from the tables on scrape — the
// last backend_capacity run's result in job_runs (the R13 lesson: never an
// in-process value; right after a restart too) and the backend_plans rows —
// cached 15 s, a failed read keeps the last values. No stored/vendor series
// before the first run; no plan series without a row (BackendPlanUnknown
// fires from their absence, on purpose).

var (
	backendStoredDesc = prometheus.NewDesc("vaultaire_backend_stored_bytes",
		"Bytes our tables say the backend holds, by kind (objects, versions, chunks, multipart, parity, packs) — the last backend_capacity run, read from job_runs.result on scrape.",
		[]string{"backend", "kind"}, nil)
	backendVendorUsedDesc = prometheus.NewDesc("vaultaire_backend_vendor_used_bytes",
		"Bytes the VENDOR's own API says the backend holds (iDrive reseller /users storage_used; a driver's own report — blomp's container HEAD) — the last backend_capacity run.",
		[]string{"backend"}, nil)
	backendVendorLimitDesc = prometheus.NewDesc("vaultaire_backend_vendor_limit_bytes",
		"The most the VENDOR itself holds before it refuses a write (Blomp's container quota, from its HEAD) — the last backend_capacity run. No series where the vendor reports no limit.",
		[]string{"backend"}, nil)
	backendVendorEgressDesc = prometheus.NewDesc("vaultaire_backend_vendor_egress_month_bytes",
		"Egress this UTC month to date by the vendor's own meter (iDrive reseller usage_stats downloads) — the last backend_capacity run.",
		[]string{"backend"}, nil)
	backendPlanBytesDesc = prometheus.NewDesc("vaultaire_backend_plan_bytes",
		"capacity_bytes of the backend's plan row (backend_plans): what we bought before overage. No series without a row — BackendPlanUnknown fires from that.",
		[]string{"backend"}, nil)
	backendPlanTermEndDesc = prometheus.NewDesc("vaultaire_backend_plan_term_end_timestamp_seconds",
		"term_end of the backend's plan row, as a Unix timestamp: the renewal alerts count down to it.",
		[]string{"backend"}, nil)
	backendPlanPaymentDesc = prometheus.NewDesc("vaultaire_backend_plan_payment_on_file",
		"1 while the backend's plan row says a payment method is on file, else 0 (BackendPlanNoPayment pages near term_end without one — the 2026-08-27 shape).",
		[]string{"backend"}, nil)
	backendEgressFreeDesc = prometheus.NewDesc("vaultaire_backend_egress_free_bytes",
		"Free egress this month: the plan row's egress_free_multiple × the stored total of the last backend_capacity run. No series while the vendor does not meter egress (NULL multiple) or before the first run.",
		[]string{"backend"}, nil)
)

type capacitySnapshot struct {
	stored       map[string]*BackendBytes
	vendor       map[string]VendorView
	plans        []BackendPlan
	hasRunResult bool
}

// capacityCollector reads the snapshot on scrape.
type capacityCollector struct {
	db      *sql.DB
	jobName string
	logger  *zap.Logger

	mu   sync.Mutex
	at   time.Time
	snap capacitySnapshot
}

func newCapacityCollector(db *sql.DB, jobName string, logger *zap.Logger) *capacityCollector {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &capacityCollector{db: db, jobName: jobName, logger: logger}
}

func (c *capacityCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{backendStoredDesc, backendVendorUsedDesc, backendVendorLimitDesc, backendVendorEgressDesc,
		backendPlanBytesDesc, backendPlanTermEndDesc, backendPlanPaymentDesc, backendEgressFreeDesc} {
		ch <- d
	}
}

func (c *capacityCollector) Collect(ch chan<- prometheus.Metric) {
	snap := c.read()
	kinds := func(b *BackendBytes) map[string]int64 {
		return map[string]int64{"objects": b.Objects, "versions": b.Versions, "chunks": b.Chunks,
			"multipart": b.Multipart, "parity": b.Parity, "packs": b.Packs, "copies": b.Copies}
	}
	for backend, b := range snap.stored {
		for kind, v := range kinds(b) {
			ch <- prometheus.MustNewConstMetric(backendStoredDesc, prometheus.GaugeValue, float64(v), backend, kind)
		}
	}
	for backend, v := range snap.vendor {
		ch <- prometheus.MustNewConstMetric(backendVendorUsedDesc, prometheus.GaugeValue, float64(v.UsedBytes), backend)
		if v.LimitBytes > 0 {
			ch <- prometheus.MustNewConstMetric(backendVendorLimitDesc, prometheus.GaugeValue, float64(v.LimitBytes), backend)
		}
		if !v.EgressUnmetered {
			ch <- prometheus.MustNewConstMetric(backendVendorEgressDesc, prometheus.GaugeValue, float64(v.EgressMonthBytes), backend)
		}
	}
	for _, p := range snap.plans {
		ch <- prometheus.MustNewConstMetric(backendPlanBytesDesc, prometheus.GaugeValue, float64(p.CapacityBytes), p.Backend)
		ch <- prometheus.MustNewConstMetric(backendPlanTermEndDesc, prometheus.GaugeValue, float64(p.TermEnd.Unix()), p.Backend)
		pay := 0.0
		if p.PaymentOnFile {
			pay = 1
		}
		ch <- prometheus.MustNewConstMetric(backendPlanPaymentDesc, prometheus.GaugeValue, pay, p.Backend)
		if p.EgressFreeMultiple != nil && snap.hasRunResult {
			var total int64
			if b := snap.stored[p.Backend]; b != nil {
				total = b.Total
			}
			ch <- prometheus.MustNewConstMetric(backendEgressFreeDesc, prometheus.GaugeValue,
				*p.EgressFreeMultiple*float64(total), p.Backend)
		}
	}
}

func (c *capacityCollector) read() capacitySnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || (!c.at.IsZero() && time.Since(c.at) < 15*time.Second) {
		return c.snap
	}
	c.at = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var raw []byte
	err := c.db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, c.jobName).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		c.snap.stored, c.snap.vendor, c.snap.hasRunResult = nil, nil, false
	case err != nil:
		c.logger.Warn("capacity metrics: could not read job_runs — keeping the last values", zap.Error(err))
	default:
		var res BackendCapacityResult
		if len(raw) > 0 && json.Unmarshal(raw, &res) == nil && res.Backends != nil {
			c.snap.stored, c.snap.vendor, c.snap.hasRunResult = res.Backends, res.Vendor, true
		} else {
			c.snap.stored, c.snap.vendor, c.snap.hasRunResult = nil, nil, false
		}
	}
	if plans, err := listBackendPlans(ctx, c.db); err != nil {
		c.logger.Warn("capacity metrics: could not read backend_plans — keeping the last values", zap.Error(err))
	} else {
		c.snap.plans = plans
	}
	return c.snap
}
