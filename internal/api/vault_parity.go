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
	// Set by every run (Prompt 2b.2 C4): what the parity does not cover.
	vaultParityUnprotectedBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_vault_parity_unprotected_bytes",
		Help: "Bytes of vault-floor objects with no complete parity copy of their current version, as the job's last run counted them (pending, partial, too large for a run, chunked).",
	})
	vaultParityUnprotectedObjects = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vaultaire_vault_parity_unprotected_objects",
		Help: "Vault-floor objects with no complete parity copy of their current version, as the job's last run counted them, by reason: too_large (a protect estimated longer than a whole run — never started, never takes a slot), flag_off (the tenant's vault_parity flag is off — never protected while it is), other (pending, partial, chunked).",
	}, []string{"reason"})
	vaultParityOrphans = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_vault_parity_orphans_total",
		Help: "Parity shard folders no row names, as the job's reconcile pass meets them: found (a new sighting — once per folder), erased (its files deleted after two sightings at least VAULT_PARITY_ORPHAN_GRACE apart — once per folder), failed (a delete that did not go through).",
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
	for _, r := range []string{"too_large", "other"} {
		vaultParityUnprotectedObjects.WithLabelValues(r)
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
	// Prompt 2a.2 G3: the legs not reconciled and why ("sync: not
	// registered"), empty folders removed, folder listings spent (the
	// per-run budget — on Sync each is a PROPFIND against the account's
	// ~14 ops/s), why the walk stopped short ("time slice", "listing
	// budget"), sightings pruned (an erased tenant, a leg gone), and the
	// reconcile's own state, carried from run to run in job_runs.result:
	// when it last ran (it runs at most every ReconcileEvery) and where each
	// leg's walk resumes.
	SkippedLegs          []string                   `json:"skipped_legs,omitempty"`
	OrphanFoldersRemoved int                        `json:"orphan_folders_removed,omitempty"`
	ReconcileListings    int                        `json:"reconcile_listings"`
	ReconcileStopped     string                     `json:"reconcile_stopped,omitempty"`
	ReconcileDeferred    bool                       `json:"reconcile_deferred,omitempty"`
	SightingsPruned      int                        `json:"sightings_pruned,omitempty"`
	ReconcileAt          time.Time                  `json:"reconcile_at,omitempty"`
	ReconcileCursors     map[string]reconcileCursor `json:"reconcile_cursors,omitempty"`
	// WorkStopped: a protect was not started because its estimate did not
	// fit before the run's deadline (the reconcile runs first, in its own
	// time slice).
	WorkStopped bool `json:"work_stopped,omitempty"`
	// TooLarge: candidates whose protect estimate exceeds a whole run
	// (MaxRunTime): never started — they would be cut on every run.
	TooLarge int `json:"too_large,omitempty"`
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
	// Notes: standing conditions, not failures of this run — the objects
	// too large for a run (the first 20). They used to be Errors, so the
	// admin page said "N failed" on every run (Prompt 2b.3 D2.3).
	Notes []string `json:"notes,omitempty"`
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
	// ProtectRate and ProtectOverhead are a protect's size estimate
	// (size/rate + overhead): a protect is started only when it can finish
	// before the run's deadline (VAULT_PARITY_PROTECT_RATE).
	ProtectRate     int64
	ProtectOverhead time.Duration

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
	// ReconcileEvery: the reconcile runs at most this often
	// (VAULT_PARITY_RECONCILE_EVERY, 30 min); ReconcileSlice: the share of
	// the run it keeps for itself when it is due; MaxReconcileListingsPerRun:
	// folder listings per leg per run (each one PROPFIND on a WebDAV leg,
	// one Graph call per account on the OneDrive fleet).
	ReconcileEvery             time.Duration
	ReconcileSlice             time.Duration
	MaxReconcileListingsPerRun int
	// reconcileMem is the reconcile state when job_runs has none (a test
	// calling RunOnce directly; a first run).
	reconcileMem reconcileState

	// scopeTenant limits a run to one tenant (tests on the shared DB).
	scopeTenant string
	now         func() time.Time
	// beforeFinish (tests) runs after the shards are written, before the
	// row records them.
	beforeFinish func(parityCandidate)
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
		ProtectRate: protectRateFromEnv(os.Getenv, logger), ProtectOverhead: defaultProtectOverhead,
		Legs:        parityLegsFromEnv(os.Getenv),
		OrphanGrace: orphanGraceFromEnv(os.Getenv, logger), MaxOrphanCandidatesPerRun: defaultMaxOrphanCandidatesPerRun,
		MaxReconcileTenantsPerRun: defaultMaxReconcileTenantsPerRun,
		ReconcileEvery:            reconcileEveryFromEnv(os.Getenv, logger), ReconcileSlice: defaultReconcileSlice,
		MaxReconcileListingsPerRun: defaultMaxReconcileListingsPerRun}
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
			if res.WorkStopped {
				notes = append(notes, "a protect whose estimate did not fit before the run's deadline was left for the next run")
			}
			if res.TooLarge > 0 {
				note := fmt.Sprintf("%d protect(s) estimated longer than a whole run, never started", res.TooLarge)
				if len(res.Notes) > 0 {
					note += ", first: " + res.Notes[0]
				}
				notes = append(notes, note)
			}
			if res.ReconcileStopped != "" {
				notes = append(notes, "reconcile stopped at its "+res.ReconcileStopped+", resumes at its cursor")
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
	// The reconcile state is carried into every result, a failed run's too:
	// the scheduler records it, and an empty one wiped every leg's cursor.
	st := p.loadReconcileState(ctx)
	res.ReconcileAt, res.ReconcileCursors = st.At, st.cursors()
	legName, _, ok := p.Leg()
	if !ok {
		return res, fmt.Errorf("no parity leg is registered (%s)", strings.Join(p.Legs, ", "))
	}
	res.Leg = legName
	due := st.At.IsZero() || p.now().Sub(st.At) >= p.reconcileEvery()

	// When the reconcile is due it runs FIRST, in its own slice; the erase
	// and protect passes have what is left (Prompt 2b 0.1: run last, it was
	// starved — a protect started just before its slice ran to the run's
	// deadline, RunOnce returned, and the reconcile never ran while the
	// backlog exceeded one run). A protect is started only when its size
	// estimate fits before the run's deadline, so nothing started is cut.
	if due {
		rctx, cancel := context.WithTimeout(ctx, p.reconcileSlice())
		p.reconcileOrphans(rctx, &res, st)
		cancel()
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
	} else {
		res.ReconcileDeferred = true
	}
	if err := p.eraseStale(ctx, &res); err != nil {
		return res, err
	}
	if err := p.protectPending(ctx, &res); err != nil {
		return res, err
	}
	return res, ctx.Err()
}

const (
	// defaultProtectRate is the throughput a protect's estimate assumes:
	// the object is read from Geyser at ~22 MB/s (8 range streams, bench
	// 2026-10-04 §16.1) and the parity written to Sync at ~27 MB/s.
	defaultProtectRate = 20 << 20
	// defaultProtectOverhead is the fixed part of the estimate (the row,
	// four shard PUTs' first bytes, the finish).
	defaultProtectOverhead = 30 * time.Second
)

// protectRateFromEnv is VAULT_PARITY_PROTECT_RATE in MB/s (1–10000), else
// the default; a rejected value is logged at Warn and the default kept.
func protectRateFromEnv(getenv func(string) string, logger *zap.Logger) int64 {
	raw := strings.TrimSpace(getenv("VAULT_PARITY_PROTECT_RATE"))
	if raw == "" {
		return defaultProtectRate
	}
	var mb int64
	if _, err := fmt.Sscanf(raw, "%d", &mb); err != nil || mb < 1 || mb > 10000 || fmt.Sprint(mb) != raw {
		logger.Warn("VAULT_PARITY_PROTECT_RATE rejected; default kept", zap.String("value", raw), zap.Int64("default_mb_per_s", defaultProtectRate>>20))
		return defaultProtectRate
	}
	return mb << 20
}

// protectEstimate is how long a protect of size bytes is expected to take.
func (p *VaultParity) protectEstimate(size int64) time.Duration {
	rate := p.ProtectRate
	if rate <= 0 {
		rate = defaultProtectRate
	}
	overhead := p.ProtectOverhead
	if overhead <= 0 {
		overhead = defaultProtectOverhead
	}
	return overhead + time.Duration(float64(size)/float64(rate)*float64(time.Second))
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

// eraseStale erases the shards of rows whose object is gone or has a new
// etag; a new erase starts only while the run is live.
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
	// rowAt is the row's updated_at as this protect wrote it: its finish
	// records the outcome only on a row nothing touched since (Prompt 2b.2
	// C4 — a delete marks the row and erases shards meanwhile).
	rowAt time.Time
}

// protectPending protects the vault-floor objects without a complete row; a
// protect starts only when its estimate fits before the run's deadline
// (a smaller one later in the list may still fit). The run's slots
// (MaxObjectsPerRun) go to candidates it can protect (Prompt 2b.2 C4): an
// object whose estimate exceeds a whole run is left out of the candidates
// (counted, named, never started — it used to stay at the head of the
// oldest-first list with no row and take a slot every run, so a newer
// object behind it was never protected), and a candidate skipped on sight —
// a chunked row (a broken invariant), a tenant whose flag is off — is
// passed over without a slot, the list read on past it.
func (p *VaultParity) protectPending(ctx context.Context, res *VaultParityResult) error {
	maxSize := p.maxProtectableSize()
	if err := p.countUnprotected(ctx, res, maxSize); err != nil {
		return err
	}
	cands, err := p.protectCandidates(ctx, res, maxSize)
	if err != nil {
		return err
	}
	var budget int64
	deadline, hasDeadline := ctx.Deadline()
	for _, c := range cands {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p.MaxBytesPerRun > 0 && budget+c.size > p.MaxBytesPerRun && budget > 0 {
			break
		}
		if est := p.protectEstimate(c.size); hasDeadline && time.Until(deadline) < est {
			res.WorkStopped = true
			continue
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

// vaultParityCandidatePages bounds the pages of candidates one run reads
// past skipped rows (each max(MaxObjectsPerRun, 100) rows).
const vaultParityCandidatePages = 20

// maxProtectableSize is the largest object a protect may take on — its
// estimate fits a whole run (-1 = no limit, MaxRunTime unset).
func (p *VaultParity) maxProtectableSize() int64 {
	if p.MaxRunTime <= 0 {
		return -1
	}
	if p.protectEstimate(0) > p.MaxRunTime {
		return 0
	}
	lo, hi := int64(0), int64(1)<<50
	for lo < hi { // the estimate grows with the size: the largest that fits
		mid := lo + (hi-lo+1)/2
		if p.protectEstimate(mid) <= p.MaxRunTime {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// unprotectedFrom: vault-floor objects with no complete parity row of their
// current etag ($1 = the run's tenant scope, ” = all).
const unprotectedFrom = `
		FROM object_head_cache o
		LEFT JOIN vault_parity v
		  ON v.tenant_id = o.tenant_id AND v.bucket = o.bucket AND v.object_key = o.object_key
		WHERE o.floor = 'vault' AND o.size_bytes > 0
		  AND (v.tenant_id IS NULL OR v.etag <> o.etag OR v.state <> 'complete')
		  AND ($1 = '' OR o.tenant_id = $1)`

// countUnprotected sets the unprotected gauges — per tenant, so a tenant
// whose flag is off is flag_off, not "other" (Prompt 2b.3 D2.3: with the
// flag off per tenant, every vault object counted as other) — and names the
// objects too large for a run (res.TooLarge, res.Notes — the first 20).
func (p *VaultParity) countUnprotected(ctx context.Context, res *VaultParityResult, maxSize int64) error {
	rows, err := p.db.QueryContext(ctx, `
		SELECT o.tenant_id, COUNT(*), COALESCE(SUM(o.size_bytes), 0)::BIGINT,
		       COUNT(*) FILTER (WHERE $2::BIGINT >= 0 AND o.size_bytes > $2::BIGINT AND NOT o.is_chunked)`+unprotectedFrom+`
		GROUP BY o.tenant_id`, p.scopeTenant, maxSize)
	if err != nil {
		return fmt.Errorf("vault parity: count unprotected: %w", err)
	}
	var other, large, flagOff int
	var allBytes int64
	var largeTenants []string
	for rows.Next() {
		var tenantID string
		var n, l int
		var b int64
		if err := rows.Scan(&tenantID, &n, &b, &l); err != nil {
			_ = rows.Close()
			return fmt.Errorf("vault parity: count unprotected: %w", err)
		}
		allBytes += b
		switch {
		case !p.enabled(tenantID):
			flagOff += n
		default:
			large += l
			other += n - l
			if l > 0 {
				largeTenants = append(largeTenants, tenantID)
			}
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("vault parity: count unprotected: %w", err)
	}
	vaultParityUnprotectedBytes.Set(float64(allBytes))
	vaultParityUnprotectedObjects.WithLabelValues("too_large").Set(float64(large))
	vaultParityUnprotectedObjects.WithLabelValues("flag_off").Set(float64(flagOff))
	vaultParityUnprotectedObjects.WithLabelValues("other").Set(float64(other))
	res.TooLarge = large
	if large == 0 {
		return nil
	}
	lrows, err := p.db.QueryContext(ctx, `SELECT o.bucket, o.object_key, o.size_bytes`+unprotectedFrom+`
		  AND $2::BIGINT >= 0 AND o.size_bytes > $2::BIGINT AND NOT o.is_chunked
		  AND o.tenant_id = ANY($3)
		ORDER BY o.updated_at LIMIT 20`, p.scopeTenant, maxSize, pq.Array(largeTenants))
	if err != nil {
		return fmt.Errorf("vault parity: too large: %w", err)
	}
	defer func() { _ = lrows.Close() }()
	for lrows.Next() {
		var bucket, key string
		var size int64
		if err := lrows.Scan(&bucket, &key, &size); err != nil {
			return fmt.Errorf("vault parity: too large: %w", err)
		}
		res.Notes = append(res.Notes, fmt.Sprintf("protect %s/%s not started: estimated %s exceeds a whole run (%s)",
			bucket, key, p.protectEstimate(size).Round(time.Second), p.MaxRunTime))
	}
	if err := lrows.Err(); err != nil {
		return fmt.Errorf("vault parity: too large: %w", err)
	}
	return nil
}

// protectCandidates reads up to MaxObjectsPerRun objects to protect, oldest
// first: the ones it will not protect (a chunked row, a flag-off tenant)
// are counted and passed over without a slot, and objects larger than
// maxSize are not read at all.
func (p *VaultParity) protectCandidates(ctx context.Context, res *VaultParityResult, maxSize int64) ([]parityCandidate, error) {
	if p.MaxObjectsPerRun <= 0 {
		return nil, nil
	}
	page := max(p.MaxObjectsPerRun, 100)
	var (
		cands            []parityCandidate
		curAt            time.Time
		curT, curB, curK string
		first            = true
	)
	for n := 0; n < vaultParityCandidatePages && len(cands) < p.MaxObjectsPerRun; n++ {
		rows, err := p.db.QueryContext(ctx, `
			SELECT o.tenant_id, o.bucket, o.object_key, o.etag, o.size_bytes, o.is_chunked, o.updated_at`+unprotectedFrom+`
			  AND (v.tenant_id IS NULL OR v.etag <> o.etag OR v.attempts < $2)
			  AND ($3::BIGINT < 0 OR o.size_bytes <= $3::BIGINT)
			  AND ($4::BOOLEAN OR (o.updated_at, o.tenant_id, o.bucket, o.object_key) > ($5::TIMESTAMPTZ, $6::TEXT, $7::TEXT, $8::TEXT))
			ORDER BY o.updated_at, o.tenant_id, o.bucket, o.object_key
			LIMIT $9`, p.scopeTenant, vaultParityMaxAttempts, maxSize, first, curAt, curT, curB, curK, page)
		if err != nil {
			return nil, fmt.Errorf("vault parity: candidates: %w", err)
		}
		read := 0
		for rows.Next() {
			var c parityCandidate
			if err := rows.Scan(&c.tenantID, &c.bucket, &c.key, &c.etag, &c.size, &c.chunked, &curAt); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("vault parity: scan candidate: %w", err)
			}
			read++
			curT, curB, curK = c.tenantID, c.bucket, c.key
			if len(cands) >= p.MaxObjectsPerRun {
				continue
			}
			res.Scanned++
			switch {
			case c.chunked:
				// Never handled: the archive classes disable chunking at
				// the PUT (storageClassDisablesChunking), so this row is a
				// broken invariant, not a case.
				res.ChunkedSkipped++
				p.logger.Error("vault parity: a chunked object is on the vault floor — invariant broken, not protected",
					zap.String("tenant_id", c.tenantID), zap.String("bucket", c.bucket), zap.String("key", c.key))
			case !p.enabled(c.tenantID):
				res.FlagOff++
			default:
				cands = append(cands, c)
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("vault parity: candidates: %w", err)
		}
		first = false
		if read < page {
			break
		}
	}
	return cands, nil
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
	if err := p.db.QueryRowContext(ctx, `
		INSERT INTO vault_parity (tenant_id, bucket, object_key, etag, size_bytes, data_shards, parity_shards,
		                          stripe_bytes, shard_bytes, shard_prefix, legs, state, attempts, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'partial',1,NOW(),NOW())
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
		    etag = EXCLUDED.etag, size_bytes = EXCLUDED.size_bytes, stripe_bytes = EXCLUDED.stripe_bytes,
		    shard_bytes = EXCLUDED.shard_bytes, shard_prefix = EXCLUDED.shard_prefix, legs = EXCLUDED.legs,
		    state = 'partial', written_at = NULL, last_error = NULL,
		    attempts = CASE WHEN vault_parity.etag = EXCLUDED.etag THEN vault_parity.attempts + 1 ELSE 1 END,
		    updated_at = NOW()
		RETURNING updated_at`,
		c.tenantID, c.bucket, c.key, c.etag, c.size, l.k, l.m, l.stripe, l.shardBytes(), prefix, pq.Array(intent)).Scan(&c.rowAt); err != nil {
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

	// Sized like every read of an object (Prompt 2b.2 C3): a backend that
	// can hold two versions of a key serves the one of the row's size.
	src, err := p.eng.Get(engine.WithExpectedSize(tctx, c.size), (&tenant.Tenant{ID: c.tenantID}).NamespaceContainer(c.bucket), c.key)
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
	if p.beforeFinish != nil {
		p.beforeFinish(c)
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
	// The row is the one this protect wrote — its etag AND its updated_at: a
	// delete that ran meanwhile marked it (markIncomplete) and may have
	// erased shards this write had just put down; completing it would name
	// shards that are gone, for good on a same-content re-upload (Prompt
	// 2b.2 C4). Such a finish is errParityRowGone: the caller erases what it
	// wrote and the next run protects again.
	var q string
	if state == "complete" {
		q = `UPDATE vault_parity SET legs = $4, state = 'complete', last_error = NULL, written_at = NOW(), updated_at = NOW()
		     WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND etag = $5 AND updated_at = $6`
	} else {
		q = `UPDATE vault_parity SET legs = $4, state = 'partial', last_error = $7, written_at = NULL, updated_at = NOW()
		     WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND etag = $5 AND updated_at = $6`
	}
	args := []any{c.tenantID, c.bucket, c.key, pq.Array(legs), c.etag, c.rowAt}
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

// parityRowDeleteTimeout bounds the row delete that follows the shard
// deletes, on its own clock.
const parityRowDeleteTimeout = 5 * time.Second

// eraseShards deletes every shard a row records, then the row. A leg that
// is not registered or whose breaker is open is errParityLegUnavailable and
// the row stays (the bytes may still be there). Once the shards are
// provably gone the row delete runs on its own deadline, whatever is left of
// the caller's (Prompt 2a.3 H1: a delete whose budget ended there left a
// `complete` row naming 0 shards, which a same-content re-upload never
// re-protected). No folder is removed here: only the reconcile does, under
// the job lock.
func (p *VaultParity) eraseShards(ctx context.Context, r parityRow) error {
	if err := p.deleteRecordedShards(ctx, r, ""); err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), parityRowDeleteTimeout)
	defer cancel()
	if _, err := p.db.ExecContext(rctx, `DELETE FROM vault_parity WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND etag = $4`,
		r.tenantID, r.bucket, r.key, r.etag); err != nil {
		return fmt.Errorf("delete row: %w", err)
	}
	return nil
}

// deleteRecordedShards deletes the shards a row records, except those on
// keepLeg (about to be overwritten in place by a write at the same prefix).
// The first leg that is not registered or whose breaker is open stops it
// with errParityLegUnavailable; a delete that fails stops it with its error.
// A shard already missing is fine. Before the first delete a `complete` row
// is made `partial` (Prompt 2b 0.2): a delete stopped mid-shards — a leg
// 5xx, the caller's budget — used to leave a `complete` row naming shards
// that are gone, which a same-content re-upload (same etag: the stale pass
// ignores it, protect skips a complete row) never re-protected. A `partial`
// row is protected again on a re-upload and erased by the stale pass
// otherwise; when it cannot be marked, nothing is deleted. The `<etag>`
// folder the deletes empty is left to the reconcile (an empty folder no row
// names goes at its first sighting): only the job writes shards and only
// the job removes folders, under one lock — so no removal can race a write.
//
// The row is marked again when the deletes stop — on an error, and after
// the last one (Prompt 2b.3 D2.2): a protect that upserted AFTER the first
// mark (its finish guard matches its own upsert) and wrote fresh shards
// that these deletes then removed was completed over them — `complete` with
// 2 of 4 shards, never re-protected on a same-content re-upload. Touched
// again, a protect still writing mismatches at its finish (rowGone erases
// what it wrote), and one that has finished is flipped to `partial` and
// protected again by the next run.
func (p *VaultParity) deleteRecordedShards(ctx context.Context, r parityRow, keepLeg string) (err error) {
	tctx := common.WithTenantID(ctx, r.tenantID)
	container := parityContainer(r.tenantID)
	status := p.eng.GetFailoverStatus()
	marked := false
	defer func() {
		if !marked {
			return
		}
		if merr := p.markIncomplete(ctx, r); merr != nil && err == nil {
			err = merr
		}
	}()
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
		if !marked {
			if err := p.markIncomplete(ctx, r); err != nil {
				return err
			}
			marked = true
		}
		if err := drv.Delete(tctx, container, shardArtifact(r.prefix, j)); err != nil && !isObjectMissingErr(err) {
			return fmt.Errorf("delete shard p%d on %s: %w", j, legName, err)
		}
	}
	return nil
}

// markIncomplete makes the row of this etag `partial` (a `complete` one with
// a fresh attempt count), on its own clock: its shards are about to be
// deleted. A `partial` row is marked too (its updated_at): a protect
// writing it right now must not complete it over shards this delete
// removes (Prompt 2b.2 C4).
func (p *VaultParity) markIncomplete(ctx context.Context, r parityRow) error {
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), parityRowDeleteTimeout)
	defer cancel()
	if _, err := p.db.ExecContext(mctx, `
		UPDATE vault_parity SET state = 'partial', written_at = NULL,
		       attempts = CASE WHEN state = 'complete' THEN 0 ELSE attempts END,
		       last_error = 'shards being deleted (a delete that stopped here left this row partial)', updated_at = NOW()
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND etag = $4`,
		r.tenantID, r.bucket, r.key, r.etag); err != nil {
		return fmt.Errorf("mark the row partial before its shard deletes: %w", err)
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
		rc, err := p.eng.GetRange(engine.WithExpectedSize(common.WithTenantID(ctx, tenantID), size), objContainer, key, o, m)
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
