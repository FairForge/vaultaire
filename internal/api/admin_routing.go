package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// Admin view of the routing truth (WP-R7-5).
//
//	GET  /api/v1/admin/routing-truth               the boot check, the rows on no registered
//	                                                backend (live), the last run of the job
//	POST /api/v1/admin/routing-truth/resolve-null  rows with NO backend: ask every registered
//	                                                driver once, write the one that answers
//	                                                (dry run by default)
//
// The job itself is triggered through POST /api/v1/admin/jobs/routing_truth/run.

// adminRoutingTruthView is GET's answer.
type adminRoutingTruthView struct {
	Job            string               `json:"job"`
	Registered     []string             `json:"registered_backends"`
	Boot           *RoutingBootReport   `json:"boot_check"`
	Unknown        []unknownRows        `json:"unknown_backend_rows"`
	SharedStores   []engine.SharedStore `json:"shared_stores"`
	LastOutcome    string               `json:"last_outcome"`
	LastStartedAt  *time.Time           `json:"last_started_at"`
	LastFinishedAt *time.Time           `json:"last_finished_at"`
	LastSuccessAt  *time.Time           `json:"last_success_at"`
	LastNote       string               `json:"last_note"`
	LastRun        json.RawMessage      `json:"last_run"`
}

// handleAdminRoutingTruth serves GET /api/v1/admin/routing-truth.
func (s *Server) handleAdminRoutingTruth(w http.ResponseWriter, r *http.Request) {
	c := s.routingTruth
	if c == nil {
		http.Error(w, "routing truth not available (no database)", http.StatusServiceUnavailable)
		return
	}
	ctx := r.Context()
	unknown, err := c.unknownBackendRows(ctx)
	if err != nil {
		s.logger.Error("admin routing truth: read tables", zap.Error(err))
		http.Error(w, "could not read the routing tables", http.StatusInternalServerError)
		return
	}
	view := adminRoutingTruthView{
		Job:          c.JobName,
		Registered:   c.eng.GetDriverNames(),
		Boot:         c.bootReport(),
		Unknown:      unknown,
		SharedStores: c.eng.SharedStores(),
	}
	if view.Unknown == nil {
		view.Unknown = []unknownRows{}
	}
	if view.SharedStores == nil {
		view.SharedStores = []engine.SharedStore{}
	}
	rows, err := jobRunRows(ctx, s.db)
	if err != nil {
		s.logger.Error("admin routing truth: read job_runs", zap.Error(err))
		http.Error(w, "could not read job_runs", http.StatusInternalServerError)
		return
	}
	if jr, ok := rows[c.JobName]; ok {
		view.LastOutcome = jr.Outcome
		view.LastStartedAt = timePtr(jr.LastStarted.Time, jr.LastStarted.Valid)
		view.LastFinishedAt = timePtr(jr.LastFinished.Time, jr.LastFinished.Valid)
		view.LastSuccessAt = timePtr(jr.LastSuccess.Time, jr.LastSuccess.Valid)
		view.LastNote = jr.Error
		view.LastRun = jr.Result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(view)
}

// resolveNullResult is POST resolve-null's answer.
type resolveNullResult struct {
	DryRun bool `json:"dry_run"`
	// Rows is how many NULL rows were looked at (up to limit).
	Rows int `json:"rows"`
	// Resolved: exactly one backend holds the bytes (written when not a dry run).
	Resolved int `json:"resolved"`
	// Nowhere: no backend holds the bytes — the object is gone; delete the
	// row through DELETE (quota is released) or the tenant's erasure.
	Nowhere int `json:"nowhere"`
	// Ambiguous: more than one backend holds a blob at that key — left for
	// the operator (the etag decides; see the note).
	Ambiguous int `json:"ambiguous"`
	// Errors: a backend could not be asked for the row — nothing written.
	Errors int `json:"errors"`
	// Remaining NULL rows after this call.
	Remaining int64 `json:"remaining"`
	// ByBackend: resolved rows per backend.
	ByBackend map[string]int `json:"by_backend"`
	// Details: one line per row (bounded by limit).
	Details []resolveNullRow `json:"details"`
}

type resolveNullRow struct {
	Tenant   string   `json:"tenant_id"`
	Bucket   string   `json:"bucket"`
	Key      string   `json:"key"`
	Holders  []string `json:"holders"`
	Verdict  string   `json:"verdict"` // resolved | nowhere | ambiguous | error
	Written  bool     `json:"written"`
	ErrorMsg string   `json:"error,omitempty"`
}

// resolveNullDefaultLimit bounds one call: every row costs one Exists per
// registered backend, synchronously (HAProxy's 50 s).
const resolveNullDefaultLimit = 50

// handleAdminRoutingTruthResolveNull serves POST /api/v1/admin/routing-truth/resolve-null.
//
// Rows with no backend_name (multipart completes before R3-03) are served by
// the engine's fan-out only. This is the ONE place a fan-out is justified:
// each registered driver is asked once, with the row's tenant in the
// context, and the row is written only when exactly one of them holds the
// bytes — under a guard (backend_name IS NULL AND etag unchanged), so a
// concurrent write is never overwritten. Nothing is deleted here.
func (s *Server) handleAdminRoutingTruthResolveNull(w http.ResponseWriter, r *http.Request) {
	c := s.routingTruth
	if c == nil {
		http.Error(w, "routing truth not available (no database)", http.StatusServiceUnavailable)
		return
	}
	dry := true
	if v := r.URL.Query().Get("dry_run"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			http.Error(w, "dry_run must be true or false", http.StatusBadRequest)
			return
		}
		dry = b
	}
	limit := resolveNullDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 500 {
			http.Error(w, "limit must be an integer in 1..500", http.StatusBadRequest)
			return
		}
		limit = n
	}
	ctx, cancel := adminTriggerContext(r)
	defer cancel()
	res, err := c.resolveNull(ctx, dry, limit)
	actor, _ := r.Context().Value(userIDKey).(string)
	audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, EventType: "admin", Action: "admin.routing_truth_resolve_null", Error: err,
		Metadata: map[string]any{"dry_run": dry, "rows": res.Rows, "resolved": res.Resolved, "nowhere": res.Nowhere,
			"ambiguous": res.Ambiguous, "errors": res.Errors, "remaining": res.Remaining}})
	if err != nil {
		s.logger.Error("routing truth: resolve NULL rows failed", zap.Error(err))
		http.Error(w, "resolve failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Info("routing truth: NULL rows resolved", zap.Bool("dry_run", dry), zap.Int("rows", res.Rows),
		zap.Int("resolved", res.Resolved), zap.Int("nowhere", res.Nowhere), zap.Int("ambiguous", res.Ambiguous), zap.Int("errors", res.Errors))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// resolveNull is the work of the handler above.
func (c *RoutingTruthChecker) resolveNull(ctx context.Context, dry bool, limit int) (resolveNullResult, error) {
	res := resolveNullResult{DryRun: dry, ByBackend: map[string]int{}, Details: []resolveNullRow{}}
	rows, err := c.db.QueryContext(ctx, `SELECT tenant_id, bucket, object_key, COALESCE(etag, '') FROM object_head_cache
		WHERE backend_name IS NULL AND NOT is_chunked AND ($1 = '' OR tenant_id = $1)
		ORDER BY tenant_id, bucket, object_key LIMIT $2`, c.scopeTenant, limit)
	if err != nil {
		return res, err
	}
	var refs []headRef
	for rows.Next() {
		var h headRef
		if err := rows.Scan(&h.tenantID, &h.bucket, &h.key, &h.etag); err != nil {
			_ = rows.Close()
			return res, err
		}
		refs = append(refs, h)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return res, err
	}
	_ = rows.Close()

	backends := c.eng.GetDriverNames()
	for _, h := range refs {
		if ctx.Err() != nil {
			break
		}
		res.Rows++
		d := resolveNullRow{Tenant: h.tenantID, Bucket: h.bucket, Key: h.key, Holders: []string{}}
		container := h.tenantID + "_" + h.bucket
		for _, b := range backends {
			if st := c.eng.GetFailoverStatus()[b]; st == engine.StateOpen.String() {
				d.Verdict, d.ErrorMsg = "error", "circuit breaker open on "+b
				break
			}
			cctx, cancel := context.WithTimeout(common.WithTenantID(ctx, h.tenantID), routingExistsTimeout)
			ok, eErr := c.eng.ExistsOn(cctx, b, container, h.key)
			cancel()
			if eErr != nil {
				d.Verdict, d.ErrorMsg = "error", b+": "+eErr.Error()
				break
			}
			if ok {
				d.Holders = append(d.Holders, b)
			}
		}
		switch {
		case d.Verdict == "error":
			res.Errors++
		case len(d.Holders) == 0:
			d.Verdict = "nowhere"
			res.Nowhere++
		case len(d.Holders) > 1:
			d.Verdict = "ambiguous"
			res.Ambiguous++
		default:
			d.Verdict = "resolved"
			res.Resolved++
			res.ByBackend[d.Holders[0]]++
			if !dry {
				n, wErr := c.db.ExecContext(ctx, `UPDATE object_head_cache SET backend_name = $1
					WHERE tenant_id = $2 AND bucket = $3 AND object_key = $4 AND backend_name IS NULL AND COALESCE(etag, '') = $5`,
					d.Holders[0], h.tenantID, h.bucket, h.key, h.etag)
				if wErr != nil {
					d.Verdict, d.ErrorMsg = "error", "write: "+wErr.Error()
					res.Resolved--
					res.ByBackend[d.Holders[0]]--
					res.Errors++
				} else if affected, _ := n.RowsAffected(); affected == 1 {
					d.Written = true
					c.logger.Info("routing truth: NULL head row resolved", zap.String("tenant_id", h.tenantID),
						zap.String("bucket", h.bucket), zap.String("key", h.key), zap.String("backend", d.Holders[0]))
				}
			}
		}
		res.Details = append(res.Details, d)
	}
	var remaining sql.NullInt64
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM object_head_cache WHERE backend_name IS NULL AND ($1 = '' OR tenant_id = $1)`, c.scopeTenant).Scan(&remaining); err == nil {
		res.Remaining = remaining.Int64
	}
	return res, nil
}
