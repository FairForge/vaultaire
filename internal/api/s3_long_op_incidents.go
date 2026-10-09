package api

import (
	"context"
	"database/sql"
	"strconv"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// What a stopping slot could not finish, persisted (082,
// s3_long_op_incidents): Prometheus scrapes only the ACTIVE slot, so the
// in-process counters of the slot a deploy stops — the operations it cut at
// their bound, the <Error> documents it sent inside a committed 200 while
// draining — were never seen, and LongOpsAbandonedAtShutdown could not fire
// (post-merge review of Prompt 2a). The stopping slot writes one row per
// such operation while its database is still open; whichever slot is active
// exports the counts from the table, like the job metrics from job_runs.

// Incident outcomes (s3_long_op_incidents.outcome).
const (
	longOpAbandoned = "abandoned"
	// longOpErrorAfterCommit (s3_long_op.go) is the other one.
)

// longOpIncident is one row to write.
type longOpIncident struct {
	e       *longOpEntry
	outcome string
	age     time.Duration
}

// recordLongOpIncident writes e's row.
func (s *Server) recordLongOpIncident(e *longOpEntry, outcome string, age time.Duration) {
	s.recordLongOpIncidents([]longOpIncident{{e: e, outcome: outcome, age: age}})
}

// recordLongOpIncidents writes the rows in one statement. Best effort with
// one short context: the stop sequence must not hang on it, and each Warn
// line is already written.
func (s *Server) recordLongOpIncidents(rows []longOpIncident) {
	if s.db == nil || len(rows) == 0 {
		return
	}
	slot := ""
	if s.config != nil && s.config.Server.Port != 0 {
		slot = strconv.Itoa(s.config.Server.Port)
	}
	outcomes, ops, tenants, buckets, keys := make([]string, len(rows)), make([]string, len(rows)), make([]string, len(rows)), make([]string, len(rows)), make([]string, len(rows))
	ages := make([]float64, len(rows))
	for i, r := range rows {
		outcomes[i], ops[i], tenants[i], buckets[i], keys[i], ages[i] = r.outcome, r.e.Op, r.e.tenant, r.e.Bucket, r.e.Key, r.age.Seconds()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO s3_long_op_incidents (outcome, op, tenant_id, bucket, object_key, age_seconds, slot, version)
		SELECT u.outcome, u.op, u.tenant, u.bucket, u.key, u.age, $7, $8
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::float8[]) AS u(outcome, op, tenant, bucket, key, age)`,
		pq.Array(outcomes), pq.Array(ops), pq.Array(tenants), pq.Array(buckets), pq.Array(keys), pq.Array(ages), slot, BuildSHA); err != nil {
		for _, r := range rows {
			s.log().Error("long S3 operation incident not recorded", zap.Error(err),
				zap.String("outcome", r.outcome), zap.String("op", r.e.Op), zap.String("tenant", r.e.tenant),
				zap.String("bucket", r.e.Bucket), zap.String("key", r.e.Key))
		}
	}
}

// longOpIncidentsCollector exports the table's counts per op:
// vaultaire_s3_long_ops_abandoned_total and
// vaultaire_s3_long_ops_drain_errors_total. A counter must never fall —
// Prometheus reads a lower value as a reset and increase() counts the whole
// new value as an increase (a false alert; Prompt 2a.3 H3): an account
// erasure blanks its rows instead of deleting them (account.Deleted), the
// series are not exported at all until the table has been read once (a
// failed first read after a restart exported 0, and the next good read
// looked like N new incidents), and a read never lowers what was exported.
type longOpIncidentsCollector struct {
	db        *sql.DB
	logger    *zap.Logger
	abandoned *prometheus.Desc
	drainErrs *prometheus.Desc

	mu     sync.Mutex
	at     time.Time
	counts map[[2]string]float64 // {outcome, op}
}

// longOpIncidentsCacheTTL: one query per scrape interval at most.
const longOpIncidentsCacheTTL = 15 * time.Second

func newLongOpIncidentsCollector(db *sql.DB, logger *zap.Logger) *longOpIncidentsCollector {
	return &longOpIncidentsCollector{
		db:     db,
		logger: logger,
		abandoned: prometheus.NewDesc("vaultaire_s3_long_ops_abandoned_total",
			"Long S3 operations a stopping slot cut because they were still running 15 min after their start, by operation — read from s3_long_op_incidents (the stopping slot is no longer scraped). Each one also has a Warn line with tenant, bucket, key and age.",
			[]string{"op"}, nil),
		drainErrs: prometheus.NewDesc("vaultaire_s3_long_ops_drain_errors_total",
			"Long S3 operations that failed inside their committed 200 while their slot was stopping (error_after_commit during a drain), by operation — read from s3_long_op_incidents.",
			[]string{"op"}, nil),
	}
}

func (c *longOpIncidentsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.abandoned
	ch <- c.drainErrs
}

func (c *longOpIncidentsCollector) Collect(ch chan<- prometheus.Metric) {
	counts := c.read()
	if counts == nil {
		return // never read: unknown, not zero
	}
	for _, op := range []string{longOpComplete, longOpCopy, longOpBatch} {
		ch <- prometheus.MustNewConstMetric(c.abandoned, prometheus.CounterValue, counts[[2]string{longOpAbandoned, op}], op)
		ch <- prometheus.MustNewConstMetric(c.drainErrs, prometheus.CounterValue, counts[[2]string{longOpErrorAfterCommit, op}], op)
	}
}

func (c *longOpIncidentsCollector) read() map[[2]string]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || (c.counts != nil && time.Since(c.at) < longOpIncidentsCacheTTL) {
		return c.counts
	}
	// Stamped on failure too: a dead database must not cost every scrape a
	// query timeout.
	c.at = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := c.db.QueryContext(ctx, `SELECT outcome, op, COUNT(*) FROM s3_long_op_incidents GROUP BY outcome, op`)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("long-op metrics: could not read s3_long_op_incidents", zap.Error(err))
		}
		return c.counts
	}
	defer func() { _ = rows.Close() }()
	counts := map[[2]string]float64{}
	for rows.Next() {
		var outcome, op string
		var n int64
		if err := rows.Scan(&outcome, &op, &n); err != nil {
			return c.counts
		}
		counts[[2]string{outcome, op}] = float64(n)
	}
	if rows.Err() != nil {
		return c.counts
	}
	for k, v := range c.counts {
		counts[k] = max(counts[k], v)
	}
	c.counts = counts
	return counts
}
