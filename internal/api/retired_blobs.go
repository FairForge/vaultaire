package api

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// The retired_blobs collector (WP-R2-1 Part C, the `retired_blobs` job, every
// 10 min): whole-object blobs that a write displaced, a version delete
// released, or a write that failed after its bytes landed left behind
// (retired_blobs, 086) are deleted once their grace has passed — modelled on
// key_reencrypt's retired_chunk_blobs pass.
//
// It is the ONLY path that deletes a displaced whole-object blob, and it
// never trusts the row that queued it: under the key's head-row advisory
// lock (every writer's flip holds it) it asks again whether anything names
// the address (blobNamedBy: the head row, a version that keeps bytes, an
// open demotion ledger row, the newest version behind a delete marker for a
// blob at <key>, an active multipart upload). A named blob is kept and its
// row dropped — a later write that displaces it queues it again; a blob only
// an active upload names waits another hour. The delete goes to the backend
// the row RECORDS, never the engine's fan-out: an unregistered backend or an
// open breaker is skipped (a note; the row stays), never a failed run.

const (
	retiredBlobsJobName = "retired_blobs"
	retiredBlobsBatch   = 1000
	// retiredBlobUploadRecheck is how long a blob only an active upload names
	// waits before it is asked about again.
	retiredBlobUploadRecheck = time.Hour
	// retiredBlobDeleteTimeout bounds one backend delete.
	retiredBlobDeleteTimeout = 30 * time.Second
)

// Collector outcomes (vaultaire_retired_blobs_total{outcome}).
const (
	retiredOutcomeDeleted = "deleted" // no row named it: deleted on its backend, row removed
	retiredOutcomeKept    = "kept"    // a row names it again (or an active upload does): not deleted
	retiredOutcomeSkipped = "skipped" // its backend is unregistered or its breaker open: the row waits
	retiredOutcomeFailed  = "failed"  // the delete failed: the row waits for the next run
)

var retiredBlobsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_retired_blobs_total",
	Help: "Retired whole-object blobs the retired_blobs job handled, by outcome: deleted (no row named it; deleted on its recorded backend) | kept (a row, or an active multipart upload, names it again) | skipped (its backend is not registered or its breaker is open; the row waits) | failed (the backend delete failed; retried next run).",
}, []string{"outcome"})

func init() {
	for _, o := range []string{retiredOutcomeDeleted, retiredOutcomeKept, retiredOutcomeSkipped, retiredOutcomeFailed} {
		retiredBlobsTotal.WithLabelValues(o)
	}
}

// RetiredBlobCollector deletes retired whole-object blobs after their grace.
type RetiredBlobCollector struct {
	db     *sql.DB
	eng    engine.Engine
	logger *zap.Logger
	// JobName is the job_runs name (tests use their own: the table is shared).
	JobName string
	now     func() time.Time
	// onlyTenant limits a run to one tenant's rows (tests: packages share
	// the database).
	onlyTenant string
	// beforeDelete runs under the key's lock, after the re-check, before the
	// backend delete (tests).
	beforeDelete func(storageKey string)
}

// RetiredBlobsResult is one run's outcome, recorded as job_runs.result.
type RetiredBlobsResult struct {
	Due     int `json:"due"`
	Deleted int `json:"deleted"`
	Kept    int `json:"kept"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
	// SkippedBackends names the backends whose rows waited (unregistered or
	// breaker open).
	SkippedBackends []string `json:"skipped_backends,omitempty"`
	Errors          []string `json:"errors,omitempty"`
}

// NewRetiredBlobCollector builds the collector; nil without a database or
// an engine.
func NewRetiredBlobCollector(db *sql.DB, eng engine.Engine, logger *zap.Logger) *RetiredBlobCollector {
	if db == nil || eng == nil {
		return nil
	}
	return &RetiredBlobCollector{db: db, eng: eng, logger: logger, JobName: retiredBlobsJobName, now: time.Now}
}

func (c *RetiredBlobCollector) spec() jobSpec {
	return jobSpec{Name: c.JobName, Every: 10 * time.Minute, BootDelay: 5 * time.Minute, MaxRunTime: 9 * time.Minute,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := c.RunOnce(ctx)
			c.logger.Info("retired blobs run", zap.Int("due", res.Due), zap.Int("deleted", res.Deleted),
				zap.Int("kept", res.Kept), zap.Int("skipped", res.Skipped), zap.Int("failed", res.Failed),
				zap.Strings("skipped_backends", res.SkippedBackends), zap.Error(err))
			rep := jobReport{Rows: int64(res.Deleted), Result: res}
			if res.Skipped > 0 || res.Failed > 0 {
				rep.Note = fmt.Sprintf("%d retired blob(s) skipped (backend unregistered or breaker open: %s), %d failed; they stay for the next run",
					res.Skipped, strings.Join(res.SkippedBackends, ","), res.Failed)
			}
			return rep, err
		}}
}

// retiredRow is one due row.
type retiredRow struct {
	backend, container, storageKey, tenantID string
}

// RunOnce handles the rows due now, oldest first, up to one batch.
func (c *RetiredBlobCollector) RunOnce(ctx context.Context) (RetiredBlobsResult, error) {
	var res RetiredBlobsResult
	rows, err := c.db.QueryContext(ctx, `
		SELECT backend_id, container, storage_key, tenant_id FROM retired_blobs
		WHERE retire_after <= $1 AND ($2 = '' OR tenant_id = $2)
		ORDER BY retire_after LIMIT $3`, c.now(), c.onlyTenant, retiredBlobsBatch)
	if err != nil {
		return res, fmt.Errorf("read retired blobs: %w", err)
	}
	var due []retiredRow
	for rows.Next() {
		var r retiredRow
		if err := rows.Scan(&r.backend, &r.container, &r.storageKey, &r.tenantID); err != nil {
			_ = rows.Close()
			return res, fmt.Errorf("scan retired blob: %w", err)
		}
		due = append(due, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return res, fmt.Errorf("read retired blobs: %w", err)
	}
	_ = rows.Close()
	res.Due = len(due)

	ce, _ := c.eng.(*engine.CoreEngine)
	skipped := map[string]bool{}
	for _, r := range due {
		if ctx.Err() != nil {
			return res, nil // the rest waits for the next run
		}
		var drv engine.Driver
		var registered bool
		if ce != nil {
			drv, registered = ce.GetDriver(r.backend)
		}
		open := ce != nil && ce.GetFailoverStatus()[r.backend] == engine.StateOpen.String()
		if !registered || open {
			res.Skipped++
			retiredBlobsTotal.WithLabelValues(retiredOutcomeSkipped).Inc()
			if !skipped[r.backend] {
				skipped[r.backend] = true
				res.SkippedBackends = append(res.SkippedBackends, r.backend)
			}
			continue
		}
		outcome, err := c.collect(ctx, drv, r)
		retiredBlobsTotal.WithLabelValues(outcome).Inc()
		switch outcome {
		case retiredOutcomeDeleted:
			res.Deleted++
		case retiredOutcomeKept:
			res.Kept++
		default:
			res.Failed++
			if len(res.Errors) < 10 {
				res.Errors = append(res.Errors, fmt.Sprintf("%s on %s: %v", r.storageKey, r.backend, err))
			}
			c.logger.Warn("retired blob not deleted (kept for the next run)", zap.String("backend", r.backend),
				zap.String("container", r.container), zap.String("storage_key", r.storageKey), zap.Error(err))
		}
	}
	return res, nil
}

// retiredBlobKey is the object key a retired address belongs to: the part
// before "@<id>" for an address newBlobAddress made, else the address itself
// (a blob at <key>).
func retiredBlobKey(storageKey string) string {
	if i := strings.LastIndexByte(storageKey, '@'); i >= 0 && isBlobAddressOf(storageKey[:i], storageKey) {
		return storageKey[:i]
	}
	return storageKey
}

// collect handles one row under the key's head-row lock.
func (c *RetiredBlobCollector) collect(ctx context.Context, drv engine.Driver, r retiredRow) (string, error) {
	bucket, ok := strings.CutPrefix(r.container, r.tenantID+"_")
	if !ok {
		return retiredOutcomeFailed, fmt.Errorf("container %q is not the tenant's", r.container)
	}
	key := retiredBlobKey(r.storageKey)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return retiredOutcomeFailed, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockHeadKey(ctx, tx, r.tenantID, bucket, key); err != nil {
		return retiredOutcomeFailed, err
	}
	naming, err := blobNamedBy(ctx, tx, r.tenantID, bucket, key, r.storageKey)
	if err != nil {
		return retiredOutcomeFailed, err
	}
	switch naming {
	case blobNamed:
		if _, err := tx.ExecContext(ctx, `DELETE FROM retired_blobs WHERE backend_id = $1 AND container = $2 AND storage_key = $3`,
			r.backend, r.container, r.storageKey); err != nil {
			return retiredOutcomeFailed, err
		}
		return retiredOutcomeKept, tx.Commit()
	case blobUploadOwned:
		if _, err := tx.ExecContext(ctx, `UPDATE retired_blobs SET retire_after = $4
			WHERE backend_id = $1 AND container = $2 AND storage_key = $3`,
			r.backend, r.container, r.storageKey, c.now().Add(retiredBlobUploadRecheck)); err != nil {
			return retiredOutcomeFailed, err
		}
		return retiredOutcomeKept, tx.Commit()
	}
	if c.beforeDelete != nil {
		c.beforeDelete(r.storageKey)
	}
	dctx, cancel := context.WithTimeout(common.WithTenantID(ctx, r.tenantID), retiredBlobDeleteTimeout)
	defer cancel()
	if err := drv.Delete(dctx, r.container, r.storageKey); err != nil && !isObjectMissingErr(err) {
		return retiredOutcomeFailed, fmt.Errorf("delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM retired_blobs WHERE backend_id = $1 AND container = $2 AND storage_key = $3`,
		r.backend, r.container, r.storageKey); err != nil {
		return retiredOutcomeFailed, err
	}
	return retiredOutcomeDeleted, tx.Commit()
}

// retiredBlobsBacklogCollector exports vaultaire_retired_blobs_backlog: the
// rows already due (past retire_after) — read from the table, so a fresh
// process reports the truth; a backlog that never drains means the
// collector is not running or cannot delete.
type retiredBlobsBacklogCollector struct {
	db     *sql.DB
	logger *zap.Logger
	desc   *prometheus.Desc

	mu  sync.Mutex
	at  time.Time
	due *float64
}

const retiredBlobsBacklogTTL = 15 * time.Second

func newRetiredBlobsBacklogCollector(db *sql.DB, logger *zap.Logger) *retiredBlobsBacklogCollector {
	return &retiredBlobsBacklogCollector{db: db, logger: logger,
		desc: prometheus.NewDesc("vaultaire_retired_blobs_backlog",
			"Retired whole-object blobs past their grace and not yet deleted (retired_blobs rows with retire_after <= now). Read from the table.",
			nil, nil)}
}

func (c *retiredBlobsBacklogCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *retiredBlobsBacklogCollector) Collect(ch chan<- prometheus.Metric) {
	if v := c.read(); v != nil {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, *v)
	}
}

func (c *retiredBlobsBacklogCollector) read() *float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || (c.due != nil && time.Since(c.at) < retiredBlobsBacklogTTL) {
		return c.due
	}
	c.at = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var n int64
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM retired_blobs WHERE retire_after <= NOW()`).Scan(&n); err != nil {
		if c.logger != nil {
			c.logger.Warn("retired blobs metrics: could not read the backlog", zap.Error(err))
		}
		return c.due
	}
	v := float64(n)
	c.due = &v
	return c.due
}
