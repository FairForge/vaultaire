package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// Stale copies of an object (WP-R13-2, Review R13-06 / R13-10).
//
// A key's bytes can be on more than one backend: a Smart-demoted object has a
// hot and a cold copy until the hot one is reclaimed, and any overwrite that
// lands on another backend than the row it replaces (a demoted object
// rewritten hot, a class change, a bucket made public, a failover) leaves the
// old blob where it was. Nothing pointed at those copies, so nothing ever
// deleted them — tape bytes billed to us for every overwritten demoted object.
//
// Two things remove them:
//
//   - dropDisplacedBlob, after every whole-object write: the blob of the row
//     the write replaced, when it is on another backend than the new one;
//   - ledgerSettler.settle, for the copies the demotion ledger knows: called
//     by the daily reclaim (after the grace) and by DeleteObject (at once).
//
// One rule decides in both: a copy is stale when the key does not route to
// its backend — "route" being the head row, or the newest live version
// behind a delete marker (its bytes are what GET ?versionId still serves).
//
// What neither can do is order itself against a writer. A PUT writes its
// bytes to the backend BEFORE its head-row transaction (the in-place
// overwrite, WP-R2-1), so a PUT of the same key that is between those two
// steps is invisible to any lock taken here: a delete of a "stale" copy can
// remove bytes that landed a moment earlier and are about to become the
// object. That cannot be prevented without WP-R2-1; it is detected —
// checkLostWrite re-reads the row after the delete (waiting for a writer
// whose transaction is open) and, when the key now routes to the backend the
// delete ran on and the bytes are not there, logs at Error with the key and
// counts it.

// staleCopyLostWrites counts writes a stale-copy delete destroyed. Every
// source exists at 0 from boot so a rule on increase() has a series to read.
var staleCopyLostWrites = newStaleCopyLostWrites()

const (
	lostWriteReclaim   = "reclaim"   // the daily job (hot-copy reclaim, stale promotion)
	lostWriteDelete    = "delete"    // DeleteObject / DeleteObjects settling a demoted key
	lostWriteOverwrite = "overwrite" // PutObject / CopyObject / CompleteMultipartUpload dropping a displaced blob
)

func newStaleCopyLostWrites() *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_stale_copy_lost_writes_total",
		Help: "Objects whose bytes were removed by the delete of a stale copy while a write of the same key was in flight (source: reclaim, delete, overwrite). Not preventable before WP-R2-1; every increment has an Error log line with the key.",
	}, []string{"source"})
	for _, s := range []string{lostWriteReclaim, lostWriteDelete, lostWriteOverwrite} {
		c.WithLabelValues(s)
	}
	return c
}

// staleCopyTimeout bounds one cleanup on a request path. It runs detached
// from the request: the write or delete it follows has already committed.
const staleCopyTimeout = 30 * time.Second

// rowQuerier is the part of *sql.DB / *sql.Tx the route read needs.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// keyRoute is where the whole-object bytes of a key live now.
type keyRoute struct {
	live    bool   // something still describes this key: a head row, or a live version behind a delete marker
	headRow bool   // that something is the head row
	backend string // the backend its whole blob is on; "" for a chunked row (no whole blob anywhere)
	etag    string
	unknown bool // live, whole, and no backend on record: no copy may be deleted
}

// routeOf reads the key's route. With lock, the head row is taken FOR UPDATE
// (q must be a transaction): every writer and deleter of the key then waits
// for the caller's commit before it can change the row.
func routeOf(ctx context.Context, q rowQuerier, tenantID, bucket, key string, lock bool) (keyRoute, error) {
	query := `SELECT COALESCE(backend_name,''), COALESCE(etag,''), is_chunked FROM object_head_cache
		WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`
	if lock {
		query += ` FOR UPDATE`
	}
	var rt keyRoute
	var chunked bool
	err := q.QueryRowContext(ctx, query, tenantID, bucket, key).Scan(&rt.backend, &rt.etag, &chunked)
	switch {
	case err == nil:
		rt.live, rt.headRow = true, true
		if chunked {
			rt.backend = "" // the object is its chunks: no whole blob at this key is live
		} else if rt.backend == "" {
			rt.unknown = true
		}
		return rt, nil
	case !errors.Is(err, sql.ErrNoRows):
		return keyRoute{}, fmt.Errorf("read head row: %w", err)
	}

	// No head row. On a versioning-enabled bucket that is also the state of a
	// key whose current version is a delete marker — and then the newest live
	// version's bytes are still at the key (versionBytesIntact).
	var latestIsMarker bool
	err = q.QueryRowContext(ctx, `SELECT is_delete_marker FROM object_versions
		WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3 AND is_latest = TRUE LIMIT 1`,
		tenantID, bucket, key).Scan(&latestIsMarker)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !latestIsMarker) {
		return keyRoute{}, nil
	}
	if err != nil {
		return keyRoute{}, fmt.Errorf("read latest version: %w", err)
	}
	err = q.QueryRowContext(ctx, `SELECT COALESCE(backend_name,''), COALESCE(etag,'') FROM object_versions
		WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3 AND is_delete_marker = FALSE
		ORDER BY created_at DESC LIMIT 1`, tenantID, bucket, key).Scan(&rt.backend, &rt.etag)
	if errors.Is(err, sql.ErrNoRows) {
		return keyRoute{}, nil
	}
	if err != nil {
		return keyRoute{}, fmt.Errorf("read newest live version: %w", err)
	}
	rt.live = true
	rt.unknown = rt.backend == ""
	return rt, nil
}

// deleteCopyOn removes the key's blob on ONE named backend, through its
// driver. Never through engine.Delete: that falls back to the primary when
// the named backend misses, and the primary is where the live bytes of an
// overwritten object are. Two registered driver names never share a store
// (cmd/vaultaire/main.go registers each backend once; the default iDrive
// region is the primary and gets no `idrive-<region>` twin), so a delete on
// a backend the key does not route to cannot reach the bytes it does route to.
func deleteCopyOn(ctx context.Context, eng *engine.CoreEngine, backend, tenantID, container, key string) error {
	drv, ok := eng.GetDriver(backend)
	if !ok {
		return fmt.Errorf("backend %q not registered", backend)
	}
	if err := drv.Delete(common.WithTenantID(ctx, tenantID), container, key); err != nil && !isObjectMissingErr(err) {
		return err
	}
	return nil
}

// checkLostWrite is the detection half of R13-06: called after a stale-copy
// delete on deletedFrom. It takes the advisory lock every head-row writer
// holds for its transaction (atomicHeadUpsertReleasing) — so it waits for a
// writer that was already committing — re-reads the row, and when the key
// now routes to a backend the delete ran on, asks that backend whether the
// bytes are there. Missing = the delete removed a write that was in flight.
//
// What it cannot see: a writer whose bytes landed before the delete and
// whose transaction had not begun when this ran (the gap between a PUT's
// backend write returning and its BeginTx — the measured-size and digest
// checks, no I/O). That write is lost undetected.
func checkLostWrite(ctx context.Context, db *sql.DB, eng *engine.CoreEngine, logger *zap.Logger,
	source, tenantID, bucket, key string, deletedFrom []string) {
	if len(deletedFrom) == 0 {
		return
	}
	// Bounded: the wait is for a writer's head-row transaction, a few
	// statements long. A check that cannot run is a Warn, not a stalled job.
	ctx, cancel := context.WithTimeout(ctx, staleCopyTimeout)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		logger.Warn("stale copy: lost-write check could not run", zap.Error(err),
			zap.String("tenant", tenantID), zap.String("bucket", bucket), zap.String("key", key))
		return
	}
	var rt keyRoute
	_, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1 || '/' || $2 || '/' || $3)::bigint)`, tenantID, bucket, key)
	if err == nil {
		rt, err = routeOf(ctx, tx, tenantID, bucket, key, false)
	}
	_ = tx.Rollback()
	if err != nil {
		logger.Warn("stale copy: lost-write check could not run", zap.Error(err),
			zap.String("tenant", tenantID), zap.String("bucket", bucket), zap.String("key", key))
		return
	}
	if !rt.headRow || rt.backend == "" {
		return
	}
	for _, b := range deletedFrom {
		if b != rt.backend {
			continue
		}
		drv, ok := eng.GetDriver(b)
		if !ok {
			return
		}
		exists, exErr := drv.Exists(common.WithTenantID(ctx, tenantID), tenantID+"_"+bucket, key)
		if exErr != nil {
			logger.Warn("stale copy: a row appeared on the backend a stale copy was deleted from and its bytes could not be checked",
				zap.String("source", source), zap.String("tenant", tenantID), zap.String("bucket", bucket),
				zap.String("key", key), zap.String("backend", b), zap.Error(exErr))
			return
		}
		if !exists {
			staleCopyLostWrites.WithLabelValues(source).Inc()
			logger.Error("stale copy: the delete of a stale copy removed a write of the same key that was in flight — the object has a head row and no bytes (R13-06; the customer must upload it again)",
				zap.String("source", source), zap.String("tenant", tenantID), zap.String("bucket", bucket),
				zap.String("key", key), zap.String("backend", b), zap.String("etag", rt.etag))
		}
		return
	}
}

// dropDisplacedBlob removes the whole blob of the head row a write replaced,
// when that row routed to ANOTHER backend than the one just written (same
// backend = the write overwrote it in place). Best-effort and detached from
// the request: the write is committed; a failed delete costs an orphan.
//
// The row is re-read first: when the key routes to the displaced backend
// again (another writer put it back there in the meantime) the blob is that
// writer's. A writer that has written and not yet committed is the window
// described at the top of this file; checkLostWrite follows the delete.
func dropDisplacedBlob(ctx context.Context, db *sql.DB, eng engine.Engine, logger *zap.Logger, source string,
	tenantID, bucket, container, key string, displaced displacedRow, written string) {
	if db == nil || displaced.Chunked || displaced.Backend == "" || written == "" || displaced.Backend == written {
		return
	}
	ce, ok := eng.(*engine.CoreEngine)
	if !ok {
		return
	}
	if _, registered := ce.GetDriver(displaced.Backend); !registered {
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), staleCopyTimeout)
	defer cancel()

	rt, err := routeOf(dctx, db, tenantID, bucket, key, false)
	if err != nil {
		logger.Warn("displaced blob left in place: the key's route could not be read", zap.Error(err),
			zap.String("tenant", tenantID), zap.String("bucket", bucket), zap.String("key", key),
			zap.String("backend", displaced.Backend))
		return
	}
	if rt.live && (rt.unknown || rt.backend == displaced.Backend) {
		return
	}
	if err := deleteCopyOn(dctx, ce, displaced.Backend, tenantID, container, key); err != nil {
		logger.Warn("displaced blob not removed — orphan on the backend the object left",
			zap.Error(err), zap.String("tenant", tenantID), zap.String("bucket", bucket), zap.String("key", key),
			zap.String("backend", displaced.Backend))
		return
	}
	logger.Info("displaced blob removed",
		zap.String("tenant", tenantID), zap.String("bucket", bucket), zap.String("key", key),
		zap.String("from", displaced.Backend), zap.String("object_now_on", written))
	checkLostWrite(dctx, db, ce, logger, source, tenantID, bucket, key, []string{displaced.Backend})
}

// Ledger outcomes (smart_demotions.hot_outcome).
const (
	ledgerDeleted      = "deleted"       // demoted, hot copy reclaimed: the live state of a demoted object
	ledgerKeptChanged  = "kept_changed"  // the object was replaced or moved since: the row is history
	ledgerObjectGone   = "object_gone"   // the object was deleted since: the row is history
	ledgerDeleteFailed = "delete_failed" // a stale copy could not be removed: still open, retried
)

// ledgerSettler brings one demotion ledger row in line with what its key
// routes to now.
type ledgerSettler struct {
	db     *sql.DB
	eng    *engine.CoreEngine
	logger *zap.Logger
	now    func() time.Time

	beforeDelete func(bucket, key string) // test hook: inside the tx, after the locks, before the first delete
	afterDelete  func(bucket, key string) // test hook: after the commit, before the lost-write check
}

// settle is the one rule for the copies a ledger row knows about (the hot
// copy until it is reclaimed, and the cold copy):
//
//   - the key still routes to the cold backend with the demoted etag — the
//     object is as the demotion left it. Inside the grace nothing happens
//     (a read flips it back hot for free); with graceElapsed the hot copy is
//     reclaimed (`deleted`). A row already reclaimed is left alone.
//   - anything else — the object was overwritten, moved, chunked or deleted —
//     every copy on a backend the key does NOT route to is ours and stale:
//     removed, and the row closed as `kept_changed` / `object_gone`. A copy
//     on the backend the key DOES route to is the customer's current object
//     (a PUT with an archive class puts their bytes on the cold backend at
//     this very key) and is never touched.
//
// Lock order is head row, then ledger row — the order of the demotion, the
// flip-back and the copy-back — and both are held across the deletes, so a
// writer's or a flip-back's transaction waits for the verdict to be on
// record. A delete that fails leaves an open row open (`delete_failed`): the
// next reclaim tries again.
//
// Returns the outcome written, or "" when there was nothing to settle.
func (l *ledgerSettler) settle(ctx context.Context, source, tenantID, bucket, key string, graceElapsed bool) (string, error) {
	container := tenantID + "_" + bucket

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rt, err := routeOf(ctx, tx, tenantID, bucket, key, true)
	if err != nil {
		return "", err
	}
	var etag, hotBackend, coldBackend, prevOutcome string
	var hotDeletedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT etag, hot_backend, cold_backend, hot_deleted_at, hot_outcome FROM smart_demotions
		WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3 FOR UPDATE`,
		tenantID, bucket, key).Scan(&etag, &hotBackend, &coldBackend, &hotDeletedAt, &prevOutcome)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lock ledger row: %w", err)
	}
	hotOnRecord := !hotDeletedAt.Valid
	if !hotOnRecord && (prevOutcome == ledgerKeptChanged || prevOutcome == ledgerObjectGone) {
		return "", nil // settled before
	}

	var outcome string
	var stale []string
	switch {
	case rt.headRow && !rt.unknown && rt.backend == coldBackend && rt.etag == etag:
		if !hotOnRecord || !graceElapsed {
			return "", nil
		}
		outcome, stale = ledgerDeleted, []string{hotBackend}
	case rt.unknown:
		// A live whole object with no backend on record: we cannot tell which
		// copy it reads. Close the row, delete nothing.
		outcome = ledgerKeptChanged
	default:
		outcome = ledgerObjectGone
		if rt.live {
			outcome = ledgerKeptChanged
		}
		if hotOnRecord && hotBackend != rt.backend {
			stale = append(stale, hotBackend)
		}
		if coldBackend != rt.backend {
			stale = append(stale, coldBackend)
		}
	}

	if len(stale) > 0 && l.beforeDelete != nil {
		l.beforeDelete(bucket, key)
	}
	for _, b := range stale {
		if delErr := deleteCopyOn(ctx, l.eng, b, tenantID, container, key); delErr != nil {
			// An orphan costs money, never data. An open row stays open and
			// the next reclaim retries; a row whose hot copy was reclaimed
			// long ago is retried by whatever touches the key next.
			if hotOnRecord {
				_, _ = tx.ExecContext(ctx, `UPDATE smart_demotions SET hot_outcome=$4 WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
					tenantID, bucket, key, ledgerDeleteFailed)
				_ = tx.Commit()
			}
			return "", fmt.Errorf("delete stale copy on %s: %w", b, delErr)
		}
	}
	// A settled row asks for nothing any more: a pending copy-back or
	// restore of an object that has since changed must not run.
	clearRequests := outcome != ledgerDeleted
	if _, err := tx.ExecContext(ctx, `UPDATE smart_demotions
		SET hot_deleted_at = COALESCE(hot_deleted_at, $4), hot_outcome = $5,
		    promote_requested_at = CASE WHEN $6 THEN NULL ELSE promote_requested_at END,
		    restore_requested_at = CASE WHEN $6 THEN NULL ELSE restore_requested_at END
		WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
		tenantID, bucket, key, l.now(), outcome, clearRequests); err != nil {
		return "", fmt.Errorf("close ledger: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	if len(stale) > 0 {
		if l.afterDelete != nil {
			l.afterDelete(bucket, key)
		}
		checkLostWrite(ctx, l.db, l.eng, l.logger, source, tenantID, bucket, key, stale)
	}
	return outcome, nil
}

// OnDelete settles the demotion ledger row of a key that DeleteObject (or
// DeleteObjects) has just removed. Without it the hot copy of an object
// deleted inside the grace stayed until a later reclaim came for it — paid
// storage for a deleted object, and the delete that R13-06 is about: by then
// the customer may be uploading the key again. Doing it here, in the request
// that deleted the object, leaves the reclaim only the deletes that failed.
// No ledger row (every key that was never demoted) costs one index lookup.
func (p *SmartPromoter) OnDelete(ctx context.Context, tenantID, bucket, key string) {
	if p == nil {
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), staleCopyTimeout)
	defer cancel()
	var one int
	err := p.db.QueryRowContext(dctx, `SELECT 1 FROM smart_demotions WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
		tenantID, bucket, key).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err == nil {
		_, err = p.settler().settle(dctx, lostWriteDelete, tenantID, bucket, key, false)
	}
	if err != nil {
		p.logger.Warn("smart demotion: ledger not settled after a delete (the reclaim retries)",
			zap.String("tenant", tenantID), zap.String("bucket", bucket), zap.String("key", key), zap.Error(err))
	}
}

func (p *SmartPromoter) settler() *ledgerSettler {
	return &ledgerSettler{db: p.db, eng: p.eng, logger: p.logger, now: p.now}
}
