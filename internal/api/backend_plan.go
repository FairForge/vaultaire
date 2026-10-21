package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// The vendor plan record (WP-CAP-1 A, docs/IMPLEMENTATION_PLAN.md § WP-CAP):
// one backend_plans row per registered backend — what we bought, what it
// holds before overage, when it renews, whether a card is on file. Written
// by the operator through PUT /api/v1/admin/backends/{backend}/plan; read by
// the backend_capacity job, the capacity gauges and the System page. The
// original iDrive account died in 2026-09 because no system of ours knew a
// renewal date existed.

// BackendPlan is one row, as the API reads and writes it.
type BackendPlan struct {
	Backend            string    `json:"backend"`
	Vendor             string    `json:"vendor"`
	PlanName           string    `json:"plan_name"`
	CapacityBytes      int64     `json:"capacity_bytes"`
	EgressFreeMultiple *float64  `json:"egress_free_multiple"` // nil = unmetered
	OverageUSDPerTBMo  *float64  `json:"overage_usd_per_tb_month"`
	EgressUSDPerGB     *float64  `json:"egress_usd_per_gb"`
	TermStart          time.Time `json:"term_start"`
	TermEnd            time.Time `json:"term_end"`
	AutoRenew          bool      `json:"auto_renew"`
	PaymentOnFile      bool      `json:"payment_on_file"`
	InvoiceRef         string    `json:"invoice_ref"`
	Note               string    `json:"note"`
	UpdatedAt          time.Time `json:"updated_at"`
	UpdatedBy          string    `json:"updated_by"`
}

// errNoBackendPlan: the backend has no plan row yet.
var errNoBackendPlan = errors.New("no plan row for this backend")

// readBackendPlan reads one row.
func readBackendPlan(ctx context.Context, db *sql.DB, backend string) (BackendPlan, error) {
	var p BackendPlan
	var mult, overage, egress sql.NullFloat64
	err := db.QueryRowContext(ctx, `
		SELECT backend, vendor, plan_name, capacity_bytes, egress_free_multiple,
		       overage_usd_per_tb_month, egress_usd_per_gb, term_start, term_end,
		       auto_renew, payment_on_file, invoice_ref, note, updated_at, updated_by
		  FROM backend_plans WHERE backend = $1`, backend).
		Scan(&p.Backend, &p.Vendor, &p.PlanName, &p.CapacityBytes, &mult, &overage, &egress,
			&p.TermStart, &p.TermEnd, &p.AutoRenew, &p.PaymentOnFile, &p.InvoiceRef, &p.Note, &p.UpdatedAt, &p.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return p, errNoBackendPlan
	}
	if err != nil {
		return p, fmt.Errorf("read backend plan %s: %w", backend, err)
	}
	if mult.Valid {
		p.EgressFreeMultiple = &mult.Float64
	}
	if overage.Valid {
		p.OverageUSDPerTBMo = &overage.Float64
	}
	if egress.Valid {
		p.EgressUSDPerGB = &egress.Float64
	}
	return p, nil
}

// upsertBackendPlan writes the row (one per backend).
func upsertBackendPlan(ctx context.Context, db *sql.DB, p BackendPlan) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO backend_plans (backend, vendor, plan_name, capacity_bytes, egress_free_multiple,
		                           overage_usd_per_tb_month, egress_usd_per_gb, term_start, term_end,
		                           auto_renew, payment_on_file, invoice_ref, note, updated_at, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, NOW(), $14)
		ON CONFLICT (backend) DO UPDATE SET
		    vendor = EXCLUDED.vendor, plan_name = EXCLUDED.plan_name,
		    capacity_bytes = EXCLUDED.capacity_bytes, egress_free_multiple = EXCLUDED.egress_free_multiple,
		    overage_usd_per_tb_month = EXCLUDED.overage_usd_per_tb_month,
		    egress_usd_per_gb = EXCLUDED.egress_usd_per_gb,
		    term_start = EXCLUDED.term_start, term_end = EXCLUDED.term_end,
		    auto_renew = EXCLUDED.auto_renew, payment_on_file = EXCLUDED.payment_on_file,
		    invoice_ref = EXCLUDED.invoice_ref, note = EXCLUDED.note,
		    updated_at = NOW(), updated_by = EXCLUDED.updated_by`,
		p.Backend, p.Vendor, p.PlanName, p.CapacityBytes, nullFloat(p.EgressFreeMultiple),
		nullFloat(p.OverageUSDPerTBMo), nullFloat(p.EgressUSDPerGB), p.TermStart.UTC(), p.TermEnd.UTC(),
		p.AutoRenew, p.PaymentOnFile, p.InvoiceRef, p.Note, p.UpdatedBy)
	if err != nil {
		return fmt.Errorf("write backend plan %s: %w", p.Backend, err)
	}
	return nil
}

func nullFloat(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *p, Valid: true}
}

// listBackendPlans reads every row (the System page, the plan collector).
func listBackendPlans(ctx context.Context, db *sql.DB) ([]BackendPlan, error) {
	rows, err := db.QueryContext(ctx, `SELECT backend FROM backend_plans ORDER BY backend`)
	if err != nil {
		return nil, fmt.Errorf("list backend plans: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("list backend plans: scan: %w", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list backend plans: %w", err)
	}
	out := make([]BackendPlan, 0, len(names))
	for _, n := range names {
		p, err := readBackendPlan(ctx, db, n)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// backendRegistered reports whether the engine has a driver of that name.
func (s *Server) backendRegistered(name string) bool {
	if s.engine == nil {
		return false
	}
	for _, n := range s.engine.GetDriverNames() {
		if n == name {
			return true
		}
	}
	return false
}

// registerAdminBackendPlanRoutes mounts the plan record under /api/v1/admin.
func (s *Server) registerAdminBackendPlanRoutes(r chi.Router) {
	r.Get("/backends/{backend}/plan", s.requireAdmin(s.handleGetBackendPlan))
	r.Put("/backends/{backend}/plan", s.requireAdmin(s.handlePutBackendPlan))
}

// handleGetBackendPlan answers the row: 404 for an unregistered backend or
// a registered one with no row yet (the body says which).
func (s *Server) handleGetBackendPlan(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		http.Error(w, "no database", http.StatusServiceUnavailable)
		return
	}
	backend := chi.URLParam(r, "backend")
	if !s.backendRegistered(backend) {
		http.Error(w, fmt.Sprintf("backend %q is not registered", backend), http.StatusNotFound)
		return
	}
	p, err := readBackendPlan(r.Context(), s.db, backend)
	if errors.Is(err, errNoBackendPlan) {
		http.Error(w, fmt.Sprintf("backend %q has no plan row yet — PUT one", backend), http.StatusNotFound)
		return
	}
	if err != nil {
		s.logger.Error("backend plan: read failed", zap.String("backend", backend), zap.Error(err))
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p)
}

// handlePutBackendPlan validates and writes the row, audited.
func (s *Server) handlePutBackendPlan(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		http.Error(w, "no database", http.StatusServiceUnavailable)
		return
	}
	backend := chi.URLParam(r, "backend")
	if !s.backendRegistered(backend) {
		http.Error(w, fmt.Sprintf("backend %q is not registered", backend), http.StatusNotFound)
		return
	}
	var p BackendPlan
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&p); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	p.Backend = backend
	switch {
	case p.CapacityBytes <= 0:
		http.Error(w, "capacity_bytes must be > 0", http.StatusBadRequest)
		return
	case p.TermStart.IsZero() || p.TermEnd.IsZero() || !p.TermEnd.After(p.TermStart):
		http.Error(w, "term_end must be after term_start (both RFC 3339)", http.StatusBadRequest)
		return
	case p.EgressFreeMultiple != nil && *p.EgressFreeMultiple < 0:
		http.Error(w, "egress_free_multiple must be >= 0 (null = unmetered)", http.StatusBadRequest)
		return
	}
	actor, _ := r.Context().Value(userIDKey).(string)
	p.UpdatedBy = actor
	if err := upsertBackendPlan(r.Context(), s.db, p); err != nil {
		s.logger.Error("backend plan: write failed", zap.String("backend", backend), zap.Error(err))
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, EventType: "admin", Action: "backend.plan_set",
		Resource: "backend:" + backend, Metadata: map[string]any{
			"backend": backend, "vendor": p.Vendor, "plan_name": p.PlanName, "capacity_bytes": p.CapacityBytes,
			"term_start": p.TermStart.UTC().Format(time.RFC3339), "term_end": p.TermEnd.UTC().Format(time.RFC3339),
			"auto_renew": p.AutoRenew, "payment_on_file": p.PaymentOnFile, "invoice_ref": p.InvoiceRef}})
	s.logger.Info("backend plan set", zap.String("backend", backend), zap.Int64("capacity_bytes", p.CapacityBytes),
		zap.Time("term_end", p.TermEnd), zap.Bool("payment_on_file", p.PaymentOnFile))
	stored, err := readBackendPlan(r.Context(), s.db, backend)
	if err != nil {
		http.Error(w, "written, read-back failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stored)
}
