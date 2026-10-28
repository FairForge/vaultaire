package api

// The data-plane hosts' health (WP-EDGE-3 step 13): the gauge
// vaultaire_data_plane_host_up{host,kind} read from data_plane_hosts on
// scrape (a 15 s cache) — 1 when a host's health is up and, for a node,
// its last heartbeat is within dataPlaneHostStaleAfter (the Worker and the
// origin send none: their row's health is the truth) — and the sweep the
// edge_admit_reaper runs: a node up but unseen that long is marked down
// (a draining one is the operator's and stays). Rule DataPlaneHostDown
// (deploy/monitoring/vaultaire-dataplane.yml).

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/dataplane"
)

// dataPlaneHostStaleAfter: a node heartbeats every 30 s; three missed is down.
const dataPlaneHostStaleAfter = 90 * time.Second

const dataPlaneHostsMetricsTTL = 15 * time.Second

type dataPlaneHostUp struct {
	host, kind string
	up         bool
}

type dataPlaneHostsCollector struct {
	db     *sql.DB
	logger *zap.Logger
	desc   *prometheus.Desc
	mu     sync.Mutex
	at     time.Time
	rows   []dataPlaneHostUp
}

func newDataPlaneHostsCollector(db *sql.DB, logger *zap.Logger) *dataPlaneHostsCollector {
	return &dataPlaneHostsCollector{db: db, logger: logger,
		desc: prometheus.NewDesc("vaultaire_data_plane_host_up",
			"1 when a data-plane host is up: its registry health is up and, for a node, its last heartbeat is within 90 s (data_plane_hosts, WP-EDGE-3). Read from the table.",
			[]string{"host", "kind"}, nil)}
}

func (c *dataPlaneHostsCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *dataPlaneHostsCollector) Collect(ch chan<- prometheus.Metric) {
	for _, r := range c.read() {
		v := 0.0
		if r.up {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, v, r.host, r.kind)
	}
}

func (c *dataPlaneHostsCollector) read() []dataPlaneHostUp {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || (c.rows != nil && time.Since(c.at) < dataPlaneHostsMetricsTTL) {
		return c.rows
	}
	c.at = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := c.db.QueryContext(ctx, `
		SELECT host, kind, health = 'up' AND (kind <> $1 OR (last_seen IS NOT NULL AND last_seen > NOW() - $2 * interval '1 second'))
		FROM data_plane_hosts ORDER BY host`, dataplane.KindNode, dataPlaneHostStaleAfter.Seconds())
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("data-plane hosts metrics: could not read the registry", zap.Error(err))
		}
		return c.rows
	}
	defer func() { _ = rows.Close() }()
	out := []dataPlaneHostUp{}
	for rows.Next() {
		var r dataPlaneHostUp
		if err := rows.Scan(&r.host, &r.kind, &r.up); err != nil {
			return c.rows
		}
		out = append(out, r)
	}
	if rows.Err() != nil {
		return c.rows
	}
	c.rows = out
	return c.rows
}

// sweepUnseenDataPlaneHosts marks nodes up but unseen for
// dataPlaneHostStaleAfter down (never a draining one, never the Worker or
// the origin); the count marked.
func sweepUnseenDataPlaneHosts(ctx context.Context, db *sql.DB) (int, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE data_plane_hosts SET health = 'down'
		WHERE kind = $1 AND health = 'up' AND (last_seen IS NULL OR last_seen < NOW() - $2 * interval '1 second')`,
		dataplane.KindNode, dataPlaneHostStaleAfter.Seconds())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
