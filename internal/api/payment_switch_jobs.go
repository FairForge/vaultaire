package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/payments"
	"go.uber.org/zap"
)

// WP-PAY-5 (docs/PAYMENTS.md § 4): the two jobs of switching providers.
//
//	renewal_stops         every 10 min: the pending renewal_stop rows — a
//	                      subscription a provider switch took the house from
//	                      whose stop the provider has not confirmed — asked
//	                      again (payments.Registry.StopPendingRenewals).
//	stripe_bulk_renewals  manual: the owner's bulk stop / resume of Stripe
//	                      renewals (§ 4.3), started by
//	                        POST /api/v1/admin/payments/stripe/stop-renewals
//	                        POST /api/v1/admin/payments/stripe/resume-renewals
//	                      {dry_run: true by default} — a dry run answers the
//	                      list at once and writes nothing; a real run answers
//	                      202 and is a job run (lock, job_runs row with its
//	                      result, 409 while one runs). Never cancels a period.

const (
	renewalStopsJobName       = "renewal_stops"
	stripeBulkRenewalsJobName = "stripe_bulk_renewals"

	bulkStop   = "stop"
	bulkResume = "resume"
)

// renewalStopsSpec is the renewal_stops job.
func renewalStopsSpec(reg *payments.Registry) jobSpec {
	return jobSpec{Name: renewalStopsJobName, Every: 10 * time.Minute, BootDelay: 3 * time.Minute, MaxRunTime: 2 * time.Minute,
		Run: func(ctx context.Context) (jobReport, error) {
			run, err := reg.StopPendingRenewals(ctx, "")
			rep := jobReport{Rows: int64(run.Stopped), Result: run}
			if len(run.Errors) > 0 {
				// A provider that failed or is not configured: the row stays
				// pending, the next run asks again — a note, never a failed run.
				rep.Note = fmt.Sprintf("%d stop(s) not confirmed: %s", len(run.Errors), strings.Join(run.Errors, "; "))
			}
			return rep, err
		}}
}

// BulkRenewals is the owner's bulk stop / resume of Stripe renewals.
type BulkRenewals struct {
	db       *sql.DB
	logger   *zap.Logger
	Payments *payments.Registry
	// Mailer sends the per-tenant notice (renewal_stopped.html); nil =
	// none.
	Mailer *payments.Mailer
	// JobName is the job_runs name (tests use their own: the table is
	// shared).
	JobName string

	// onlyTenants scopes a run to some tenants (tests: the database is
	// shared, other packages hold Stripe rows of their own).
	onlyTenants func(tenantID string) bool
}

// NewBulkRenewals builds it; nil without a database or a registry.
func NewBulkRenewals(db *sql.DB, reg *payments.Registry, logger *zap.Logger) *BulkRenewals {
	if db == nil || reg == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &BulkRenewals{db: db, logger: logger, Payments: reg, JobName: stripeBulkRenewalsJobName}
}

// spec is the manual job.
func (b *BulkRenewals) spec() jobSpec {
	return jobSpec{Name: b.JobName, Manual: true, MaxRunTime: 30 * time.Minute,
		Run: func(context.Context) (jobReport, error) {
			return jobReport{}, errors.New("started only by POST /api/v1/admin/payments/stripe/{stop,resume}-renewals")
		}}
}

// BulkCandidate is one row a bulk action would change.
type BulkCandidate struct {
	TenantID    string    `json:"tenant_id"`
	PaidThrough time.Time `json:"paid_through"`
	ProviderRef string    `json:"provider_ref,omitempty"`
}

// BulkResult is a bulk action's outcome (the dry run's answer; a real
// run's job_runs.result).
type BulkResult struct {
	Action  string          `json:"action"`
	DryRun  bool            `json:"dry_run"`
	Tenants []BulkCandidate `json:"tenants"`
	Done    int             `json:"done"`
	Failed  int             `json:"failed"`
	Mailed  int             `json:"mailed"`
	Errors  []string        `json:"errors,omitempty"`
}

// candidates lists the rows a bulk action applies to.
//
//	stop:   every granted house Stripe renews by itself.
//	resume: every granted Stripe house whose renewal is stopped, whose date
//	        still runs, with no pending switch — and whose LAST renewal
//	        change was the bulk stop (a departure from § 4.3: a customer who
//	        stopped their own renewal is never resumed by the owner's bulk).
func (b *BulkRenewals) candidates(ctx context.Context, action string) ([]BulkCandidate, error) {
	q := `
		SELECT e.tenant_id, e.paid_through, COALESCE(e.provider_ref, '') FROM entitlements e
		 WHERE e.provider = 'stripe' AND e.renewal = 'auto' AND e.lapsed_at IS NULL
		 ORDER BY e.paid_through, e.tenant_id`
	if action == bulkResume {
		q = `
		SELECT e.tenant_id, e.paid_through, COALESCE(e.provider_ref, '') FROM entitlements e
		 WHERE e.provider = 'stripe' AND e.renewal = 'prepaid' AND e.lapsed_at IS NULL
		   AND e.paid_through > NOW() AND e.pending_provider IS NULL AND COALESCE(e.provider_ref, '') <> ''
		   AND (SELECT a.metadata->>'reason' FROM audit_logs a
		         WHERE a.tenant_id = e.tenant_id AND a.action IN ('billing.renewal_stopped', 'billing.renewal_resumed')
		         ORDER BY a.timestamp DESC LIMIT 1) = 'bulk'
		 ORDER BY e.paid_through, e.tenant_id`
	}
	rows, err := b.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("bulk %s candidates: %w", action, err)
	}
	defer func() { _ = rows.Close() }()
	out := []BulkCandidate{}
	for rows.Next() {
		var c BulkCandidate
		if err := rows.Scan(&c.TenantID, &c.PaidThrough, &c.ProviderRef); err != nil {
			return nil, fmt.Errorf("bulk %s candidates: %w", action, err)
		}
		if b.onlyTenants != nil && !b.onlyTenants(c.TenantID) {
			continue
		}
		c.PaidThrough = c.PaidThrough.UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// DryRun lists what the action would change and writes nothing.
func (b *BulkRenewals) DryRun(ctx context.Context, action string) (BulkResult, error) {
	cands, err := b.candidates(ctx, action)
	return BulkResult{Action: action, DryRun: true, Tenants: cands}, err
}

// Run applies the action to every candidate, one tenant at a time: under
// the tenant's entitlement lock the row is read again (still a candidate),
// the provider is told (Stripe: cancel_at_period_end true / false — never a
// cancel), the renewal written; after the commit one audit row
// (billing.renewal_stopped / _resumed, reason bulk, the admin as actor) and
// one mail. A tenant that fails is counted and named; the others go on.
func (b *BulkRenewals) Run(ctx context.Context, action, actor string) (BulkResult, error) {
	res := BulkResult{Action: action}
	cands, err := b.candidates(ctx, action)
	if err != nil {
		return res, err
	}
	res.Tenants = cands
	p := b.Payments.Provider(payments.ProviderStripe)
	if p == nil {
		return res, fmt.Errorf("bulk %s: %w: stripe", action, payments.ErrUnknownProvider)
	}
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		e, changed, err := b.one(ctx, p, action, c.TenantID)
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, c.TenantID+": "+err.Error())
			b.logger.Warn("bulk renewals: tenant not changed", zap.String("action", action), zap.String("tenant", c.TenantID), zap.Error(err))
			continue
		}
		if !changed {
			continue
		}
		res.Done++
		renewal, act := payments.RenewalPrepaid, "billing.renewal_stopped"
		if action == bulkResume {
			renewal, act = payments.RenewalAuto, "billing.renewal_resumed"
		}
		audit.Record(ctx, b.db, audit.Entry{Actor: actor, TenantID: c.TenantID, EventType: "billing", Action: act,
			Resource: "tenant:" + c.TenantID, Metadata: map[string]any{"tenant": c.TenantID, "actor": actor, "provider": payments.ProviderStripe,
				"provider_ref": e.ProviderRef, "renewal": string(renewal), "reason": "bulk",
				"paid_through": e.PaidThrough.UTC().Format(time.RFC3339)}})
		if b.Mailer != nil {
			mctx, cancel := payments.Detached(ctx)
			err := b.Mailer.SendRenewalStopped(mctx, c.TenantID, e, action == bulkResume)
			cancel()
			if err != nil {
				res.Errors = append(res.Errors, c.TenantID+": mail: "+err.Error())
			} else {
				res.Mailed++
			}
		}
	}
	return res, nil
}

// one changes one tenant's renewal under its entitlement lock (changed =
// it was still a candidate).
func (b *BulkRenewals) one(ctx context.Context, p payments.Provider, action, tenantID string) (payments.Entitlement, bool, error) {
	var e payments.Entitlement
	changed := false
	err := payments.InTx(ctx, b.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
			return fmt.Errorf("set lock_timeout: %w", err)
		}
		if err := payments.Lock(ctx, tx, tenantID); err != nil {
			return err
		}
		var err error
		if e, err = payments.Get(ctx, tx, tenantID); err != nil {
			return err
		}
		if e.LapsedAt != nil || e.Provider != payments.ProviderStripe {
			return nil
		}
		want, to := payments.RenewalAuto, payments.RenewalPrepaid
		call := p.StopRenewal
		if action == bulkResume {
			want, to, call = payments.RenewalPrepaid, payments.RenewalAuto, p.ResumeRenewal
			if e.PendingProvider != "" || !e.PaidThrough.After(time.Now()) {
				return nil
			}
		}
		if e.Renewal != want {
			return nil
		}
		if err := call(ctx, tenantID); err != nil {
			return fmt.Errorf("%s renewal at stripe: %w", action, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE entitlements SET renewal = $2, updated_at = NOW() WHERE tenant_id = $1`,
			tenantID, string(to)); err != nil {
			return fmt.Errorf("write renewal of tenant %s: %w", tenantID, err)
		}
		e.Renewal, changed = to, true
		return nil
	})
	return e, changed, err
}

// --- the admin endpoints -------------------------------------------------

type bulkRenewalsRequest struct {
	// DryRun defaults to true: a bulk action that writes is asked for.
	DryRun *bool `json:"dry_run"`
}

func (s *Server) handleBulkStopRenewals(w http.ResponseWriter, r *http.Request) {
	s.bulkRenewals(w, r, bulkStop)
}

func (s *Server) handleBulkResumeRenewals(w http.ResponseWriter, r *http.Request) {
	s.bulkRenewals(w, r, bulkResume)
}

func (s *Server) bulkRenewals(w http.ResponseWriter, r *http.Request, action string) {
	if s.stripeBulk == nil {
		http.Error(w, "payments not configured", http.StatusServiceUnavailable)
		return
	}
	var req bulkRenewalsRequest
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	dry := req.DryRun == nil || *req.DryRun
	actor, _ := r.Context().Value(userIDKey).(string)
	w.Header().Set("Content-Type", "application/json")
	if dry {
		res, err := s.stripeBulk.DryRun(r.Context(), action)
		if err != nil {
			s.logger.Error("bulk renewals dry run", zap.String("action", action), zap.Error(err))
			http.Error(w, "could not list the rows", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "action": action, "dry_run": true, "tenants": res.Tenants})
		return
	}
	j := s.jobs.job(s.stripeBulk.JobName)
	err := j.StartDetachedWith(r.Context(), func(ctx context.Context) (jobReport, error) {
		res, err := s.stripeBulk.Run(ctx, action, actor)
		rep := jobReport{Rows: int64(res.Done), Result: res}
		if len(res.Errors) > 0 {
			rep.Note = fmt.Sprintf("%d tenant(s) not changed or not mailed: %s", len(res.Errors), strings.Join(res.Errors, "; "))
		}
		return rep, err
	}, nil)
	if errors.Is(err, errJobAlreadyRunning) {
		w.Header().Del("Content-Type")
		writeJobAlreadyRunning(w, s.stripeBulk.JobName)
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, EventType: "admin", Action: "admin.stripe_bulk_renewals", Error: err,
		Metadata: map[string]any{"action": action, "started": err == nil}})
	if err != nil {
		s.logger.Error("bulk renewals: could not start", zap.String("action", action), zap.Error(err))
		http.Error(w, "could not start: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"job": s.stripeBulk.JobName, "action": action, "status": "started",
		"status_url": "/api/v1/admin/jobs"})
}
