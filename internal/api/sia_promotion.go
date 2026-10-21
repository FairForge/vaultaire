package api

import (
	"context"
	"crypto/md5" // #nosec G501 -- an integrity digest of a copy, compared with the object's own MD5 (its ETag), not a security primitive
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// Hydrate-before-serve for large Sia objects (Phase 6.5; migration 101,
// bench-results/SIA-S3D-2026-10.md SIA-BENCH-2..6).
//
// The `sia` backend has a read budget: hosts refuse reads once the
// account's per-host balances are spent (refilled every 5 min), and the
// patched gateway waits for the refill instead of failing — so a large read
// can sit idle for minutes and a client with a 60 s read timeout gives up.
// The benches' answer is the Vault one: hydrate once, serve from a fast copy.
//
// A GET of an object recorded on `sia` whose size is at least
// SIA_PROMOTE_MIN_BYTES (a Range GET: whose range is) is
//
//   - served from its promoted copy on the primary when one is ready for
//     the object as it is now (same address on sia, same ETag) — every
//     integrity check of a whole read applies (the copy holds the object's
//     stored bytes: a sealed object opens as from sia, an unsealed one is
//     MD5-checked against its ETag) — and the copy's last access is
//     refreshed;
//   - otherwise answered 503 SlowDown + Retry-After while ONE background
//     promotion (per object, process-wide at most SIA_PROMOTE_CONCURRENCY)
//     streams the stored bytes from sia into the primary at a new blob
//     address, checks the length (and the MD5 of an unsealed object), and
//     marks the row ready under the key's lock — only while the head row is
//     still the object it copied. The 503 is the state of one object, not a
//     server failure: kept out of vaultaire_errors_total (markClientRefusal),
//     as the Smart tier's restore wait is.
//
// Smaller objects and ranges stream straight from sia.
//
// The head row is never re-pointed: the object stays on sia (its route,
// its bill); the copy is a cache, never billed. A write gives the object a
// new address, so a row of an older one is never served; it is retired —
// the row deleted, the copy queued in retired_blobs (24 h grace, deleted by
// the retired_blobs collector on the primary) — by the write's flip
// transaction (commitBlobWrite), by a DELETE's aftermath, and by the
// `sia_promotion` job, which also reclaims copies idle past SIA_PROMOTE_TTL
// and promotions abandoned by a process that stopped.
//
// Why not the smart_demotions ledger: a demotion row describes a key whose
// head row routes to the cold copy and is flipped back hot by a promotion;
// here the head row stays on sia for ever and the copy is a cache with its
// own address and lifetime, possibly several per key while a write and a
// promotion overlap. Bending the demotion ledger's flip-back, reclaim and
// settle rules around that would put both tiers at risk.

// siaSourceBackend is the backend whose large objects are promoted.
const siaSourceBackend = "sia"

// retiredSiaPromotion is the retired_blobs reason of a promoted copy.
const retiredSiaPromotion = "sia_promotion"

// siaPromotionJobName is the job's job_runs name.
const siaPromotionJobName = "sia_promotion"

// Defaults and bounds of the SIA_PROMOTE_* knobs.
const (
	siaPromoteMinBytesDefault    = 64 << 20
	siaPromoteMinBytesFloor      = 1 << 20
	siaPromoteMinBytesCeiling    = 1 << 40
	siaPromoteConcurrencyDefault = 2
	siaPromoteConcurrencyMax     = 64
	siaPromoteTTLDefault         = 7 * 24 * time.Hour
	siaPromoteTTLMin             = time.Hour
	siaPromoteTTLMax             = 90 * 24 * time.Hour
	siaReadBudgetMax             = 10 << 30
)

// The 503's Retry-After: the estimate of one promotion (size / the read
// budget when SIA_READ_BUDGET is set, else 60 s), never under 15 s, never
// over an hour.
const (
	siaRetryAfterDefault = 60
	siaRetryAfterMin     = 15
	siaRetryAfterMax     = 3600
)

const siaHydrateMessage = "This object is being brought to fast storage before it is served. No action is needed: retry the request after the Retry-After interval."

// siaPromoteConfig is the knobs, read once at boot.
type siaPromoteConfig struct {
	// MinBytes: objects (ranges) at least this large are promoted; 0 = off.
	MinBytes    int64
	Concurrency int
	TTL         time.Duration
	// ReadBudget is SIA_READ_BUDGET in bytes/s (0 = not set): the
	// Retry-After estimate only.
	ReadBudget int64
}

// siaPromoteConfigFromEnv reads SIA_PROMOTE_MIN_BYTES (a size such as
// 64MiB, 1 MiB–1 TiB; `off`/`0` disables promotion), SIA_PROMOTE_CONCURRENCY
// (1–64), SIA_PROMOTE_TTL (a Go duration, 1h–2160h) and SIA_READ_BUDGET
// (bytes per second such as 25MiB; `off`/`0` = not set). A rejected value is
// logged at Warn and the default kept.
func siaPromoteConfigFromEnv(getenv func(string) string, logger *zap.Logger) siaPromoteConfig {
	c := siaPromoteConfig{MinBytes: siaPromoteMinBytesDefault, Concurrency: siaPromoteConcurrencyDefault, TTL: siaPromoteTTLDefault}
	if v := strings.TrimSpace(getenv("SIA_PROMOTE_MIN_BYTES")); v != "" {
		switch n, err := parseByteSize(v); {
		case strings.EqualFold(v, "off") || v == "0":
			c.MinBytes = 0
		case err == nil && n >= siaPromoteMinBytesFloor && n <= siaPromoteMinBytesCeiling:
			c.MinBytes = n
		default:
			logger.Warn("invalid SIA_PROMOTE_MIN_BYTES (need a size such as 64MiB, 1MiB..1TiB, or off), keeping default",
				zap.String("value", v), zap.Int64("default", c.MinBytes))
		}
	}
	if v := strings.TrimSpace(getenv("SIA_PROMOTE_CONCURRENCY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= siaPromoteConcurrencyMax {
			c.Concurrency = n
		} else {
			logger.Warn("invalid SIA_PROMOTE_CONCURRENCY (need 1..64), keeping default",
				zap.String("value", v), zap.Int("default", c.Concurrency))
		}
	}
	if v := strings.TrimSpace(getenv("SIA_PROMOTE_TTL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= siaPromoteTTLMin && d <= siaPromoteTTLMax {
			c.TTL = d
		} else {
			logger.Warn("invalid SIA_PROMOTE_TTL (need a duration 1h..2160h), keeping default",
				zap.String("value", v), zap.Duration("default", c.TTL))
		}
	}
	if v := strings.TrimSpace(getenv("SIA_READ_BUDGET")); v != "" && v != "0" && !strings.EqualFold(v, "off") {
		if n, err := parseByteSize(v); err == nil && n > 0 && n <= siaReadBudgetMax {
			c.ReadBudget = n
		} else {
			logger.Warn("invalid SIA_READ_BUDGET for the promotion's Retry-After (need bytes per second such as 25MiB, or off), keeping default (not set)",
				zap.String("value", v))
		}
	}
	return c
}

// Metrics.
var (
	siaPromotions = newSiaPromotionsCounter()

	siaPromotionSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "vaultaire_sia_promotion_seconds",
		Help:    "Duration of one Sia promotion (the copy from sia into the primary, checked and committed), whatever its outcome.",
		Buckets: []float64{5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400},
	})
)

// Outcomes of vaultaire_sia_promotions_total.
const (
	siaOutcomeStarted   = "started"   // a read claimed the object and started its promotion
	siaOutcomePromoted  = "promoted"  // the copy landed, checked, and is served from now on
	siaOutcomeFailed    = "failed"    // the copy failed (read, write, length, MD5); the row and the partial copy retired
	siaOutcomeChanged   = "changed"   // the object was overwritten or deleted while it was copied: the copy retired
	siaOutcomeBusy      = "busy"      // no promotion slot free (SIA_PROMOTE_CONCURRENCY): the read answered 503, nothing started
	siaOutcomeServed    = "served"    // a read served from a promoted copy
	siaOutcomeRetired   = "retired"   // a copy of an object since overwritten or deleted, or an abandoned promotion, retired
	siaOutcomeReclaimed = "reclaimed" // a copy idle past SIA_PROMOTE_TTL retired
)

func newSiaPromotionsCounter() *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_sia_promotions_total",
		Help: "Hydrate-before-serve of large Sia objects, by outcome: started | promoted | failed | changed (overwritten/deleted during the copy) | busy (no promotion slot; 503, nothing started) | served (a read from a promoted copy) | retired (stale or abandoned copy) | reclaimed (idle past SIA_PROMOTE_TTL).",
	}, []string{"outcome"})
	for _, o := range []string{siaOutcomeStarted, siaOutcomePromoted, siaOutcomeFailed, siaOutcomeChanged, siaOutcomeBusy,
		siaOutcomeServed, siaOutcomeRetired, siaOutcomeReclaimed} {
		c.WithLabelValues(o)
	}
	return c
}

// SiaPromoter promotes large Sia objects into the primary on read.
type SiaPromoter struct {
	db     *sql.DB
	eng    *engine.CoreEngine
	logger *zap.Logger
	cfg    siaPromoteConfig

	// SourceBackend is the backend whose objects are promoted ("sia").
	SourceBackend string
	// JobName is the job_runs name (tests use their own: the table is shared).
	JobName string

	sem chan struct{}
	wg  sync.WaitGroup
	now func() time.Time
	// sync runs a promotion inline (tests).
	sync bool
	// onlyTenant confines the job to one tenant (tests: the table is shared).
	onlyTenant string
	// beforeCommit runs after the copy, before its commit (tests).
	beforeCommit func(bucket, key string)
}

// NewSiaPromoter builds the promoter; nil without a database, an engine, or
// a `sia` driver.
func NewSiaPromoter(db *sql.DB, eng *engine.CoreEngine, cfg siaPromoteConfig, logger *zap.Logger) *SiaPromoter {
	if db == nil || eng == nil {
		return nil
	}
	if _, ok := eng.GetDriver(siaSourceBackend); !ok {
		return nil
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = siaPromoteConcurrencyDefault
	}
	if cfg.TTL <= 0 {
		cfg.TTL = siaPromoteTTLDefault
	}
	return &SiaPromoter{db: db, eng: eng, logger: logger, cfg: cfg, SourceBackend: siaSourceBackend,
		JobName: siaPromotionJobName, sem: make(chan struct{}, cfg.Concurrency), now: time.Now}
}

// siaObject is what a read knows of the object from its head row.
type siaObject struct {
	tenantID, bucket, key string
	etag, storageKey      string
	encAlgo, bodyMD5      string
	size                  int64
}

func (o siaObject) sourceKey() string { return blobArtifact(o.key, o.storageKey) }
func (o siaObject) container() string { return o.tenantID + "_" + o.bucket }

// siaReadDecision is what a read does.
type siaReadDecision int

const (
	siaReadDirect   siaReadDecision = iota // stream from sia, as before
	siaReadPromoted                        // read the promoted copy (backend, blob)
	siaReadWait                            // 503 SlowDown + Retry-After: a promotion is running
)

type siaReadPlan struct {
	decision      siaReadDecision
	backend, blob string
	retryAfter    int
}

// primary is the backend promoted copies go to now.
func (p *SiaPromoter) primary() string { return p.eng.GetPrimary() }

// enabled reports whether promotion is on and has a primary to copy into.
func (p *SiaPromoter) enabled() bool {
	if p == nil || p.cfg.MinBytes <= 0 {
		return false
	}
	pr := p.primary()
	return pr != "" && pr != p.SourceBackend
}

// OnRead decides how a GET of o reads wantBytes (the whole object, or the
// range asked). Called only for a head-row read (never ?versionId) of a
// whole object recorded on SourceBackend.
func (p *SiaPromoter) OnRead(ctx context.Context, o siaObject, wantBytes int64) siaReadPlan {
	if !p.enabled() || o.size < p.cfg.MinBytes || !storedBlobKnown(o.encAlgo) {
		return siaReadPlan{decision: siaReadDirect}
	}
	// A ready copy of the object as it is now: served, its access refreshed.
	var backend, blob string
	err := p.db.QueryRowContext(ctx, `
		UPDATE sia_promotions SET last_accessed = NOW()
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND source_key = $4
		  AND etag = $5 AND state = 'ready'
		RETURNING promoted_backend, promoted_key`,
		o.tenantID, o.bucket, o.key, o.sourceKey(), o.etag).Scan(&backend, &blob)
	switch {
	case err == nil:
		if p.eng.CanAsk(backend) == nil {
			siaPromotions.WithLabelValues(siaOutcomeServed).Inc()
			return siaReadPlan{decision: siaReadPromoted, backend: backend, blob: blob}
		}
		// The copy's backend cannot be asked now (breaker open, no longer
		// registered): the object is still on sia.
		return siaReadPlan{decision: siaReadDirect}
	case !errors.Is(err, sql.ErrNoRows):
		p.logger.Warn("sia promotion: lookup failed; reading from sia", zap.Error(err),
			zap.String("tenant_id", o.tenantID), zap.String("bucket", o.bucket), zap.String("key", o.key))
		return siaReadPlan{decision: siaReadDirect}
	}
	if wantBytes < p.cfg.MinBytes {
		return siaReadPlan{decision: siaReadDirect}
	}
	// The primary must be able to take the copy; when it cannot, waiting
	// would not help and the object is still readable from sia.
	if p.eng.CanAsk(p.primary()) != nil {
		return siaReadPlan{decision: siaReadDirect}
	}
	p.start(ctx, o)
	return siaReadPlan{decision: siaReadWait, retryAfter: p.retryAfter(o)}
}

// retryAfter is the 503's Retry-After in seconds.
func (p *SiaPromoter) retryAfter(o siaObject) int {
	s := siaRetryAfterDefault
	if p.cfg.ReadBudget > 0 {
		s = int(storedBlobLength(o.encAlgo, o.size, nil)/p.cfg.ReadBudget) + 1
	}
	return min(max(s, siaRetryAfterMin), siaRetryAfterMax)
}

// promoteTimeout bounds one promotion of a blob of stored bytes: 30 min plus
// the time at 4 MiB/s (well under the slowest bench). A pending row older
// than this (plus a margin) was abandoned by a process that stopped.
func promoteTimeout(stored int64) time.Duration {
	return 30*time.Minute + time.Duration(stored/(4<<20))*time.Second
}

const siaAbandonMargin = 10 * time.Minute

// start claims o and runs its promotion, unless one is running (here or in
// the other slot) or every slot is busy.
func (p *SiaPromoter) start(ctx context.Context, o siaObject) {
	select {
	case p.sem <- struct{}{}:
	default:
		siaPromotions.WithLabelValues(siaOutcomeBusy).Inc()
		return
	}
	primary := p.primary()
	addr, claimed, err := p.claim(ctx, o, primary)
	if err != nil || !claimed {
		<-p.sem
		if err != nil {
			p.logger.Warn("sia promotion: claim failed", zap.Error(err),
				zap.String("tenant_id", o.tenantID), zap.String("bucket", o.bucket), zap.String("key", o.key))
		}
		return
	}
	siaPromotions.WithLabelValues(siaOutcomeStarted).Inc()
	run := func() {
		defer func() { <-p.sem }()
		stored := storedBlobLength(o.encAlgo, o.size, nil)
		pctx, cancel := context.WithTimeout(context.Background(), promoteTimeout(stored))
		defer cancel()
		began := p.now()
		outcome, perr := p.promote(pctx, o, primary, addr)
		siaPromotionSeconds.Observe(p.now().Sub(began).Seconds())
		siaPromotions.WithLabelValues(outcome).Inc()
		if perr != nil {
			p.logger.Warn("sia promotion failed (the next read starts another)", zap.Error(perr),
				zap.String("tenant_id", o.tenantID), zap.String("bucket", o.bucket), zap.String("key", o.key))
			return
		}
		p.logger.Info("sia promotion", zap.String("outcome", outcome), zap.Int64("bytes", stored),
			zap.String("tenant_id", o.tenantID), zap.String("bucket", o.bucket), zap.String("key", o.key))
	}
	if p.sync {
		run()
		return
	}
	p.wg.Add(1)
	go func() { // #nosec G118 -- a promotion outlives the read that started it by design
		defer p.wg.Done()
		run()
	}()
}

// claim records the promotion of o before any byte is written: a new row
// (state pending) naming a fresh address on primary. Not claimed when a row
// of this object exists (a promotion running here or in the other slot, or
// a copy ready since the lookup), unless that row is a pending promotion
// abandoned past its timeout — then its address is retired and the row is
// taken over.
func (p *SiaPromoter) claim(ctx context.Context, o siaObject, primary string) (string, bool, error) {
	addr, err := newBlobAddress(o.key)
	if err != nil {
		return "", false, err
	}
	stored := storedBlobLength(o.encAlgo, o.size, nil)
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockHeadKey(ctx, tx, o.tenantID, o.bucket, o.key); err != nil {
		return "", false, err
	}
	var state, oldBackend, oldKey string
	var requested time.Time
	var oldStored int64
	err = tx.QueryRowContext(ctx, `
		SELECT state, promoted_backend, promoted_key, requested_at, stored_bytes FROM sia_promotions
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND source_key = $4 FOR UPDATE`,
		o.tenantID, o.bucket, o.key, o.sourceKey()).Scan(&state, &oldBackend, &oldKey, &requested, &oldStored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sia_promotions (tenant_id, bucket, object_key, source_backend, source_key, etag, size_bytes, stored_bytes,
			                            promoted_backend, promoted_key, state, requested_at, last_accessed)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'pending', $11, $11)`,
			o.tenantID, o.bucket, o.key, p.SourceBackend, o.sourceKey(), o.etag, o.size, stored, primary, addr, p.now()); err != nil {
			return "", false, fmt.Errorf("record promotion: %w", err)
		}
	case err != nil:
		return "", false, fmt.Errorf("read promotion: %w", err)
	case state == "pending" && p.now().Sub(requested) > promoteTimeout(oldStored)+siaAbandonMargin:
		if _, err := tx.ExecContext(ctx, `
			UPDATE sia_promotions SET etag = $5, size_bytes = $6, stored_bytes = $7, promoted_backend = $8, promoted_key = $9,
			       requested_at = $10, last_accessed = $10, state = 'pending', ready_at = NULL, promoted_md5 = ''
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND source_key = $4`,
			o.tenantID, o.bucket, o.key, o.sourceKey(), o.etag, o.size, stored, primary, addr, p.now()); err != nil {
			return "", false, fmt.Errorf("take over an abandoned promotion: %w", err)
		}
		if err := queueRetired(ctx, tx, o.tenantID, o.container(), oldBackend, oldKey); err != nil {
			return "", false, err
		}
	default:
		return "", false, nil
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("commit claim: %w", err)
	}
	return addr, true, nil
}

// queueRetired queues one promoted copy for the retired_blobs collector
// (retiredBlobGrace) inside the caller's transaction. The caller has
// deleted (or re-pointed) the row that named it.
func queueRetired(ctx context.Context, tx *sql.Tx, tenantID, container, backend, addr string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO retired_blobs (backend_id, container, storage_key, tenant_id, reason, retire_after)
		VALUES ($1, $2, $3, $4, $5, NOW() + make_interval(secs => $6))
		ON CONFLICT (backend_id, container, storage_key) DO UPDATE SET reason = EXCLUDED.reason, retire_after = EXCLUDED.retire_after`,
		backend, container, addr, tenantID, retiredSiaPromotion, retiredBlobGrace.Seconds()); err != nil {
		return fmt.Errorf("retire promoted copy %s: %w", addr, err)
	}
	return nil
}

// hashingCounter counts and hashes what it passes on.
type hashingCounter struct {
	r io.Reader
	h hash.Hash
	n int64
}

func (c *hashingCounter) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if n > 0 {
		c.h.Write(b[:n])
		c.n += int64(n)
	}
	return n, err
}

// promote copies o's stored bytes from sia to (primary, addr) and commits
// the row ready while the head row is still o. Returns the outcome
// (promoted, changed, failed); on anything but promoted the row is gone and
// the copy retired.
func (p *SiaPromoter) promote(ctx context.Context, o siaObject, primary, addr string) (string, error) {
	md5hex, err := p.copy(ctx, o, primary, addr)
	if err != nil {
		p.abandon(ctx, o, primary, addr)
		return siaOutcomeFailed, err
	}
	if p.beforeCommit != nil {
		p.beforeCommit(o.bucket, o.key)
	}
	ready, err := p.commit(ctx, o, primary, addr, md5hex)
	if err != nil {
		p.abandon(ctx, o, primary, addr)
		return siaOutcomeFailed, err
	}
	if !ready {
		return siaOutcomeChanged, nil
	}
	return siaOutcomePromoted, nil
}

// copy streams the blob and checks it: the stored length, and for an
// unsealed object its MD5 (the ETag of a single-part object, the whole-body
// MD5 of a multipart one when the row records it). A sealed blob's bytes are
// ciphertext: its length is checked here and every segment is
// authenticated when it is served.
func (p *SiaPromoter) copy(ctx context.Context, o siaObject, primary, addr string) (string, error) {
	src, ok := p.eng.GetDriver(p.SourceBackend)
	if !ok {
		return "", fmt.Errorf("source backend %q not registered", p.SourceBackend)
	}
	dst, ok := p.eng.GetDriver(primary)
	if !ok {
		return "", fmt.Errorf("primary backend %q not registered", primary)
	}
	tctx := common.WithTenantID(ctx, o.tenantID)
	stored := storedBlobLength(o.encAlgo, o.size, nil)
	rc, err := src.Get(tctx, o.container(), o.sourceKey())
	if err != nil {
		return "", fmt.Errorf("read %s from %s: %w", o.sourceKey(), p.SourceBackend, err)
	}
	hc := &hashingCounter{r: rc, h: md5.New()} // #nosec G401 -- integrity check of a copy against the object's MD5
	putErr := dst.Put(tctx, o.container(), addr, hc, engine.WithContentLength(stored))
	_ = rc.Close()
	if putErr != nil {
		return "", fmt.Errorf("write %s to %s: %w", addr, primary, putErr)
	}
	if hc.n != stored {
		return "", fmt.Errorf("copy of %s: %d bytes, the object's stored length is %d", o.sourceKey(), hc.n, stored)
	}
	sum := hex.EncodeToString(hc.h.Sum(nil))
	if o.encAlgo == "" {
		want := o.bodyMD5
		if want == "" && len(o.etag) == 32 && !strings.Contains(o.etag, "-") {
			want = o.etag
		}
		if want != "" && !strings.EqualFold(want, sum) {
			return "", fmt.Errorf("copy of %s: MD5 %s, the object's is %s", o.sourceKey(), sum, want)
		}
	}
	return sum, nil
}

// commit marks the row ready under the key's lock while the head row is
// still the object copied and the row still names this copy. false = the
// object changed (or the row was retired by a write or a delete): the copy
// is retired and the row gone.
func (p *SiaPromoter) commit(ctx context.Context, o siaObject, primary, addr, md5hex string) (bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockHeadKey(ctx, tx, o.tenantID, o.bucket, o.key); err != nil {
		return false, err
	}
	rt, err := routeOf(ctx, tx, o.tenantID, o.bucket, o.key, false)
	if err != nil {
		return false, err
	}
	if p.current(rt, o.etag, o.sourceKey()) {
		res, err := tx.ExecContext(ctx, `
			UPDATE sia_promotions SET state = 'ready', ready_at = NOW(), last_accessed = NOW(), promoted_md5 = $7
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND source_key = $4
			  AND promoted_backend = $5 AND promoted_key = $6 AND state = 'pending'`,
			o.tenantID, o.bucket, o.key, o.sourceKey(), primary, addr, md5hex)
		if err != nil {
			return false, fmt.Errorf("mark ready: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return true, tx.Commit()
		}
	}
	if err := p.dropTx(ctx, tx, o, primary, addr); err != nil {
		return false, err
	}
	return false, tx.Commit()
}

// current reports whether rt (the key's route) is the object a row of
// (etag, sourceKey) describes: the head row, on the source backend, at
// that address, with that ETag.
func (p *SiaPromoter) current(rt keyRoute, etag, sourceKey string) bool {
	return rt.headRow && rt.backend == p.SourceBackend && rt.etag == etag && rt.blob == sourceKey
}

// dropTx deletes the row naming (primary, addr), if it is still there, and
// queues the copy for the collector.
func (p *SiaPromoter) dropTx(ctx context.Context, tx *sql.Tx, o siaObject, primary, addr string) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM sia_promotions WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		  AND promoted_backend = $4 AND promoted_key = $5`,
		o.tenantID, o.bucket, o.key, primary, addr); err != nil {
		return fmt.Errorf("drop promotion: %w", err)
	}
	return queueRetired(ctx, tx, o.tenantID, o.container(), primary, addr)
}

// abandon retires a failed promotion's row and whatever of its copy landed,
// on its own short deadline (the promotion's may be spent).
func (p *SiaPromoter) abandon(ctx context.Context, o siaObject, primary, addr string) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), staleCopyTimeout)
	defer cancel()
	err := func() error {
		tx, err := p.db.BeginTx(actx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := lockHeadKey(actx, tx, o.tenantID, o.bucket, o.key); err != nil {
			return err
		}
		if err := p.dropTx(actx, tx, o, primary, addr); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		// The pending row stays: the job retires it once abandoned.
		p.logger.Warn("sia promotion: failed promotion not retired (the job retires it)", zap.Error(err),
			zap.String("tenant_id", o.tenantID), zap.String("bucket", o.bucket), zap.String("key", o.key))
	}
}

// OnObjectChanged retires the key's promoted copies that no longer describe
// its object: called by a DELETE's aftermath once the head row is gone or
// superseded. Best effort — the job retires what this could not.
func (p *SiaPromoter) OnObjectChanged(ctx context.Context, tenantID, bucket, key string) {
	if p == nil {
		return
	}
	if _, err := p.retireKey(ctx, tenantID, bucket, key, time.Time{}); err != nil {
		p.logger.Warn("sia promotion: copies of a deleted object not retired (the job retires them)", zap.Error(err),
			zap.String("tenant_id", tenantID), zap.String("bucket", bucket), zap.String("key", key))
	}
}

// retired is what retireKey removed.
type siaRetired struct{ stale, idle int }

// retireKey retires, under the key's lock, every row of the key that is
// stale (its object is no longer the head row's), a pending promotion
// abandoned past its timeout, and — with ttlCutoff set — a ready copy last
// read before it.
func (p *SiaPromoter) retireKey(ctx context.Context, tenantID, bucket, key string, ttlCutoff time.Time) (siaRetired, error) {
	var out siaRetired
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockHeadKey(ctx, tx, tenantID, bucket, key); err != nil {
		return out, err
	}
	rt, err := routeOf(ctx, tx, tenantID, bucket, key, false)
	if err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT source_key, etag, state, promoted_backend, promoted_key, last_accessed, requested_at, stored_bytes
		FROM sia_promotions WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 FOR UPDATE`,
		tenantID, bucket, key)
	if err != nil {
		return out, fmt.Errorf("read promotions: %w", err)
	}
	type row struct {
		source, etag, state, backend, addr string
		last, requested                    time.Time
		stored                             int64
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.source, &r.etag, &r.state, &r.backend, &r.addr, &r.last, &r.requested, &r.stored); err != nil {
			_ = rows.Close()
			return out, fmt.Errorf("scan promotion: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return out, err
	}
	_ = rows.Close()
	now := p.now()
	o := siaObject{tenantID: tenantID, bucket: bucket, key: key}
	for _, r := range all {
		stale := !p.current(rt, r.etag, r.source) ||
			(r.state == "pending" && now.Sub(r.requested) > promoteTimeout(r.stored)+siaAbandonMargin)
		idle := !stale && r.state == "ready" && !ttlCutoff.IsZero() && r.last.Before(ttlCutoff)
		if !stale && !idle {
			continue
		}
		if err := p.dropTx(ctx, tx, o, r.backend, r.addr); err != nil {
			return siaRetired{}, err
		}
		if stale {
			out.stale++
		} else {
			out.idle++
		}
	}
	if err := tx.Commit(); err != nil {
		return siaRetired{}, fmt.Errorf("commit: %w", err)
	}
	siaPromotions.WithLabelValues(siaOutcomeRetired).Add(float64(out.stale))
	siaPromotions.WithLabelValues(siaOutcomeReclaimed).Add(float64(out.idle))
	return out, nil
}

// dropMissing retires a ready row whose copy a read found missing: the next
// read starts another promotion.
func (p *SiaPromoter) dropMissing(ctx context.Context, o siaObject, backend, addr string) {
	if p == nil {
		return
	}
	p.logger.Error("sia promotion: a promoted copy is missing; retired, the next read promotes again",
		zap.String("tenant_id", o.tenantID), zap.String("bucket", o.bucket), zap.String("key", o.key),
		zap.String("backend", backend), zap.String("copy", addr))
	p.abandon(ctx, o, backend, addr)
}

// SiaPromotionResult is one job run, recorded as job_runs.result.
type SiaPromotionResult struct {
	Keys      int      `json:"keys"`
	Retired   int      `json:"retired"`
	Reclaimed int      `json:"reclaimed"`
	Failed    int      `json:"failed"`
	Errors    []string `json:"errors,omitempty"`
}

const siaPromotionBatch = 1000

// RunOnce retires, key by key, the copies that are stale, abandoned, or
// idle past the TTL — at most one batch of keys per run.
func (p *SiaPromoter) RunOnce(ctx context.Context) (SiaPromotionResult, error) {
	var res SiaPromotionResult
	now := p.now()
	ttlCutoff := now.Add(-p.cfg.TTL)
	rows, err := p.db.QueryContext(ctx, `
		SELECT DISTINCT s.tenant_id, s.bucket, s.object_key FROM sia_promotions s
		LEFT JOIN object_head_cache h ON h.tenant_id = s.tenant_id AND h.bucket = s.bucket AND h.object_key = s.object_key
		WHERE ($1 = '' OR s.tenant_id = $1) AND (
		      h.tenant_id IS NULL OR h.is_chunked OR COALESCE(h.backend_name, '') <> s.source_backend
		   OR h.etag <> s.etag OR COALESCE(h.storage_key, h.object_key) <> s.source_key
		   OR (s.state = 'ready' AND s.last_accessed < $2)
		   OR (s.state = 'pending' AND s.requested_at < $3))
		LIMIT $4`,
		p.onlyTenant, ttlCutoff, now.Add(-promoteTimeout(0)-siaAbandonMargin), siaPromotionBatch)
	if err != nil {
		return res, fmt.Errorf("list promotions to retire: %w", err)
	}
	type keyOf struct{ tenant, bucket, key string }
	var keys []keyOf
	for rows.Next() {
		var k keyOf
		if err := rows.Scan(&k.tenant, &k.bucket, &k.key); err != nil {
			_ = rows.Close()
			return res, fmt.Errorf("scan: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return res, err
	}
	_ = rows.Close()
	for _, k := range keys {
		if ctx.Err() != nil {
			break
		}
		res.Keys++
		r, err := p.retireKey(ctx, k.tenant, k.bucket, k.key, ttlCutoff)
		if err != nil {
			res.Failed++
			if len(res.Errors) < 20 {
				res.Errors = append(res.Errors, fmt.Sprintf("%s %s/%s: %v", k.tenant, k.bucket, k.key, err))
			}
			continue
		}
		res.Retired += r.stale
		res.Reclaimed += r.idle
	}
	return res, nil
}

func (p *SiaPromoter) spec() jobSpec {
	return jobSpec{Name: p.JobName, Every: 10 * time.Minute, BootDelay: 6 * time.Minute, MaxRunTime: 9 * time.Minute,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := p.RunOnce(ctx)
			p.logger.Info("sia promotion run", zap.Int("keys", res.Keys), zap.Int("retired", res.Retired),
				zap.Int("reclaimed", res.Reclaimed), zap.Int("failed", res.Failed), zap.Error(err))
			rep := jobReport{Rows: int64(res.Retired + res.Reclaimed), Result: res}
			if res.Failed > 0 {
				rep.Note = fmt.Sprintf("%d key(s) not retired this run: %s", res.Failed, strings.Join(res.Errors, "; "))
			}
			return rep, err
		}}
}

// Wait waits for the promotions this process started (tests).
func (p *SiaPromoter) Wait() {
	if p != nil {
		p.wg.Wait()
	}
}

// writeSiaWait answers a read that waits on a promotion: 503 SlowDown +
// Retry-After, marked as one object's state (not a server error).
func writeSiaWait(w http.ResponseWriter, r *http.Request, retryAfter int) {
	markClientRefusal(r.Context())
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	WriteS3ErrorWithContext(w, ErrSlowDown, r.URL.Path, generateRequestID(), WithMessage(siaHydrateMessage))
}

// siaWantBytes is how many bytes a GET asks for: the range's length, the
// whole object for no range or a multi-range (served whole); 0 for a range
// the object cannot satisfy (answered 416 further on).
func siaWantBytes(r *http.Request, size int64) int64 {
	rh := r.Header.Get("Range")
	if rh == "" || errors.Is(rangeParseErr(rh, size), errMultiRange) {
		return size
	}
	rng, err := parseRangeHeader(rh, size)
	if err != nil {
		return 0
	}
	return rng.length
}

// siaPromotedBytesCollector exports vaultaire_sia_promoted_bytes: the
// stored bytes of the ready promoted copies, read from the table (15 s
// cache) — cache the primary holds, never billed.
type siaPromotedBytesCollector struct {
	db     *sql.DB
	logger *zap.Logger
	desc   *prometheus.Desc

	mu  sync.Mutex
	at  time.Time
	val *float64
}

func newSiaPromotedBytesCollector(db *sql.DB, logger *zap.Logger) *siaPromotedBytesCollector {
	return &siaPromotedBytesCollector{db: db, logger: logger, desc: prometheus.NewDesc("vaultaire_sia_promoted_bytes",
		"Stored bytes of the ready promoted copies of Sia objects on the primary (a cache, never billed; SIA_PROMOTE_TTL reclaims idle ones).", nil, nil)}
}

func (c *siaPromotedBytesCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *siaPromotedBytesCollector) Collect(ch chan<- prometheus.Metric) {
	if v := c.read(); v != nil {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, *v)
	}
}

func (c *siaPromotedBytesCollector) read() *float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || (c.val != nil && time.Since(c.at) < retiredBlobsBacklogTTL) {
		return c.val
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int64
	if err := c.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(stored_bytes), 0) FROM sia_promotions WHERE state = 'ready'`).Scan(&n); err != nil {
		c.logger.Warn("vaultaire_sia_promoted_bytes: read failed", zap.Error(err))
		return c.val
	}
	v := float64(n)
	c.val, c.at = &v, time.Now()
	return c.val
}

// retireSiaPromotionsOnWrite runs in a whole-object or chunked write's flip
// transaction (commitBlobWrite): every promoted copy of the key that names
// another address than the new object's is retired with the write — the
// row deleted, the copy queued for the collector.
func retireSiaPromotionsOnWrite(ctx context.Context, tx *sql.Tx, tenantID, bucket, key, newAddr string) error {
	if _, err := tx.ExecContext(ctx, `
		WITH gone AS (
			DELETE FROM sia_promotions WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND source_key <> $4
			RETURNING promoted_backend, promoted_key)
		INSERT INTO retired_blobs (backend_id, container, storage_key, tenant_id, reason, retire_after)
		SELECT promoted_backend, $5, promoted_key, $1, $6, NOW() + make_interval(secs => $7) FROM gone
		ON CONFLICT (backend_id, container, storage_key) DO UPDATE SET reason = EXCLUDED.reason, retire_after = EXCLUDED.retire_after`,
		tenantID, bucket, key, newAddr, tenantID+"_"+bucket, retiredSiaPromotion, retiredBlobGrace.Seconds()); err != nil {
		return fmt.Errorf("retire promoted copies: %w", err)
	}
	return nil
}
