package api

// The pool counter's drift (RP82-3, Stage 1a Prompt 53 Part 0b): the gauge
// vaultaire_pool_used_drift_bytes{root} = tenant_quotas.pool_used_bytes −
// the pool's sum, read from the tables on scrape (a 60 s cache), one series
// per pooled root whose counter differs (none when every counter is true).
// A drift is never repaired by the code: the operator reseeds the root
// (POST /api/v1/admin/quota/reconcile?tenant_id=<root>&pool=reseed,
// audited admin.pool_used_reseeded). Rule PoolUsedDrift
// (deploy/monitoring/vaultaire-quota.yml).

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/usage"
)

const poolDriftMetricsTTL = 60 * time.Second

type poolDriftCollector struct {
	db     *sql.DB
	logger *zap.Logger
	desc   *prometheus.Desc
	mu     sync.Mutex
	at     time.Time
	drift  map[string]int64
}

func newPoolDriftCollector(db *sql.DB, logger *zap.Logger) *poolDriftCollector {
	return &poolDriftCollector{db: db, logger: logger,
		desc: prometheus.NewDesc("vaultaire_pool_used_drift_bytes",
			"A pool root's tenant_quotas.pool_used_bytes minus the sum of its pool's storage_used_bytes (RP82-3), for each pooled root whose counter differs. Read from the tables; never repaired by the code.",
			[]string{"root"}, nil)}
}

func (c *poolDriftCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *poolDriftCollector) Collect(ch chan<- prometheus.Metric) {
	for root, d := range c.read() {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(d), root)
	}
}

func (c *poolDriftCollector) read() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || (c.drift != nil && time.Since(c.at) < poolDriftMetricsTTL) {
		return c.drift
	}
	c.at = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d, err := usage.PoolDrift(ctx, c.db)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("pool drift metric: could not read the pools", zap.Error(err))
		}
		return c.drift
	}
	c.drift = d
	return d
}
