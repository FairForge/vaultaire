package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// The Vault parity second copy (WP-VAULT-1 part 2, flag `vault_parity`).
//
// What the second copy IS: Reed-Solomon 4+4 over the object (vault_parity_codec.go).
// The four DATA shards are not stored — they are byte ranges of the object
// the tape already holds. The four PARITY shards, together exactly the
// object's size (rounded up to whole stripes), are written to a free leg —
// the first registered of `sync`, `permafrost`, `lyve` (VAULT_PARITY_LEGS) — by
// the `vault_parity` job behind the commit. Any four of the eight shards
// rebuild the object, so the four parity shards ALONE are a complete copy in
// information terms: when Geyser answers an error, is not found, or its
// breaker is open, a read is served by rebuilding from the parity (bench
// §16.3: 64 MiB in 1.5 s without touching tape; Geyser lost for 0.95 s with
// the data still arriving); when a parity shard is gone too, from the data
// ranges Geyser CAN serve plus the parity that is left.
//
// What it IS NOT: a byte-for-byte mirror a plain GET can read (it needs the
// decode — 1–3 GB/s per core, nothing); not synchronous (the window between
// the commit and the job's next pass, two minutes, has one copy); not on a
// second paid vendor (the leg is a free account whose durability is the
// leg's); and not for chunked objects — a vault-floor object is never
// chunked (storageClassDisablesChunking; asserted, not handled).
//
// One vault_parity row per object (migration 077), bound to the etag the
// shards were computed from: an overwrite makes the row stale, the job
// erases the shards and writes new ones. A write that landed on some shards
// and failed on others leaves state = 'partial' with the legs that hold a
// shard and the error, and is retried. DeleteObject erases the shards at
// once (best effort; the job's stale pass is the guarantee); an account
// erasure erases every row's shards before the sweep and DEFERS the tenant
// when the leg is not registered or its breaker is open (WP-R10-3c).

const (
	// parityBucket is the system bucket the shards live in: a name no S3
	// bucket can take (like `_exports`), so the container is
	// `<tenant>__parity` and the erasure sweep lists it like any bucket.
	parityBucket = "_parity"
	// vaultParityStripe is the bytes per shard per stripe: one stripe covers
	// 4 MiB of the object; encode and rebuild hold one stripe (8 MiB).
	vaultParityStripe = 1 << 20
	vaultParityK      = 4
	vaultParityM      = 4
	// vaultParityMaxAttempts: a row that could not be completed this many
	// times is left for an operator (it stays 'partial' and in the admin
	// result), not retried every two minutes forever.
	vaultParityMaxAttempts = 20
)

// vaultParityLegs is the default preference order of the parity legs: the
// first one registered takes every new parity shard of a deployment. Sync
// first (owner decision 2026-10-07: Sync approved the reseller use; its
// parity rebuilds at ~110 MB/s vs ~60 for the OneDrive fleet and ~240 for
// Lyve, which is free only until its promo ends —
// bench-results/SYNC-WORKLOADS-2026-10-07.md). VAULT_PARITY_LEGS
// (comma-separated driver names) overrides it. A complete row keeps the leg
// it records (vault_parity.legs), whatever the order now; a partial row
// retried after the order changed has its old leg's shards erased before
// the new leg is written (clearPriorShards) — or is skipped this run when
// that leg cannot be reached, so no shard is ever left with no row naming it.
var vaultParityLegs = []string{"sync", "permafrost", "lyve"}

// parityLegsFromEnv is VAULT_PARITY_LEGS split and trimmed, or the default
// when it names nothing. Names no driver is registered under are skipped by
// Leg(), so a typo falls through to the next leg rather than disabling the
// job.
func parityLegsFromEnv(getenv func(string) string) []string {
	var legs []string
	for _, n := range strings.Split(getenv("VAULT_PARITY_LEGS"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			legs = append(legs, n)
		}
	}
	if len(legs) == 0 {
		return append([]string(nil), vaultParityLegs...)
	}
	return legs
}

var (
	vaultParityObjects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_vault_parity_objects_total",
		Help: "Vault parity outcomes by the job: complete (four shards written), partial (some shards), failed (nothing written), erased (stale or deleted object's shards removed).",
	}, []string{"outcome"})
	vaultParityBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vaultaire_vault_parity_bytes_total",
		Help: "Parity bytes written to the free leg.",
	})
	vaultParityFallbacks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_vault_parity_fallback_reads_total",
		Help: "Reads of a vault object whose backend failed: served (rebuilt from the parity copy) or unavailable (no usable parity either).",
	}, []string{"outcome"})
	vaultParityOrphans = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_vault_parity_orphans_total",
		Help: "Parity shard folders no row names, as the job's reconcile pass meets them: found (a sighting; counted on every pass), erased (gone after two sightings at least VAULT_PARITY_ORPHAN_GRACE apart), failed (a delete that did not go through).",
	}, []string{"outcome"})
)

func init() {
	for _, o := range []string{"complete", "partial", "failed", "erased"} {
		vaultParityObjects.WithLabelValues(o)
	}
	for _, o := range []string{"served", "unavailable"} {
		vaultParityFallbacks.WithLabelValues(o)
	}
	for _, o := range []string{"found", "erased", "failed"} {
		vaultParityOrphans.WithLabelValues(o)
	}
}

const (
	// defaultOrphanGrace: a shard folder no row names is erased only once it
	// has been seen on two passes this far apart (VAULT_PARITY_ORPHAN_GRACE).
	defaultOrphanGrace = time.Hour
	// defaultMaxOrphanCandidatesPerRun bounds the reconcile walk of one run.
	defaultMaxOrphanCandidatesPerRun = 2000
	// defaultMaxReconcileTenantsPerRun: tenants walked per run (in rotation).
	defaultMaxReconcileTenantsPerRun = 25
)

// orphanGraceFromEnv is VAULT_PARITY_ORPHAN_GRACE (1m–24h), else the default;
// a rejected value is logged at Warn and the default kept.
func orphanGraceFromEnv(getenv func(string) string, logger *zap.Logger) time.Duration {
	raw := strings.TrimSpace(getenv("VAULT_PARITY_ORPHAN_GRACE"))
	if raw == "" {
		return defaultOrphanGrace
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < time.Minute || d > 24*time.Hour {
		logger.Warn("VAULT_PARITY_ORPHAN_GRACE rejected; default kept", zap.String("value", raw), zap.Duration("default", defaultOrphanGrace))
		return defaultOrphanGrace
	}
	return d
}

// VaultParityResult is one run's outcome (job_runs.result).
type VaultParityResult struct {
	Scanned     int `json:"scanned"`
	Protected   int `json:"protected"`
	Partial     int `json:"partial"`
	Failed      int `json:"failed"`
	Erased      int `json:"erased"`
	EraseFailed int `json:"erase_failed"`
	// The reconcile pass (shard folders no row names, PR 5 of Prompt 2a):
	// candidates met this run, erased after the grace, deletes that failed;
	// the legs walked (registered, breaker closed), tenants walked, and
	// whether the per-run bound cut the walk short.
	OrphansFound       int      `json:"orphans_found"`
	OrphansErased      int      `json:"orphans_erased"`
	OrphansFailed      int      `json:"orphans_failed"`
	ReconciledLegs     []string `json:"reconciled_legs,omitempty"`
	ReconcileTenants   int      `json:"reconcile_tenants"`
	ReconcileTruncated bool     `json:"reconcile_truncated,omitempty"`
	// Skipped: candidates whose row names shards that could not be erased
	// this run (an earlier etag, or another leg): never rewritten, retried
	// next run.
	Skipped int `json:"skipped,omitempty"`
	// DeletedMeanwhile: objects deleted (or overwritten) while their shards
	// were being written; the shards were erased with the row.
	DeletedMeanwhile int      `json:"deleted_meanwhile,omitempty"`
	ChunkedSkipped   int      `json:"chunked_skipped"`
	FlagOff          int      `json:"flag_off"`
	BytesWritten     int64    `json:"bytes_written"`
	Leg              string   `json:"leg"`
	Errors           []string `json:"errors,omitempty"`
}

// VaultParity writes, erases and reads the parity copy.
type VaultParity struct {
	db     *sql.DB
	eng    *engine.CoreEngine
	flags  flagChecker
	logger *zap.Logger

	JobName          string
	Every            time.Duration
	BootDelay        time.Duration
	MaxRunTime       time.Duration
	MaxObjectsPerRun int
	MaxBytesPerRun   int64
	Stripe           int

	// Legs is the parity-leg preference order (VAULT_PARITY_LEGS, else
	// vaultParityLegs).
	Legs []string

	// The reconcile pass: a shard folder no row names is erased once it has
	// been a candidate on two passes OrphanGrace apart; at most
	// MaxOrphanCandidatesPerRun candidates and MaxReconcileTenantsPerRun
	// tenants (in rotation, reconcileCursor) per run.
	OrphanGrace               time.Duration
	MaxOrphanCandidatesPerRun int
	MaxReconcileTenantsPerRun int
	reconcileCursor           string

	// scopeTenant limits a run to one tenant (tests on the shared DB).
	scopeTenant string
	now         func() time.Time
}

// NewVaultParity returns nil without a database or a driver-exposing engine.
func NewVaultParity(db *sql.DB, eng engine.Engine, fl flagChecker, logger *zap.Logger) *VaultParity {
	ce, ok := eng.(*engine.CoreEngine)
	if db == nil || !ok {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &VaultParity{db: db, eng: ce, flags: fl, logger: logger,
		JobName: "vault_parity", Every: 2 * time.Minute, BootDelay: 45 * time.Second, MaxRunTime: time.Hour,
		MaxObjectsPerRun: 500, MaxBytesPerRun: 256 << 30, Stripe: vaultParityStripe, now: time.Now,
		Legs:        parityLegsFromEnv(os.Getenv),
		OrphanGrace: orphanGraceFromEnv(os.Getenv, logger), MaxOrphanCandidatesPerRun: defaultMaxOrphanCandidatesPerRun,
		MaxReconcileTenantsPerRun: defaultMaxReconcileTenantsPerRun}
}

// Leg is the leg this deployment writes new parity to: the first registered
// of p.Legs.
func (p *VaultParity) Leg() (string, engine.Driver, bool) {
	legs := p.Legs
	if len(legs) == 0 {
		legs = vaultParityLegs
	}
	for _, name := range legs {
		if d, ok := p.eng.GetDriver(name); ok {
			return name, d, true
		}
	}
	return "", nil, false
}

func (p *VaultParity) enabled(tenantID string) bool {
	return p.flags != nil && p.flags.Enabled(flagVaultParity, tenantID)
}

// spec is the job as the scheduler runs it (WP-R13-3 rules): an interval
// job; a run is an error only when the tables cannot be read; one object
// that could not be protected or erased is a note.
func (p *VaultParity) spec() jobSpec {
	return jobSpec{Name: p.JobName, Every: p.Every, BootDelay: p.BootDelay, MaxRunTime: p.MaxRunTime,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := p.RunOnce(ctx)
			rep := jobReport{Rows: int64(res.Protected + res.Erased), Result: res}
			var notes []string
			if errors.Is(err, context.DeadlineExceeded) {
				notes = append(notes, "stopped at the run ceiling")
				err = nil
			}
			if n := len(res.Errors); n > 0 {
				notes = append(notes, fmt.Sprintf("%d item(s) failed, first: %s", n, res.Errors[0]))
			}
			if res.ChunkedSkipped > 0 {
				notes = append(notes, fmt.Sprintf("%d chunked vault-floor row(s) — an invariant is broken, see the log", res.ChunkedSkipped))
			}
			rep.Note = strings.Join(notes, "; ")
			return rep, err
		}}
}

// RunOnce does one pass: erase the shards of stale rows (the object is gone
// or has a new etag), then protect the vault-floor objects without a
// complete row. The leg is read once per run.
func (p *VaultParity) RunOnce(ctx context.Context) (VaultParityResult, error) {
	var res VaultParityResult
	legName, _, ok := p.Leg()
	if !ok {
		return res, fmt.Errorf("no parity leg is registered (%s)", strings.Join(p.Legs, ", "))
	}
	res.Leg = legName
	if err := p.eraseStale(ctx, &res); err != nil {
		return res, err
	}
	if err := p.protectPending(ctx, &res); err != nil {
		return res, err
	}
	p.reconcileOrphans(ctx, &res)
	return res, ctx.Err()
}

// emptyDirRemover is the optional driver surface the reconcile pass uses to
// take away the empty `<digest>/<etag>/` folders a Delete leaves (local,
// webdav; nothing to do on an S3 leg). Best effort.
type emptyDirRemover interface {
	RemoveEmptyDir(ctx context.Context, container, dir string) error
}

// reconcileOrphans is the pass over `<tenant>__parity/` on every registered
// leg whose breaker is closed: a `<digest>/<etag>/` folder no row names is
// a candidate; a candidate first seen at least OrphanGrace ago is erased
// (its files, then its folders) and forgotten; one that gained its row or
// vanished is forgotten. Nothing here fails the run: a leg that cannot be
// listed or a delete that fails is a note. The walk is bounded per run.
func (p *VaultParity) reconcileOrphans(ctx context.Context, res *VaultParityResult) {
	type leg struct {
		name string
		drv  engine.Driver
	}
	status := p.eng.GetFailoverStatus()
	names := p.Legs
	if len(names) == 0 {
		names = vaultParityLegs
	}
	var legs []leg
	for _, name := range names {
		drv, ok := p.eng.GetDriver(name)
		if !ok {
			continue
		}
		if status[name] == engine.StateOpen.String() {
			res.Errors = append(res.Errors, fmt.Sprintf("reconcile: %s skipped, circuit breaker open", name))
			continue
		}
		legs = append(legs, leg{name, drv})
		res.ReconciledLegs = append(res.ReconciledLegs, name)
	}
	if len(legs) == 0 {
		return
	}
	tenants, err := p.reconcileTenants(ctx)
	if err != nil {
		res.Errors = append(res.Errors, "reconcile: tenants: "+err.Error())
		return
	}
	budget := p.MaxOrphanCandidatesPerRun
	if budget <= 0 {
		budget = defaultMaxOrphanCandidatesPerRun
	}
	grace := p.OrphanGrace
	if grace <= 0 {
		grace = defaultOrphanGrace
	}
	for _, tenantID := range tenants {
		if ctx.Err() != nil {
			return
		}
		res.ReconcileTenants++
		for _, l := range legs {
			if res.ReconcileTruncated {
				return
			}
			p.reconcileTenantLeg(ctx, res, tenantID, l.name, l.drv, grace, &budget)
		}
	}
}

// reconcileTenants is the tenants this run walks: the scoped one, or the
// next page of the tenants table after the cursor (wrapping at the end).
func (p *VaultParity) reconcileTenants(ctx context.Context) ([]string, error) {
	if p.scopeTenant != "" {
		return []string{p.scopeTenant}, nil
	}
	limit := p.MaxReconcileTenantsPerRun
	if limit <= 0 {
		limit = defaultMaxReconcileTenantsPerRun
	}
	rows, err := p.db.QueryContext(ctx, `SELECT id FROM tenants WHERE id > $1 ORDER BY id LIMIT $2`, p.reconcileCursor, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) < limit {
		p.reconcileCursor = "" // wrap: the next run starts over
	} else {
		p.reconcileCursor = ids[len(ids)-1]
	}
	return ids, nil
}

// reconcileTenantLeg is one tenant's parity container on one leg.
func (p *VaultParity) reconcileTenantLeg(ctx context.Context, res *VaultParityResult, tenantID, legName string, drv engine.Driver, grace time.Duration, budget *int) {
	tctx := common.WithTenantID(ctx, tenantID)
	container := parityContainer(tenantID)
	names, err := drv.List(tctx, container, "")
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("reconcile: list %s on %s: %v", container, legName, err))
		return
	}
	// The folders the leg holds and the files under each.
	folders := map[string][]string{}
	for _, name := range names {
		i := strings.LastIndex(name, "/")
		if i <= 0 {
			continue // not <digest>/<etag>/<file>
		}
		folders[name[:i]] = append(folders[name[:i]], name)
	}
	// The folders rows name.
	named := map[string]bool{}
	rows, err := p.db.QueryContext(ctx, `SELECT shard_prefix FROM vault_parity WHERE tenant_id = $1`, tenantID)
	if err != nil {
		res.Errors = append(res.Errors, "reconcile: rows of "+tenantID+": "+err.Error())
		return
	}
	for rows.Next() {
		var prefix string
		if err := rows.Scan(&prefix); err != nil {
			_ = rows.Close()
			res.Errors = append(res.Errors, "reconcile: rows of "+tenantID+": "+err.Error())
			return
		}
		named[prefix] = true
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		res.Errors = append(res.Errors, "reconcile: rows of "+tenantID+": "+err.Error())
		return
	}
	var candidates []string
	for folder := range folders {
		if !named[folder] {
			candidates = append(candidates, folder)
		}
	}
	sort.Strings(candidates)
	if len(candidates) > *budget {
		candidates = candidates[:*budget]
		res.ReconcileTruncated = true
	}
	*budget -= len(candidates)

	now := p.now()
	var kept []string // sightings that stay: still candidates, not yet erased
	for _, folder := range candidates {
		res.OrphansFound++
		vaultParityOrphans.WithLabelValues("found").Inc()
		var firstSeen time.Time
		err := p.db.QueryRowContext(ctx, `
			INSERT INTO vault_parity_orphans (tenant_id, leg, prefix, first_seen, last_seen) VALUES ($1, $2, $3, $4, $4)
			ON CONFLICT (tenant_id, leg, prefix) DO UPDATE SET last_seen = EXCLUDED.last_seen
			RETURNING first_seen`, tenantID, legName, folder, now).Scan(&firstSeen)
		if err != nil {
			res.Errors = append(res.Errors, "reconcile: sighting of "+folder+": "+err.Error())
			kept = append(kept, folder)
			continue
		}
		if now.Sub(firstSeen) < grace {
			kept = append(kept, folder)
			continue
		}
		if err := p.eraseOrphanFolder(tctx, drv, container, folder, folders[folder], folders, named); err != nil {
			res.OrphansFailed++
			vaultParityOrphans.WithLabelValues("failed").Inc()
			res.Errors = append(res.Errors, fmt.Sprintf("reconcile: erase %s on %s: %v", folder, legName, err))
			kept = append(kept, folder)
			continue
		}
		res.OrphansErased++
		vaultParityOrphans.WithLabelValues("erased").Inc()
		p.logger.Warn("vault parity: orphan shards erased (no row named them)",
			zap.String("tenant_id", tenantID), zap.String("leg", legName), zap.String("folder", folder),
			zap.Int("files", len(folders[folder])), zap.Duration("first_seen_ago", now.Sub(firstSeen)))
	}
	if res.ReconcileTruncated {
		return // the unseen candidates keep their sightings
	}
	// Sightings of folders that are no longer candidates on this leg (the
	// row appeared, the folder is gone, or they were erased above). An
	// empty array, never NULL: `= ANY(NULL)` is NULL and deletes nothing.
	if kept == nil {
		kept = []string{}
	}
	if _, err := p.db.ExecContext(ctx, `
		DELETE FROM vault_parity_orphans WHERE tenant_id = $1 AND leg = $2 AND NOT (prefix = ANY($3))`,
		tenantID, legName, pq.Array(kept)); err != nil {
		res.Errors = append(res.Errors, "reconcile: forget sightings: "+err.Error())
	}
}

// eraseOrphanFolder deletes every file under folder, then the folder, then
// its `<digest>` parent when nothing else (on the leg or in a row) sits
// under it. The folder removals are best effort (not every leg has folders).
func (p *VaultParity) eraseOrphanFolder(ctx context.Context, drv engine.Driver, container, folder string, files []string, folders map[string][]string, named map[string]bool) error {
	for _, f := range files {
		if err := drv.Delete(ctx, container, f); err != nil && !isObjectMissingErr(err) {
			return fmt.Errorf("delete %s: %w", f, err)
		}
	}
	rm, ok := drv.(emptyDirRemover)
	if !ok {
		return nil
	}
	if err := rm.RemoveEmptyDir(ctx, container, folder); err != nil {
		p.logger.Debug("vault parity: orphan folder not removed", zap.String("folder", folder), zap.Error(err))
		return nil
	}
	i := strings.Index(folder, "/")
	if i <= 0 {
		return nil
	}
	digest := folder[:i]
	for other := range folders {
		if other != folder && strings.HasPrefix(other, digest+"/") {
			return nil
		}
	}
	for other := range named {
		if strings.HasPrefix(other, digest+"/") {
			return nil
		}
	}
	if err := rm.RemoveEmptyDir(ctx, container, digest); err != nil {
		p.logger.Debug("vault parity: orphan digest folder not removed", zap.String("folder", digest), zap.Error(err))
	}
	return nil
}

type parityRow struct {
	tenantID, bucket, key, etag string
	size, shardBytes            int64
	stripe                      int
	k, m                        int
	prefix                      string
	legs                        []string
	state                       string
	attempts                    int
}

func (p *VaultParity) eraseStale(ctx context.Context, res *VaultParityResult) error {
	rows, err := p.db.QueryContext(ctx, `
		SELECT v.tenant_id, v.bucket, v.object_key, v.etag, v.shard_prefix, v.legs
		FROM vault_parity v
		LEFT JOIN object_head_cache o
		  ON o.tenant_id = v.tenant_id AND o.bucket = v.bucket AND o.object_key = v.object_key
		WHERE (o.tenant_id IS NULL OR o.etag <> v.etag)
		  AND ($1 = '' OR v.tenant_id = $1)
		ORDER BY v.updated_at
		LIMIT 1000`, p.scopeTenant)
	if err != nil {
		return fmt.Errorf("vault parity: stale rows: %w", err)
	}
	var stale []parityRow
	for rows.Next() {
		var r parityRow
		var legs pq.StringArray
		if err := rows.Scan(&r.tenantID, &r.bucket, &r.key, &r.etag, &r.prefix, &legs); err != nil {
			_ = rows.Close()
			return fmt.Errorf("vault parity: scan stale row: %w", err)
		}
		r.legs = []string(legs)
		stale = append(stale, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("vault parity: stale rows: %w", err)
	}
	for _, r := range stale {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := p.eraseShards(ctx, r); err != nil {
			res.EraseFailed++
			res.Errors = append(res.Errors, fmt.Sprintf("erase %s/%s: %v", r.bucket, r.key, err))
			continue
		}
		res.Erased++
		vaultParityObjects.WithLabelValues("erased").Inc()
	}
	return nil
}

type parityCandidate struct {
	tenantID, bucket, key, etag string
	size                        int64
	chunked                     bool
}

func (p *VaultParity) protectPending(ctx context.Context, res *VaultParityResult) error {
	rows, err := p.db.QueryContext(ctx, `
		SELECT o.tenant_id, o.bucket, o.object_key, o.etag, o.size_bytes, o.is_chunked
		FROM object_head_cache o
		LEFT JOIN vault_parity v
		  ON v.tenant_id = o.tenant_id AND v.bucket = o.bucket AND v.object_key = o.object_key
		WHERE o.floor = 'vault' AND o.size_bytes > 0
		  AND (v.tenant_id IS NULL OR v.etag <> o.etag OR (v.state <> 'complete' AND v.attempts < $2))
		  AND ($1 = '' OR o.tenant_id = $1)
		ORDER BY o.updated_at
		LIMIT $3`, p.scopeTenant, vaultParityMaxAttempts, p.MaxObjectsPerRun)
	if err != nil {
		return fmt.Errorf("vault parity: candidates: %w", err)
	}
	var cands []parityCandidate
	for rows.Next() {
		var c parityCandidate
		if err := rows.Scan(&c.tenantID, &c.bucket, &c.key, &c.etag, &c.size, &c.chunked); err != nil {
			_ = rows.Close()
			return fmt.Errorf("vault parity: scan candidate: %w", err)
		}
		cands = append(cands, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("vault parity: candidates: %w", err)
	}
	var budget int64
	for _, c := range cands {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res.Scanned++
		if c.chunked {
			// Never handled: the archive classes disable chunking at the
			// PUT (storageClassDisablesChunking), so this row is a broken
			// invariant, not a case.
			res.ChunkedSkipped++
			p.logger.Error("vault parity: a chunked object is on the vault floor — invariant broken, not protected",
				zap.String("tenant_id", c.tenantID), zap.String("bucket", c.bucket), zap.String("key", c.key))
			continue
		}
		if !p.enabled(c.tenantID) {
			res.FlagOff++
			continue
		}
		if p.MaxBytesPerRun > 0 && budget+c.size > p.MaxBytesPerRun && budget > 0 {
			break
		}
		budget += c.size
		state, written, err := p.protect(ctx, c)
		res.BytesWritten += written
		switch {
		case err == nil && state == "gone":
			res.DeletedMeanwhile++
			vaultParityObjects.WithLabelValues("erased").Inc()
		case err == nil:
			res.Protected++
			vaultParityObjects.WithLabelValues("complete").Inc()
		case state == "skipped":
			res.Skipped++
			res.Errors = append(res.Errors, fmt.Sprintf("protect %s/%s (skipped, its earlier shards are not erased): %v", c.bucket, c.key, err))
		case state == "partial":
			res.Partial++
			res.Errors = append(res.Errors, fmt.Sprintf("protect %s/%s (partial): %v", c.bucket, c.key, err))
			vaultParityObjects.WithLabelValues("partial").Inc()
		default:
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("protect %s/%s: %v", c.bucket, c.key, err))
			vaultParityObjects.WithLabelValues("failed").Inc()
		}
	}
	return nil
}

// shardPrefix is the artifact prefix of an object's shards inside the
// parity container: a digest of the name (keys are up to 1 KiB and any
// bytes) and the etag the shards belong to.
func shardPrefix(bucket, key, etag string) string {
	sum := sha256.Sum256([]byte(bucket + "\x00" + key))
	return hex.EncodeToString(sum[:12]) + "/" + etag
}

func shardArtifact(prefix string, j int) string { return fmt.Sprintf("%s/p%d", prefix, j) }

func parityContainer(tenantID string) string {
	return (&tenant.Tenant{ID: tenantID}).NamespaceContainer(parityBucket)
}

// tolerantWriter keeps the encoder going when one shard's Put has failed:
// the first error is kept, later writes are dropped. The other shards
// complete; the failed one is ” in legs and the row says 'partial'.
type tolerantWriter struct {
	w   io.Writer
	err error
}

func (t *tolerantWriter) Write(p []byte) (int, error) {
	if t.err != nil {
		return len(p), nil
	}
	n, err := t.w.Write(p)
	if err != nil {
		t.err = err
		return len(p), nil
	}
	return n, nil
}

// protect encodes one object and writes its four parity shards to the leg.
//
// The row is the only thing that names a shard, so a shard must never
// outlive its row and a row must never forget a shard (post-merge review of
// #629): any shard an existing row names that this write will not overwrite
// in place is erased first, or the candidate is skipped this run; the row
// is written 'partial' with the INTENDED leg of every shard before the
// first byte lands, so a concurrent delete knows where to look; a shard
// that failed is deleted and stays recorded unless that delete succeeded;
// and a finish that updates no row — the object was deleted or overwritten
// meanwhile and its row erased — deletes the shards just written. Returns
// the row's state ('complete', 'partial', 'failed', 'skipped', 'gone') and
// the bytes written.
func (p *VaultParity) protect(ctx context.Context, c parityCandidate) (state string, written int64, err error) {
	legName, leg, ok := p.Leg()
	if !ok {
		return "failed", 0, errors.New("no parity leg registered")
	}
	if err := p.clearPriorShards(ctx, c, legName); err != nil {
		return "skipped", 0, err
	}
	l := parityLayout{k: vaultParityK, m: vaultParityM, stripe: p.Stripe, size: c.size}
	prefix := shardPrefix(c.bucket, c.key, c.etag)
	intent := make([]string, l.m)
	for j := range intent {
		intent[j] = legName
	}
	if _, err := p.db.ExecContext(ctx, `
		INSERT INTO vault_parity (tenant_id, bucket, object_key, etag, size_bytes, data_shards, parity_shards,
		                          stripe_bytes, shard_bytes, shard_prefix, legs, state, attempts, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'partial',1,NOW(),NOW())
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
		    etag = EXCLUDED.etag, size_bytes = EXCLUDED.size_bytes, stripe_bytes = EXCLUDED.stripe_bytes,
		    shard_bytes = EXCLUDED.shard_bytes, shard_prefix = EXCLUDED.shard_prefix, legs = EXCLUDED.legs,
		    state = 'partial', written_at = NULL, last_error = NULL,
		    attempts = CASE WHEN vault_parity.etag = EXCLUDED.etag THEN vault_parity.attempts + 1 ELSE 1 END,
		    updated_at = NOW()`,
		c.tenantID, c.bucket, c.key, c.etag, c.size, l.k, l.m, l.stripe, l.shardBytes(), prefix, pq.Array(intent)); err != nil {
		return "failed", 0, fmt.Errorf("vault parity: row: %w", err)
	}

	tctx := common.WithTenantID(ctx, c.tenantID)
	container := parityContainer(c.tenantID)
	// deleteShard removes one shard of this write; true when it is provably
	// gone (deleted, or never there).
	deleteShard := func(j int) bool {
		dctx := common.WithTenantID(context.WithoutCancel(ctx), c.tenantID)
		if derr := leg.Delete(dctx, container, shardArtifact(prefix, j)); derr != nil && !isObjectMissingErr(derr) {
			p.logger.Warn("vault parity: shard of a failed write not deleted; it stays on the row",
				zap.Int("shard", j), zap.String("leg", legName), zap.Error(derr))
			return false
		}
		return true
	}
	// rowGone: the finish updated no row — it was erased meanwhile (the
	// object deleted or overwritten) — so the shards just written have no
	// row and go now, every one of them.
	rowGone := func() (string, int64, error) {
		for j := 0; j < l.m; j++ {
			deleteShard(j)
		}
		p.logger.Info("vault parity: object deleted during the write; its shards erased",
			zap.String("bucket", c.bucket), zap.String("key", c.key))
		return "gone", 0, nil
	}

	src, err := p.eng.Get(tctx, (&tenant.Tenant{ID: c.tenantID}).NamespaceContainer(c.bucket), c.key)
	if err != nil {
		// Nothing was written, but the row keeps naming every shard: an
		// earlier attempt's shards at this prefix were kept for this write
		// to overwrite (clearPriorShards), and a row that named none would
		// orphan them — a delete erases only what the row names.
		ferr := p.finishRow(ctx, c, intent, "", fmt.Errorf("read the object: %w", err))
		if errors.Is(ferr, errParityRowGone) {
			return rowGone()
		}
		return "failed", 0, ferr
	}
	defer func() { _ = src.Close() }()

	writers := make([]io.Writer, l.m)
	tolerant := make([]*tolerantWriter, l.m)
	putErrs := make([]error, l.m)
	var wg sync.WaitGroup
	for j := 0; j < l.m; j++ {
		pr, pw := io.Pipe()
		tolerant[j] = &tolerantWriter{w: pw}
		writers[j] = tolerant[j]
		wg.Add(1)
		go func(j int, pr *io.PipeReader, pw *io.PipeWriter) {
			defer wg.Done()
			perr := leg.Put(tctx, container, shardArtifact(prefix, j), pr,
				engine.WithContentLength(l.shardBytes()), engine.WithContentType("application/octet-stream"))
			if perr != nil {
				putErrs[j] = perr
				_ = pr.CloseWithError(perr) // the encoder's next write to this shard fails; tolerantWriter drops the rest
				return
			}
			_ = pr.Close()
		}(j, pr, pw)
		defer func(pw *io.PipeWriter) { _ = pw.Close() }(pw)
	}
	encErr := encodeParity(l, src, writers)
	for _, t := range tolerant {
		if encErr != nil {
			_ = t.w.(*io.PipeWriter).CloseWithError(encErr)
		} else {
			_ = t.w.(*io.PipeWriter).Close()
		}
	}
	wg.Wait()

	legs := make([]string, l.m)
	var errs []error
	for j := 0; j < l.m; j++ {
		var shardErr error
		switch {
		case encErr != nil:
			// The encode stopped: nothing on the leg is a whole shard.
			shardErr = encErr
		case putErrs[j] != nil:
			shardErr = putErrs[j]
		case tolerant[j].err != nil:
			shardErr = tolerant[j].err
		default:
			legs[j] = legName
			written += l.shardBytes()
			continue
		}
		if encErr == nil {
			errs = append(errs, fmt.Errorf("shard p%d: %w", j, shardErr))
		}
		// Whatever a failed Put left behind is deleted; the leg stays on
		// the row unless it provably is, so the next pass erases it.
		if !deleteShard(j) {
			legs[j] = legName
		}
	}
	var ferr error
	switch {
	case encErr != nil:
		state, ferr = "failed", p.finishRow(ctx, c, legs, "", encErr)
	case len(errs) > 0:
		vaultParityBytes.Add(float64(written))
		state, ferr = "partial", p.finishRow(ctx, c, legs, "", errors.Join(errs...))
	default:
		vaultParityBytes.Add(float64(written))
		state, ferr = "complete", p.finishRow(ctx, c, legs, "complete", nil)
	}
	if errors.Is(ferr, errParityRowGone) {
		return rowGone()
	}
	return state, written, ferr
}

// clearPriorShards erases what an existing row of this key names that the
// coming write will not overwrite in place: every shard of a row with
// another etag (the stale pass could not erase it this run), the shards on
// another leg of a row with this etag (the leg order changed since). A
// shard on this leg at this prefix is replaced by the write itself. An
// error means the candidate must not be written this run.
func (p *VaultParity) clearPriorShards(ctx context.Context, c parityCandidate, legName string) error {
	r, err := p.loadRow(ctx, c.tenantID, c.bucket, c.key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("vault parity: prior row: %w", err)
	}
	if r.etag != c.etag {
		if err := p.eraseShards(ctx, *r); err != nil {
			return fmt.Errorf("prior shards of etag %s: %w", r.etag, err)
		}
		return nil
	}
	if err := p.deleteRecordedShards(ctx, *r, legName); err != nil {
		return fmt.Errorf("prior shards on another leg: %w", err)
	}
	return nil
}

// errParityRowGone: a finish updated no row — it was erased while the
// shards were being written (the object deleted or overwritten).
var errParityRowGone = errors.New("vault parity: row gone during the write")

// finishRow records the outcome on the row; it returns cause (or the row
// error) so the caller's error is the real one. errParityRowGone when the
// row is no longer there for this etag: the caller owns the shards it wrote.
func (p *VaultParity) finishRow(ctx context.Context, c parityCandidate, legs []string, state string, cause error) error {
	var lastErr sql.NullString
	if cause != nil {
		lastErr = sql.NullString{String: cause.Error(), Valid: true}
	}
	var q string
	if state == "complete" {
		q = `UPDATE vault_parity SET legs = $4, state = 'complete', last_error = NULL, written_at = NOW(), updated_at = NOW()
		     WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND etag = $5`
	} else {
		q = `UPDATE vault_parity SET legs = $4, state = 'partial', last_error = $6, written_at = NULL, updated_at = NOW()
		     WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND etag = $5`
	}
	args := []any{c.tenantID, c.bucket, c.key, pq.Array(legs), c.etag}
	if state != "complete" {
		args = append(args, lastErr)
	}
	result, err := p.db.ExecContext(context.WithoutCancel(ctx), q, args...)
	if err != nil {
		p.logger.Error("vault parity: finish row", zap.Error(err), zap.String("bucket", c.bucket), zap.String("key", c.key))
		if cause == nil {
			return fmt.Errorf("vault parity: finish row: %w", err)
		}
		return cause
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return errParityRowGone
	}
	return cause
}

// errParityLegUnavailable: the leg that holds the shards cannot be asked
// right now (not registered, or its breaker is open). An erasure defers.
var errParityLegUnavailable = errors.New("parity leg unavailable")

// eraseShards deletes every shard a row records, then the row. A leg that
// is not registered or whose breaker is open is errParityLegUnavailable and
// the row stays (the bytes may still be there).
func (p *VaultParity) eraseShards(ctx context.Context, r parityRow) error {
	if err := p.deleteRecordedShards(ctx, r, ""); err != nil {
		return err
	}
	if _, err := p.db.ExecContext(ctx, `DELETE FROM vault_parity WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND etag = $4`,
		r.tenantID, r.bucket, r.key, r.etag); err != nil {
		return fmt.Errorf("delete row: %w", err)
	}
	return nil
}

// deleteRecordedShards deletes the shards a row records, except those on
// keepLeg (about to be overwritten in place by a write at the same prefix).
// The first leg that is not registered or whose breaker is open stops it
// with errParityLegUnavailable; a delete that fails stops it with its error.
// A shard already missing is fine.
func (p *VaultParity) deleteRecordedShards(ctx context.Context, r parityRow, keepLeg string) error {
	tctx := common.WithTenantID(ctx, r.tenantID)
	container := parityContainer(r.tenantID)
	status := p.eng.GetFailoverStatus()
	for j, legName := range r.legs {
		if legName == "" || legName == keepLeg {
			continue
		}
		drv, ok := p.eng.GetDriver(legName)
		if !ok {
			return fmt.Errorf("%w: %s is not registered (shard p%d of %s/%s)", errParityLegUnavailable, legName, j, r.bucket, r.key)
		}
		if status[legName] == engine.StateOpen.String() {
			return fmt.Errorf("%w: %s circuit breaker open", errParityLegUnavailable, legName)
		}

		if err := drv.Delete(tctx, container, shardArtifact(r.prefix, j)); err != nil && !isObjectMissingErr(err) {
			return fmt.Errorf("delete shard p%d on %s: %w", j, legName, err)
		}
	}
	return nil
}

func (p *VaultParity) loadRow(ctx context.Context, tenantID, bucket, key string) (*parityRow, error) {
	var r parityRow
	var legs pq.StringArray
	err := p.db.QueryRowContext(ctx, `
		SELECT etag, size_bytes, data_shards, parity_shards, stripe_bytes, shard_bytes, shard_prefix, legs, state, attempts
		FROM vault_parity WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, tenantID, bucket, key).
		Scan(&r.etag, &r.size, &r.k, &r.m, &r.stripe, &r.shardBytes, &r.prefix, &legs, &r.state, &r.attempts)
	if err != nil {
		return nil, err
	}
	r.tenantID, r.bucket, r.key, r.legs = tenantID, bucket, key, []string(legs)
	return &r, nil
}

// OnObjectDeleted erases the object's shards now, best effort: a failure is
// logged and the job's stale pass finishes it. Nil-safe.
func (p *VaultParity) OnObjectDeleted(ctx context.Context, tenantID, bucket, key string) {
	if p == nil {
		return
	}
	r, err := p.loadRow(ctx, tenantID, bucket, key)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		p.logger.Warn("vault parity: row after delete", zap.Error(err), zap.String("bucket", bucket), zap.String("key", key))
		return
	}
	if err := p.eraseShards(ctx, *r); err != nil {
		p.logger.Warn("vault parity: shards not erased with the object; the job will", zap.Error(err),
			zap.String("bucket", bucket), zap.String("key", key))
		return
	}
	vaultParityObjects.WithLabelValues("erased").Inc()
}

// EraseTenant erases every row's shards for an account erasure. The first
// failure stops it and is returned: the runner defers the tenant.
func (p *VaultParity) EraseTenant(ctx context.Context, tenantID string) (int, error) {
	if p == nil {
		return 0, nil
	}
	rows, err := p.db.QueryContext(ctx, `SELECT bucket, object_key FROM vault_parity WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return 0, fmt.Errorf("vault parity rows: %w", err)
	}
	var keys [][2]string
	for rows.Next() {
		var b, k string
		if err := rows.Scan(&b, &k); err != nil {
			_ = rows.Close()
			return 0, err
		}
		keys = append(keys, [2]string{b, k})
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, bk := range keys {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		r, err := p.loadRow(ctx, tenantID, bk[0], bk[1])
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return n, err
		}
		if err := p.eraseShards(ctx, *r); err != nil {
			return n, fmt.Errorf("%s/%s: %w", bk[0], bk[1], err)
		}
		n++
	}
	return n, nil
}

// Open serves [off, off+n) of a vault object from its parity copy: the row
// must be complete for this etag and size, the flag on for the tenant, the
// leg registered. Parity shards are read as streams from the leg (a range
// of each when the leg can); a data piece the parity cannot replace is a
// range of the object from its own backend. Nil-safe.
func (p *VaultParity) Open(ctx context.Context, tenantID, bucket, key, etag string, size, off, n int64) (io.ReadCloser, error) {
	if p == nil {
		return nil, errParityUnavailable
	}
	if !p.enabled(tenantID) {
		return nil, fmt.Errorf("%w: the vault_parity flag is off for this tenant", errParityUnavailable)
	}
	r, err := p.loadRow(ctx, tenantID, bucket, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no parity row", errParityUnavailable)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errParityUnavailable, err)
	}
	if r.state != "complete" || r.etag != etag || r.size != size {
		return nil, fmt.Errorf("%w: row is %s for etag %s/%d bytes, the object is %s/%d", errParityUnavailable, r.state, r.etag, r.size, etag, size)
	}
	if off < 0 || n < 0 || off+n > size {
		return nil, fmt.Errorf("%w: window %d+%d outside %d bytes", errParityUnavailable, off, n, size)
	}
	l := parityLayout{k: r.k, m: r.m, stripe: r.stripe, size: r.size}
	tctx := common.WithTenantID(ctx, tenantID)
	container := parityContainer(tenantID)
	block := int64(l.k * l.stripe)
	firstStripe := off / block
	lastStripe := (off + n + block - 1) / block
	if n == 0 {
		lastStripe = firstStripe
	}
	status := p.eng.GetFailoverStatus()
	streams := make([]io.ReadCloser, l.m)
	opened := 0
	for j := 0; j < l.m; j++ {
		legName := r.legs[j]
		if legName == "" {
			continue
		}
		drv, ok := p.eng.GetDriver(legName)
		if !ok || status[legName] == engine.StateOpen.String() {
			continue
		}
		artifact := shardArtifact(r.prefix, j)
		var rc io.ReadCloser
		var oerr error
		if rg, isRange := drv.(engine.RangeGetter); isRange && (firstStripe > 0 || lastStripe < l.stripes()) {
			rc, oerr = rg.GetRange(tctx, container, artifact, firstStripe*int64(l.stripe), (lastStripe-firstStripe)*int64(l.stripe))
			if oerr == nil {
				// The rebuilder skips to the first stripe itself; a ranged
				// stream already starts there.
				rc = &offsetAdjusted{ReadCloser: rc, pad: firstStripe * int64(l.stripe)}
			}
		} else {
			rc, oerr = drv.Get(tctx, container, artifact)
		}
		if oerr != nil {
			p.logger.Warn("vault parity: shard unreadable", zap.Int("shard", j), zap.String("leg", legName), zap.Error(oerr))
			continue
		}
		streams[j] = rc
		opened++
	}
	objContainer := (&tenant.Tenant{ID: tenantID}).NamespaceContainer(bucket)
	dataPiece := func(ctx context.Context, stripe int64, shard int) ([]byte, error) {
		piece := make([]byte, l.stripe)
		o, m := l.dataPieceRange(stripe, shard)
		if m == 0 {
			return piece, nil
		}
		rc, err := p.eng.GetRange(common.WithTenantID(ctx, tenantID), objContainer, key, o, m)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		if _, err := io.ReadFull(rc, piece[:m]); err != nil {
			return nil, fmt.Errorf("data range %d+%d: %w", o, m, err)
		}
		return piece, nil
	}
	if opened == 0 {
		return nil, fmt.Errorf("%w: no parity shard is readable", errParityUnavailable)
	}
	return newParityRebuilder(ctx, l, streams, dataPiece, off, n), nil
}

// offsetAdjusted makes a ranged shard stream look like a whole one to the
// rebuilder's initial skip: the first `pad` bytes are served as zeros it
// discards, then the real stream.
type offsetAdjusted struct {
	io.ReadCloser
	pad int64
}

func (o *offsetAdjusted) Read(p []byte) (int, error) {
	if o.pad > 0 {
		n := int64(len(p))
		if n > o.pad {
			n = o.pad
		}
		for i := int64(0); i < n; i++ {
			p[i] = 0
		}
		o.pad -= n
		return int(n), nil
	}
	return o.ReadCloser.Read(p)
}
