package logstore

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/translog"
)

var (
	appendFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_translog_append_failures_total",
		Help: "Transparency-log appends that failed (refused entry, database error), by entry kind.",
	}, []string{"kind"})
	checkpointRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_translog_checkpoint_refusals_total",
		Help: "Checkpoint passes that refused to sign (rewritten: the log does not extend a signed tree; behind_highwater: the database was restored behind the newest tree this box signed; highwater_unreadable: the high-water file cannot be read).",
	}, []string{"reason"})
	appendRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_translog_append_refusals_total",
		Help: "Transparency-log appends refused because the log is frozen (behind_highwater: the database was restored behind the newest tree this box signed; highwater_unreadable: the high-water file cannot be read). Nothing was written; translog-restore is the way out.",
	}, []string{"reason"})
	signingKeyLoaded = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_translog_signing_key_loaded",
		Help: "1 when this process has the checkpoint signing key (credential translog_signing_key, TRANSLOG_SIGNING_KEY_FILE or TRANSLOG_SIGNING_KEY); 0 = entries append, nothing is signed.",
	})
)

func init() {
	for _, k := range translog.Kinds() {
		appendFailures.WithLabelValues(k)
	}
	for _, r := range RefusalReasons() {
		checkpointRefusals.WithLabelValues(r)
	}
	for _, r := range AppendRefusalReasons() {
		appendRefusals.WithLabelValues(r)
	}
}

// SetSigningKeyLoaded sets vaultaire_translog_signing_key_loaded.
func SetSigningKeyLoaded(loaded bool) {
	if loaded {
		signingKeyLoaded.Set(1)
		return
	}
	signingKeyLoaded.Set(0)
}

// Collectors are the process-state series.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{appendFailures, appendRefusals, checkpointRefusals, signingKeyLoaded}
}

const stateTTL = 15 * time.Second

// StateCollector reads the log's state from the tables on scrape (cached
// 15 s), so a process that has just started reports it.
type StateCollector struct {
	db     *sql.DB
	logger *zap.Logger
	ttl    time.Duration

	size, cpSize, cpTime *prometheus.Desc

	mu   sync.Mutex
	at   time.Time
	vals *[3]float64
}

// NewStateCollector builds it.
func NewStateCollector(db *sql.DB, logger *zap.Logger) *StateCollector {
	return &StateCollector{db: db, logger: logger, ttl: stateTTL,
		size:   prometheus.NewDesc("vaultaire_translog_size", "Entries in the transparency log (read from translog_entries).", nil, nil),
		cpSize: prometheus.NewDesc("vaultaire_translog_checkpoint_size", "Tree size of the newest signed checkpoint (0 = none).", nil, nil),
		cpTime: prometheus.NewDesc("vaultaire_translog_checkpoint_timestamp_seconds", "When the newest checkpoint was signed (0 = never).", nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *StateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.size
	ch <- c.cpSize
	ch <- c.cpTime
}

// Collect implements prometheus.Collector.
func (c *StateCollector) Collect(ch chan<- prometheus.Metric) {
	v := c.read()
	if v == nil {
		return // never read: unknown, not zero
	}
	ch <- prometheus.MustNewConstMetric(c.size, prometheus.GaugeValue, v[0])
	ch <- prometheus.MustNewConstMetric(c.cpSize, prometheus.GaugeValue, v[1])
	ch <- prometheus.MustNewConstMetric(c.cpTime, prometheus.GaugeValue, v[2])
}

func (c *StateCollector) read() *[3]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || (c.vals != nil && time.Since(c.at) < c.ttl) {
		return c.vals
	}
	// Stamped on failure too: a dead database must not cost every scrape a timeout.
	c.at = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var v [3]float64
	err := c.db.QueryRowContext(ctx, `SELECT (SELECT COALESCE(MAX(idx) + 1, 0) FROM translog_entries),
	        (SELECT COALESCE(MAX(size), 0) FROM translog_checkpoints),
	        (SELECT COALESCE(EXTRACT(EPOCH FROM MAX(signed_at)), 0) FROM translog_checkpoints)`).Scan(&v[0], &v[1], &v[2])
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("translog metrics: could not read the log tables", zap.Error(err))
		}
		return c.vals
	}
	c.vals = &v
	return c.vals
}

// CountAppendFailure counts a failure that happened before Append could be
// called (a producer that could not build its entry).
func CountAppendFailure(kind string) { appendFailures.WithLabelValues(kind).Inc() }

// AppendRefusalReasons lists why an append can be refused (the log frozen).
func AppendRefusalReasons() []string {
	return []string{RefusedBehindHighwater, RefusedHighwaterUnreadable}
}
