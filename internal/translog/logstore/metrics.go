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
	witnessCosigns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_translog_witness_cosign_total",
		Help: "Checkpoints submitted to a witness of TRANSLOG_WITNESSES (c2sp.org/tlog-witness add-checkpoint), by witness and outcome: ok (a verified cosignature kept), conflict (409 even after the one retry from the size it named), refused (400/403/404/422), bad_answer (a 200 whose lines do not verify under the pinned key), unreachable (no answer, a timeout, a 5xx). Never fails the signing.",
	}, []string{"witness", "outcome"})
	witnessesConfigured = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_translog_witnesses_configured",
		Help: "Witnesses the translog_checkpoint job submits each new checkpoint to (TRANSLOG_WITNESSES; 0 = none, the collect step is off).",
	})
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

// The outcomes of one submission to a witness (vaultaire_translog_witness_cosign_total).
const (
	CosignOK          = "ok"
	CosignConflict    = "conflict"
	CosignRefused     = "refused"
	CosignBadAnswer   = "bad_answer"
	CosignUnreachable = "unreachable"
)

// CosignOutcomes lists every outcome.
func CosignOutcomes() []string {
	return []string{CosignOK, CosignConflict, CosignRefused, CosignBadAnswer, CosignUnreachable}
}

// SetWitnesses sets vaultaire_translog_witnesses_configured and creates
// every (witness, outcome) series at 0 — the witnesses are named only by
// TRANSLOG_WITNESSES, read at boot, so that is where they are pre-created.
func SetWitnesses(names []string) {
	witnessesConfigured.Set(float64(len(names)))
	for _, n := range names {
		for _, o := range CosignOutcomes() {
			witnessCosigns.WithLabelValues(n, o)
		}
	}
}

// CountCosign counts one submission's outcome.
func CountCosign(witness, outcome string) { witnessCosigns.WithLabelValues(witness, outcome).Inc() }

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
	return []prometheus.Collector{appendFailures, appendRefusals, checkpointRefusals, signingKeyLoaded, witnessCosigns, witnessesConfigured}
}

const stateTTL = 15 * time.Second

// StateCollector reads the log's state from the tables on scrape (cached
// 15 s), so a process that has just started reports it.
type StateCollector struct {
	db     *sql.DB
	logger *zap.Logger
	ttl    time.Duration

	size, cpSize, cpTime, cosSize, cosTime *prometheus.Desc

	mu   sync.Mutex
	at   time.Time
	vals *[5]float64
}

// NewStateCollector builds it.
func NewStateCollector(db *sql.DB, logger *zap.Logger) *StateCollector {
	return &StateCollector{db: db, logger: logger, ttl: stateTTL,
		size:   prometheus.NewDesc("vaultaire_translog_size", "Entries in the transparency log (read from translog_entries).", nil, nil),
		cpSize: prometheus.NewDesc("vaultaire_translog_checkpoint_size", "Tree size of the newest signed checkpoint (0 = none).", nil, nil),
		cpTime: prometheus.NewDesc("vaultaire_translog_checkpoint_timestamp_seconds", "When the newest checkpoint was signed (0 = never).", nil, nil),
		cosSize: prometheus.NewDesc("vaultaire_translog_cosigned_size",
			"Tree size of the largest checkpoint any witness cosigned (read from translog_cosignatures; 0 = none).", nil, nil),
		cosTime: prometheus.NewDesc("vaultaire_translog_cosignature_timestamp_seconds",
			"The newest cosignature's own timestamp, any witness (0 = none).", nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *StateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.size
	ch <- c.cpSize
	ch <- c.cpTime
	ch <- c.cosSize
	ch <- c.cosTime
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
	ch <- prometheus.MustNewConstMetric(c.cosSize, prometheus.GaugeValue, v[3])
	ch <- prometheus.MustNewConstMetric(c.cosTime, prometheus.GaugeValue, v[4])
}

func (c *StateCollector) read() *[5]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || (c.vals != nil && time.Since(c.at) < c.ttl) {
		return c.vals
	}
	// Stamped on failure too: a dead database must not cost every scrape a timeout.
	c.at = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var v [5]float64
	err := c.db.QueryRowContext(ctx, `SELECT (SELECT COALESCE(MAX(idx) + 1, 0) FROM translog_entries),
	        (SELECT COALESCE(MAX(size), 0) FROM translog_checkpoints),
	        (SELECT COALESCE(EXTRACT(EPOCH FROM MAX(signed_at)), 0) FROM translog_checkpoints),
	        (SELECT COALESCE(MAX(checkpoint_size), 0) FROM translog_cosignatures),
	        (SELECT COALESCE(EXTRACT(EPOCH FROM MAX(cosigned_at)), 0) FROM translog_cosignatures)`).Scan(&v[0], &v[1], &v[2], &v[3], &v[4])
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
