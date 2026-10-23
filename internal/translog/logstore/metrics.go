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
	anchorCounts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_translog_anchor_total",
		Help: "Calls of the translog_anchor job to an anchoring service of TRANSLOG_ANCHORS, by kind (ots), service and outcome: ok (a pending proof kept), upgraded (a complete proof kept), unreachable (no answer, a timeout, a 5xx), refused (a 4xx), bad_answer (an answer that is not a proof). Never fails the job.",
	}, []string{"kind", "service", "outcome"})
	anchorsConfigured = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vaultaire_translog_anchors_configured",
		Help: "Anchoring services of TRANSLOG_ANCHORS by kind (0 = none: the translog_anchor job is a no-op).",
	}, []string{"kind"})
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
	for _, k := range AnchorKinds() {
		anchorsConfigured.WithLabelValues(k)
	}
}

// The outcomes of one call to an anchoring service (vaultaire_translog_anchor_total).
const (
	AnchorOK          = "ok"
	AnchorUpgraded    = "upgraded"
	AnchorUnreachable = "unreachable"
	AnchorRefused     = "refused"
	AnchorBadAnswer   = "bad_answer"
)

// AnchorOutcomes lists every outcome.
func AnchorOutcomes() []string {
	return []string{AnchorOK, AnchorUpgraded, AnchorUnreachable, AnchorRefused, AnchorBadAnswer}
}

// AnchorKinds lists the kinds of migration 106.
func AnchorKinds() []string { return []string{AnchorKindOTS, AnchorKindRekor} }

// AnchorService is one configured service.
type AnchorService struct{ Kind, Service string }

// SetAnchors sets vaultaire_translog_anchors_configured and creates every
// (kind, service, outcome) series at 0 — the services are named only by
// TRANSLOG_ANCHORS, read at boot (as SetWitnesses).
func SetAnchors(services []AnchorService) {
	n := map[string]int{}
	for _, a := range services {
		n[a.Kind]++
		for _, o := range AnchorOutcomes() {
			anchorCounts.WithLabelValues(a.Kind, a.Service, o)
		}
	}
	for _, k := range AnchorKinds() {
		anchorsConfigured.WithLabelValues(k).Set(float64(n[k]))
	}
}

// CountAnchor counts one call's outcome.
func CountAnchor(kind, service, outcome string) { AnchorCounter(kind, service, outcome).Inc() }

// AnchorCounter is one series of vaultaire_translog_anchor_total.
func AnchorCounter(kind, service, outcome string) prometheus.Counter {
	return anchorCounts.WithLabelValues(kind, service, outcome)
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
	return []prometheus.Collector{appendFailures, appendRefusals, checkpointRefusals, signingKeyLoaded, witnessCosigns, witnessesConfigured,
		anchorCounts, anchorsConfigured}
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

// AnchorStateCollector reads the anchors' state from translog_anchors on
// scrape (cached 15 s), per kind: the largest checkpoint anchored (any
// state), the newest anchor's time, the pending proofs not completed yet.
// TranslogAnchorStale reads the first two against the newest checkpoint.
type AnchorStateCollector struct {
	db     *sql.DB
	logger *zap.Logger
	ttl    time.Duration

	size, at, pending *prometheus.Desc

	mu   sync.Mutex
	when time.Time
	vals map[string][3]float64
}

// NewAnchorStateCollector builds it.
func NewAnchorStateCollector(db *sql.DB, logger *zap.Logger) *AnchorStateCollector {
	return &AnchorStateCollector{db: db, logger: logger, ttl: stateTTL,
		size: prometheus.NewDesc("vaultaire_translog_anchored_size",
			"Tree size of the largest checkpoint with an anchor of this kind, pending or complete (read from translog_anchors; 0 = none).", []string{"kind"}, nil),
		at: prometheus.NewDesc("vaultaire_translog_anchor_timestamp_seconds",
			"When the newest anchor of this kind was received (0 = never).", []string{"kind"}, nil),
		pending: prometheus.NewDesc("vaultaire_translog_anchor_pending",
			"Anchors of this kind still pending: (checkpoint, service) pairs with a pending proof and no complete one.", []string{"kind"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *AnchorStateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.size
	ch <- c.at
	ch <- c.pending
}

// Collect implements prometheus.Collector.
func (c *AnchorStateCollector) Collect(ch chan<- prometheus.Metric) {
	vals := c.read()
	if vals == nil {
		return // never read: unknown, not zero
	}
	for _, k := range AnchorKinds() {
		v := vals[k]
		ch <- prometheus.MustNewConstMetric(c.size, prometheus.GaugeValue, v[0], k)
		ch <- prometheus.MustNewConstMetric(c.at, prometheus.GaugeValue, v[1], k)
		ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, v[2], k)
	}
}

func (c *AnchorStateCollector) read() map[string][3]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || (c.vals != nil && time.Since(c.when) < c.ttl) {
		return c.vals
	}
	c.when = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := c.db.QueryContext(ctx, `SELECT kind, MAX(checkpoint_size), COALESCE(EXTRACT(EPOCH FROM MAX(received_at)), 0),
	        COUNT(*) FILTER (WHERE state = 'pending' AND NOT EXISTS (SELECT 1 FROM translog_anchors x
	            WHERE x.checkpoint_size = a.checkpoint_size AND x.kind = a.kind AND x.service = a.service AND x.state = 'complete'))
	    FROM translog_anchors a GROUP BY kind`)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("translog metrics: could not read translog_anchors", zap.Error(err))
		}
		return c.vals
	}
	defer func() { _ = rows.Close() }()
	vals := map[string][3]float64{}
	for rows.Next() {
		var k string
		var v [3]float64
		if err := rows.Scan(&k, &v[0], &v[1], &v[2]); err != nil {
			return c.vals
		}
		vals[k] = v
	}
	if rows.Err() != nil {
		return c.vals
	}
	c.vals = vals
	return c.vals
}
