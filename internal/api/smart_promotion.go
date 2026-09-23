package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// Smart-tier read-time promotion (Phase 5.15.8, PR B).
//
// The demotion job moves idle bytes hot→cold and leaves a ledger row. When a
// demoted object is READ, this brings it back hot:
//
//   - hot copy still present (inside the reclaim grace window): promotion is
//     a routing flip back to the hot backend — no data movement — and the
//     cold copy is dropped.
//   - hot copy already reclaimed: the read is served from the cold backend
//     now (Geyser keeps fresh writes on staging disk ~13 days) and a copy
//     back to hot runs asynchronously; the daily job retries leftovers.
//   - cold backend has evicted it to tape (ErrArchived): a RestoreObject is
//     submitted on the reader's behalf and the GET answers 503 + Retry-After
//     instead of Glacier's 403 (design decision Q2 in SMART_TIER_DESIGN.md —
//     Smart customers never have to know what a restore is). Once the
//     restore lands, the daily job copies it back hot.
//
// Only objects with a smart_demotions row are touched: Vault/archive-class
// objects keep their honest Glacier semantics.

// SmartPromoter promotes Smart-demoted objects back to the hot backend on read.
type SmartPromoter struct {
	db     *sql.DB
	eng    *engine.CoreEngine
	logger *zap.Logger

	HotBackend  string
	ColdBackend string
	// RestoreDays is the recall window requested from the cold backend.
	RestoreDays int32
	// Concurrency bounds simultaneous async copy-backs.
	Concurrency int

	sem      chan struct{}
	inflight sync.Map // "tenant/bucket/key" → struct{}
	sync     bool     // tests: run copy-backs inline
	now      func() time.Time

	beforeFlip func(bucket, key string) // test hook: after the hot copy, before the routing flip
}

// NewSmartPromoter builds the promoter; nil when there is no DB or engine.
func NewSmartPromoter(db *sql.DB, eng *engine.CoreEngine, logger *zap.Logger) *SmartPromoter {
	if db == nil || eng == nil {
		return nil
	}
	return &SmartPromoter{
		db: db, eng: eng, logger: logger,
		HotBackend:  "idrive",
		ColdBackend: "geyser",
		RestoreDays: 7,
		Concurrency: 4,
		sem:         make(chan struct{}, 4),
		now:         time.Now,
	}
}

type ledgerRow struct {
	etag             string
	size             int64
	hotDeleted       bool
	promoteRequested bool
	restoreRequested bool
}

func (p *SmartPromoter) lookup(ctx context.Context, tenantID, bucket, key string) (ledgerRow, bool, error) {
	var row ledgerRow
	var hotDeleted, promoteReq, restoreReq sql.NullTime
	err := p.db.QueryRowContext(ctx,
		`SELECT etag, size_bytes, hot_deleted_at, promote_requested_at, restore_requested_at
		 FROM smart_demotions WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
		tenantID, bucket, key).Scan(&row.etag, &row.size, &hotDeleted, &promoteReq, &restoreReq)
	if errors.Is(err, sql.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	row.hotDeleted, row.promoteRequested, row.restoreRequested = hotDeleted.Valid, promoteReq.Valid, restoreReq.Valid
	return row, true, nil
}

// OnRead is called by the GET path when the head cache routes an object to
// the cold backend. It returns the backend the request should read from.
func (p *SmartPromoter) OnRead(ctx context.Context, tenantID, bucket, key, etag string) string {
	if p == nil {
		return ""
	}
	row, ok, err := p.lookup(ctx, tenantID, bucket, key)
	if err != nil {
		p.logger.Warn("smart promotion: ledger lookup failed", zap.Error(err))
		return p.ColdBackend
	}
	if !ok || row.etag != etag {
		return p.ColdBackend // not ours, or the ledger is stale for this version
	}

	if !row.hotDeleted {
		if p.flipBack(ctx, tenantID, bucket, key, etag) {
			return p.HotBackend
		}
		return p.ColdBackend
	}

	// Hot copy is gone: serve cold now, copy back in the background.
	if !row.promoteRequested {
		_, _ = p.db.ExecContext(ctx,
			`UPDATE smart_demotions SET promote_requested_at = $4
			 WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3 AND promote_requested_at IS NULL`,
			tenantID, bucket, key, p.now())
	}
	p.schedule(tenantID, bucket, key, etag, row.size)
	return p.ColdBackend
}

// OnArchived is called when a GET on a cold-routed object failed with
// ErrArchived. For a Smart-demoted object it submits the restore on the
// reader's behalf and returns true (the caller answers 503 + Retry-After).
func (p *SmartPromoter) OnArchived(ctx context.Context, tenantID, bucket, key string) bool {
	if p == nil {
		return false
	}
	row, ok, err := p.lookup(ctx, tenantID, bucket, key)
	if err != nil || !ok {
		return false
	}
	p.requestRestore(ctx, tenantID, bucket, key, row)
	return true
}

func (p *SmartPromoter) requestRestore(ctx context.Context, tenantID, bucket, key string, row ledgerRow) {
	cold, ok := p.eng.GetDriver(p.ColdBackend)
	if !ok {
		return
	}
	restorer, ok := cold.(engine.Restorer)
	if !ok {
		p.logger.Warn("smart promotion: cold backend cannot restore", zap.String("backend", p.ColdBackend))
		return
	}
	tctx := common.WithTenantID(ctx, tenantID)
	container := tenantID + "_" + bucket
	if !row.restoreRequested {
		err := restorer.RestoreObject(tctx, container, key, p.RestoreDays)
		if err != nil && !errors.Is(err, engine.ErrRestoreAlreadyInProgress) {
			p.logger.Warn("smart promotion: auto-restore request failed",
				zap.String("tenant", tenantID), zap.String("bucket", bucket), zap.String("key", key), zap.Error(err))
			return
		}
		p.logger.Info("smart promotion: auto-restore requested",
			zap.String("tenant", tenantID), zap.String("bucket", bucket), zap.String("key", key))
	}
	_, _ = p.db.ExecContext(ctx,
		`UPDATE smart_demotions
		 SET restore_requested_at = COALESCE(restore_requested_at, $4),
		     promote_requested_at = COALESCE(promote_requested_at, $4)
		 WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
		tenantID, bucket, key, p.now())
}

// flipBack re-routes a demoted object to the hot backend while its hot copy
// still exists. Conditional on backend+etag; the cold copy is dropped
// best-effort afterwards (tape bytes cost money).
func (p *SmartPromoter) flipBack(ctx context.Context, tenantID, bucket, key, etag string) bool {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer func() { _ = tx.Rollback() }()
	// A promotion IS a read: refresh last_accessed so the daily job does not
	// demote the object straight back on the idle rule.
	res, err := tx.ExecContext(ctx,
		`UPDATE object_head_cache SET backend_name=$1, last_accessed=NOW()
		 WHERE tenant_id=$2 AND bucket=$3 AND object_key=$4 AND backend_name=$5 AND etag=$6`,
		p.HotBackend, tenantID, bucket, key, p.ColdBackend, etag)
	if err != nil {
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM smart_demotions WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, tenantID, bucket, key); err != nil {
		return false
	}
	if err := tx.Commit(); err != nil {
		return false
	}
	container := tenantID + "_" + bucket
	p.eng.HintBackend(container, key, p.HotBackend)
	if cold, ok := p.eng.GetDriver(p.ColdBackend); ok {
		if err := cold.Delete(common.WithTenantID(ctx, tenantID), container, key); err != nil {
			p.logger.Warn("smart promotion: cold copy not removed after flip-back", zap.Error(err))
		}
	}
	p.logger.Info("smart promotion: flipped back within grace",
		zap.String("tenant", tenantID), zap.String("bucket", bucket), zap.String("key", key))
	return true
}

func (p *SmartPromoter) schedule(tenantID, bucket, key, etag string, size int64) {
	id := tenantID + "/" + bucket + "/" + key
	if _, loaded := p.inflight.LoadOrStore(id, struct{}{}); loaded {
		return
	}
	run := func() {
		defer p.inflight.Delete(id)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		if _, err := p.promote(ctx, tenantID, bucket, key, etag, size); err != nil {
			p.logger.Warn("smart promotion: copy-back failed (daily job retries)",
				zap.String("tenant", tenantID), zap.String("bucket", bucket), zap.String("key", key), zap.Error(err))
		}
	}
	if p.sync {
		run()
		return
	}
	go func() {
		p.sem <- struct{}{}
		defer func() { <-p.sem }()
		run()
	}()
}

// promote copies one object cold→hot and flips routing under an etag guard.
// Outcomes: "promoted", "restore_requested" (evicted, recall submitted),
// "changed" (object rewritten meanwhile; ledger dropped).
func (p *SmartPromoter) promote(ctx context.Context, tenantID, bucket, key, etag string, size int64) (string, error) {
	hot, ok := p.eng.GetDriver(p.HotBackend)
	if !ok {
		return "", fmt.Errorf("hot backend %q not registered", p.HotBackend)
	}
	cold, ok := p.eng.GetDriver(p.ColdBackend)
	if !ok {
		return "", fmt.Errorf("cold backend %q not registered", p.ColdBackend)
	}
	tctx := common.WithTenantID(ctx, tenantID)
	container := tenantID + "_" + bucket

	rc, err := cold.Get(tctx, container, key)
	if err != nil {
		if errors.Is(err, engine.ErrArchived) {
			row, found, lerr := p.lookup(ctx, tenantID, bucket, key)
			if lerr == nil && found {
				p.requestRestore(ctx, tenantID, bucket, key, row)
			}
			return "restore_requested", nil
		}
		return "", fmt.Errorf("read cold: %w", err)
	}
	putErr := hot.Put(tctx, container, key, rc, engine.WithContentLength(size))
	_ = rc.Close()
	if putErr != nil {
		return "", fmt.Errorf("write hot: %w", putErr)
	}

	if p.beforeFlip != nil {
		p.beforeFlip(bucket, key)
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// A promotion IS a read: refresh last_accessed so the daily job does not
	// demote the object straight back on the idle rule.
	res, err := tx.ExecContext(ctx,
		`UPDATE object_head_cache SET backend_name=$1, last_accessed=NOW()
		 WHERE tenant_id=$2 AND bucket=$3 AND object_key=$4 AND backend_name=$5 AND etag=$6`,
		p.HotBackend, tenantID, bucket, key, p.ColdBackend, etag)
	if err != nil {
		return "", fmt.Errorf("flip routing: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM smart_demotions WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, tenantID, bucket, key); err != nil {
		return "", fmt.Errorf("drop ledger: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	if n == 0 {
		// The object changed while we copied. Only drop our hot blob when
		// nothing live routes to the hot backend at this key: a PUT that
		// landed hot overwrote it with the NEW bytes, which are not ours to
		// delete.
		var cur string
		qerr := p.db.QueryRowContext(ctx, `SELECT COALESCE(backend_name,'') FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, tenantID, bucket, key).Scan(&cur)
		if errors.Is(qerr, sql.ErrNoRows) || (qerr == nil && cur != p.HotBackend) {
			_ = hot.Delete(tctx, container, key)
		}
		return "changed", nil
	}
	p.eng.HintBackend(container, key, p.HotBackend)
	if err := cold.Delete(tctx, container, key); err != nil {
		p.logger.Warn("smart promotion: cold copy not removed after copy-back", zap.Error(err))
	}
	p.logger.Info("smart promotion: copied back to hot",
		zap.String("tenant", tenantID), zap.String("bucket", bucket), zap.String("key", key), zap.Int64("bytes", size))
	return "promoted", nil
}

// PromotePending runs the copy-back for every ledger row that asked for a
// promotion (async worker died, or a restore that has since landed). Called
// by the daily demotion job before it demotes anything.
func (p *SmartPromoter) PromotePending(ctx context.Context) (int, []string) {
	if p == nil {
		return 0, nil
	}
	rows, err := p.db.QueryContext(ctx,
		`SELECT tenant_id, bucket, object_key, etag, size_bytes FROM smart_demotions
		 WHERE promote_requested_at IS NOT NULL ORDER BY promote_requested_at ASC LIMIT 1000`)
	if err != nil {
		return 0, []string{fmt.Sprintf("promote pending: list: %v", err)}
	}
	type item struct {
		tenant, bucket, key, etag string
		size                      int64
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.tenant, &it.bucket, &it.key, &it.etag, &it.size); err != nil {
			_ = rows.Close()
			return 0, []string{fmt.Sprintf("promote pending: scan: %v", err)}
		}
		items = append(items, it)
	}
	_ = rows.Close()

	promoted := 0
	var errs []string
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		outcome, err := p.promote(ctx, it.tenant, it.bucket, it.key, it.etag, it.size)
		if err != nil {
			errs = append(errs, fmt.Sprintf("promote %s %s/%s: %v", it.tenant, it.bucket, it.key, err))
			continue
		}
		if outcome == "promoted" {
			promoted++
		}
	}
	return promoted, errs
}
