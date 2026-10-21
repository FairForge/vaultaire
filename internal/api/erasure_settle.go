package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// The listing settle of an account erasure (RB-1, the post-merge review of
// the Blomp driver; 2026-10-21).
//
// The sweep (erasure_sweep.go) asks every backend what it still holds for
// the tenant, and a walk that finds nothing is read as "nothing is left".
// On a backend whose listing is eventually consistent that is not proof:
// Blomp's Swift container hid ~30 existing objects from its listing for
// over 90 minutes on 2026-10-21, so a sweep run then would have declared
// the account erased while those bytes stayed. Such a driver says so
// (engine.ListingLagger, Swift's ListingLag: BLOMP_LISTING_SETTLE, 3 h).
//
// On a lagging backend the tenant is declared erased only after a walk
// that STARTED at least the lag after the last moment a write for the
// tenant could land, and found nothing. That moment is the end of stage d:
// the rows are erased (the tenant's api_keys and sts_tokens with them —
// S3 authentication reads the database on every request), the dashboard
// sessions revoked and the in-process credential cache evicted. Any
// earlier moment is not one: until EraseRows the account is open (D-16
// blocks nothing, and the runner never suspends it), so a write of the
// customer's — or a stale copy we leave behind a failed delete — can land
// up to the row erase itself. The one write still possible after it is a
// request authenticated before and still streaming (the gap the erasure
// documents already); the lag is hours, longer than any such request.
//
// So the rows go as before, and the erasure's record is
// `account.erasure_settling` (the counts, the lagging backends, when the
// walk may prove them empty) instead of `account.erased`. What is owed is
// kept in the job's own `job_runs.result` (`settling`: one entry per
// tenant and lagging backend) — written at once when it changes, carried
// into every run's result, never a migration: the job writes it and reads
// it under its own advisory lock. The entry is recorded BEFORE EraseRows
// with no time (an erasure that never reaches its rows drops it at the next
// run, the tenant still existing) and given its time after stage d; a
// process stopped between the two counts the time from the next run.
//
// Each run of the job first walks every entry that is due: everything the
// walk finds is deleted (it always was); a walk that deleted nothing, had
// no failure and started after the entry's due time settles it, and the
// tenant's last settled entry writes `account.erased` (outcome `erased`).
// A walk that found something leaves the entry due — the next hourly check
// walks again (the job's Again). A walk that could not run (no driver, an
// open breaker, a listing or a delete that failed) is a DEFERRED tenant,
// the run an error, retried hourly like any deferral.

// outcomeSettling: the rows are erased, a lagging backend's walk has not
// proven it empty yet.
const outcomeSettling = "settling"

// erasureSettle is one tenant's owed walk of one lagging backend.
type erasureSettle struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	Backend  string `json:"backend"`
	// Settle is the backend's listing lag when the entry was recorded.
	Settle string `json:"settle"`
	// WritesStoppedAt is the end of stage d (zero: recorded before
	// EraseRows, not given its time yet); DueAt = it + the lag.
	WritesStoppedAt time.Time `json:"writes_stopped_at"`
	DueAt           time.Time `json:"due_at"`
	// The walks run so far, the last one's start, what it found and why it
	// could not run, and the blobs the walks deleted.
	Walks      int        `json:"walks,omitempty"`
	LastWalkAt *time.Time `json:"last_walk_at,omitempty"`
	LastFound  int        `json:"last_found,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
	Swept      int        `json:"swept,omitempty"`

	lag time.Duration
}

// erasureSettleState is the job's job_runs.result.
type erasureSettleState struct {
	Settling []erasureSettle `json:"settling"`
}

// laggingBackend is a registered backend whose listing lags.
type laggingBackend struct {
	name string
	lag  time.Duration
}

// laggingBackends are the backends of the plan whose driver declares a
// listing lag.
func (r *AccountDeletionRunner) laggingBackends(backends []string) []laggingBackend {
	ce, ok := r.eng.(*engine.CoreEngine)
	if !ok {
		return nil
	}
	var out []laggingBackend
	for _, b := range backends {
		drv, ok := ce.GetDriver(b)
		if !ok {
			continue
		}
		if l, ok := drv.(engine.ListingLagger); ok && l.ListingLag() > 0 {
			out = append(out, laggingBackend{name: b, lag: l.ListingLag()})
		}
	}
	return out
}

// loadSettles reads the owed walks from the job's job_runs.result.
func (r *AccountDeletionRunner) loadSettles(ctx context.Context) (erasureSettleState, error) {
	var st erasureSettleState
	var raw []byte
	err := r.db.QueryRowContext(ctx, `SELECT COALESCE(result, 'null'::jsonb) FROM job_runs WHERE job = $1`, r.JobName).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("read the owed settle walks: %w", err)
	}
	if len(raw) == 0 || string(raw) == "null" {
		return st, nil
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("read the owed settle walks: %w", err)
	}
	return st, nil
}

// saveSettles writes the owed walks at once, on its own clock: a run that
// is stopped next keeps them (the scheduler records no result for an
// interrupted run).
func (r *AccountDeletionRunner) saveSettles(ctx context.Context, st erasureSettleState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err = r.db.ExecContext(sctx, `
		INSERT INTO job_runs (job, result) VALUES ($1, $2::jsonb)
		ON CONFLICT (job) DO UPDATE SET result = EXCLUDED.result`, r.JobName, string(b))
	if err != nil {
		return fmt.Errorf("record the owed settle walks: %w", err)
	}
	return nil
}

// settleDue is the job's Again: a walk is owed now (or an entry has no time
// yet — the next run gives it one).
func (r *AccountDeletionRunner) settleDue(ctx context.Context) (bool, error) {
	st, err := r.loadSettles(ctx)
	if err != nil {
		return false, err
	}
	now := r.now()
	for _, e := range st.Settling {
		if e.WritesStoppedAt.IsZero() || !now.Before(e.DueAt) {
			return true, nil
		}
	}
	return false, nil
}

// recordSettles adds an entry, with no time yet, per lagging backend of
// the tenant — before EraseRows, so an erasure stopped right after it
// still owes the walk.
func (r *AccountDeletionRunner) recordSettles(ctx context.Context, d account.Due, lagging []laggingBackend) error {
	st := r.settles
	for _, lb := range lagging {
		st.Settling = append(dropSettle(st.Settling, d.TenantID, lb.name),
			erasureSettle{TenantID: d.TenantID, UserID: d.UserID, Backend: lb.name, Settle: lb.lag.String(), lag: lb.lag})
	}
	r.settles = st
	return r.saveSettles(ctx, st)
}

// startSettles gives the tenant's entries the moment writes stopped.
func (r *AccountDeletionRunner) startSettles(ctx context.Context, tenantID string, at time.Time) (time.Time, error) {
	var due time.Time
	for i := range r.settles.Settling {
		e := &r.settles.Settling[i]
		if e.TenantID != tenantID {
			continue
		}
		e.WritesStoppedAt, e.DueAt = at, at.Add(e.lagOrParse())
		if e.DueAt.After(due) {
			due = e.DueAt
		}
	}
	return due, r.saveSettles(ctx, r.settles)
}

// forgetSettles drops the tenant's entries: its erasure did not reach the
// rows (a cancel, an error) — the next attempt records them again.
func (r *AccountDeletionRunner) forgetSettles(ctx context.Context, tenantID string, log *zap.Logger) {
	before := len(r.settles.Settling)
	r.settles.Settling = dropSettle(r.settles.Settling, tenantID, "")
	if len(r.settles.Settling) == before {
		return
	}
	if err := r.saveSettles(ctx, r.settles); err != nil {
		log.Error("account deletion: could not drop the owed settle walks of a tenant whose rows were not erased", zap.Error(err))
	}
}

func dropSettle(in []erasureSettle, tenantID, backend string) []erasureSettle {
	out := in[:0:0]
	for _, e := range in {
		if e.TenantID == tenantID && (backend == "" || e.Backend == backend) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (e erasureSettle) lagOrParse() time.Duration {
	if e.lag > 0 {
		return e.lag
	}
	d, err := time.ParseDuration(e.Settle)
	if err != nil || d <= 0 {
		// An entry we cannot read the lag of waits the longest one we know.
		return settleLagFallback
	}
	return d
}

// settleLagFallback is the wait of an entry whose lag cannot be read.
const settleLagFallback = 48 * time.Hour

// settleErasures walks every owed entry that is due (start of every run).
func (r *AccountDeletionRunner) settleErasures(ctx context.Context, res *AccountDeletionResult) {
	if len(r.settles.Settling) == 0 {
		return
	}
	ce, _ := r.eng.(*engine.CoreEngine)
	now := r.now()
	changed := false
	settled := map[string][]erasureSettle{} // tenant → its entries settled in this run
	var keep []erasureSettle
	for _, e := range r.settles.Settling {
		log := r.logger.With(zap.String("tenant_id", e.TenantID), zap.String("backend", e.Backend))
		if e.WritesStoppedAt.IsZero() {
			exists, err := r.tenantExists(ctx, e.TenantID)
			switch {
			case err != nil:
				res.Errors = append(res.Errors, fmt.Sprintf("tenant %s: settle of %s: %v", e.TenantID, e.Backend, err))
				keep = append(keep, e)
				continue
			case exists:
				// Recorded before an EraseRows that did not happen: the next
				// erasure records it again.
				changed = true
				continue
			}
			e.WritesStoppedAt, e.DueAt = now, now.Add(e.lagOrParse())
			changed = true
			log.Warn("account deletion: a settle walk was recorded without the time writes stopped (the process stopped after the row erase) — counted from now",
				zap.Time("due_at", e.DueAt))
		}
		if now.Before(e.DueAt) {
			res.Notes = append(res.Notes, fmt.Sprintf("tenant %s erased, %s's listing settling: a clean walk after %s finishes the erasure",
				e.TenantID, e.Backend, e.DueAt.UTC().Format(time.RFC3339)))
			keep = append(keep, e)
			continue
		}
		started := r.now()
		found, err := r.settleWalk(ctx, ce, e, log)
		changed = true
		e.Walks++
		e.LastWalkAt, e.LastFound, e.Swept = &started, found, e.Swept+found
		e.LastError = ""
		switch {
		case err != nil:
			e.LastError = err.Error()
			keep = append(keep, e)
			res.Deferred++
			accountDeletionTenants.WithLabelValues(outcomeDeferred).Inc()
			res.Tenants = append(res.Tenants, TenantErasure{TenantID: e.TenantID, UserID: e.UserID, Outcome: outcomeDeferred,
				Error: "settle walk of " + e.Backend + ": " + err.Error()})
			res.Errors = append(res.Errors, fmt.Sprintf("tenant %s deferred: settle walk of %s: %v", e.TenantID, e.Backend, err))
			log.Error("account deletion: the settle walk could not run — the erasure is not finished", zap.Error(err))
		case found > 0:
			keep = append(keep, e)
			res.Notes = append(res.Notes, fmt.Sprintf("tenant %s: the settle walk of %s deleted %d blob(s) the earlier passes did not list — walking again",
				e.TenantID, e.Backend, found))
			log.Warn("account deletion: the settle walk found blobs the earlier passes did not list (the listing lagged) — deleted, walking again",
				zap.Int("found", found))
		default:
			settled[e.TenantID] = append(settled[e.TenantID], e)
			log.Info("account deletion: lagging listing settled — nothing left", zap.Int("walks", e.Walks), zap.Int("swept", e.Swept))
		}
	}
	r.settles.Settling = keep
	if changed {
		if err := r.saveSettles(ctx, r.settles); err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
	}

	tenants := make([]string, 0, len(settled))
	for t := range settled {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)
	for _, t := range tenants {
		if r.owesSettle(t) {
			continue // another lagging backend of the tenant still owes its walk
		}
		r.finishSettled(ctx, settled[t], res)
	}
}

// owesSettle: the tenant still has an entry.
func (r *AccountDeletionRunner) owesSettle(tenantID string) bool {
	for _, e := range r.settles.Settling {
		if e.TenantID == tenantID {
			return true
		}
	}
	return false
}

// finishSettled declares the tenant erased: its last lagging backend has
// been proven empty.
func (r *AccountDeletionRunner) finishSettled(ctx context.Context, entries []erasureSettle, res *AccountDeletionResult) {
	e0 := entries[0]
	te := TenantErasure{TenantID: e0.TenantID, UserID: e0.UserID, Outcome: outcomeErased, Swept: map[string]int{}}
	backends := make([]string, 0, len(entries))
	walks, stopped := map[string]int{}, e0.WritesStoppedAt
	for _, e := range entries {
		backends = append(backends, e.Backend)
		walks[e.Backend] = e.Walks
		if e.Swept > 0 {
			te.Swept[e.Backend] = e.Swept
		}
		te.SweptAfterErase += e.Swept
	}
	te.SweptBackends = backends
	audit.Record(ctx, r.db, audit.Entry{TenantID: e0.TenantID, EventType: "account", Action: "account.erased",
		Severity: "warning", Metadata: map[string]any{
			"actor": "system", "user_id": e0.UserID, "settled_backends": backends,
			"writes_stopped_at": stopped, "settle_walks": walks, "swept_after_settle": te.Swept,
			"rows_erased": "see the tenant's account.erasure_settling row",
		}})
	res.Tenants = append(res.Tenants, te)
	res.Erased++
	accountDeletionTenants.WithLabelValues(outcomeErased).Inc()
	r.logger.Info("account erased — every lagging backend's walk came back empty",
		zap.String("tenant_id", e0.TenantID), zap.Strings("backends", backends), zap.Any("swept_after_settle", te.Swept))
}

// settleWalk walks the tenant on one lagging backend, deleting everything
// it finds (the shared chunk container excepted, as in the sweep): found is
// what it listed (deleted or failed); err when the walk could not run.
func (r *AccountDeletionRunner) settleWalk(ctx context.Context, ce *engine.CoreEngine, e erasureSettle, log *zap.Logger) (int, error) {
	if ce == nil {
		return 0, errors.New("the engine does not expose its drivers")
	}
	if err := sweepableTenantID(e.TenantID); err != nil {
		return 0, err
	}
	drv, ok := ce.GetDriver(e.Backend)
	if !ok {
		return 0, errors.New("the backend is not registered — its bytes cannot be walked")
	}
	if st := ce.GetFailoverStatus()[e.Backend]; st == engine.StateOpen.String() {
		return 0, errors.New("unavailable (circuit breaker open)")
	}
	walker, ok := drv.(engine.TenantWalker)
	if !ok {
		return 0, errors.New("the driver cannot walk a tenant")
	}
	te := &TenantErasure{TenantID: e.TenantID, Swept: map[string]int{}}
	s := &tenantSweep{r: r, ce: ce, d: account.Due{TenantID: e.TenantID, UserID: e.UserID}, te: te, log: log, final: true}
	err := s.walk(ctx, e.Backend, walker)
	found := te.Swept[e.Backend] + te.SweepFailures
	return found, err
}

// tenantExists: the tenant's row is still there (its rows were not erased).
func (r *AccountDeletionRunner) tenantExists(ctx context.Context, tenantID string) (bool, error) {
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenants WHERE id = $1`, tenantID).Scan(&n); err != nil {
		return false, fmt.Errorf("read tenant: %w", err)
	}
	return n > 0, nil
}
