package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// The admin money routes (WP-PAY-3 item 4, docs/PAYMENTS.md § 7), admin
// JWT, every call audited by internal/payments with the actor (the admin's
// user id), the subject tenant and the reason — the reason is required
// (400 without one):
//
//	GET  /api/v1/admin/tenants/{tenant_id}/payments      {entitlement, status, payments}
//	POST /api/v1/admin/tenants/{tenant_id}/payments      a manual payment: ledger row 'manual' settled (numbered) +
//	                                                     Grant (no row — the body names the house) / Extend (a row)
//	POST /api/v1/admin/tenants/{tenant_id}/entitlement   set paid_through and/or the house by hand
//	POST /api/v1/admin/tenants/{tenant_id}/refunds       record a refund made outside; optionally shorten the date

func (s *Server) registerAdminPaymentRoutes(r chi.Router) {
	r.Get("/tenants/{tenant_id}/payments", s.requireAdmin(s.handleAdminTenantPayments))
	r.Post("/tenants/{tenant_id}/payments", s.requireAdmin(s.handleAdminManualPayment))
	r.Post("/tenants/{tenant_id}/entitlement", s.requireAdmin(s.handleAdminAdjustEntitlement))
	r.Post("/tenants/{tenant_id}/refunds", s.requireAdmin(s.handleAdminRefund))
}

// entitlementJSON is the record as the admin API and the dashboard show it.
type entitlementJSON struct {
	TenantID    string     `json:"tenant_id"`
	StdTB       int        `json:"std_tb"`
	VaultTB     int        `json:"vault_tb"`
	PinHotTB    int        `json:"pin_hot_tb"`
	Period      string     `json:"period"`
	PaidThrough time.Time  `json:"paid_through"`
	Renewal     string     `json:"renewal"`
	Provider    string     `json:"provider"`
	ProviderRef string     `json:"provider_ref,omitempty"`
	LapsedAt    *time.Time `json:"lapsed_at,omitempty"`
}

func entitlementView(e payments.Entitlement) entitlementJSON {
	return entitlementJSON{TenantID: e.TenantID, StdTB: e.Order.Std, VaultTB: e.Order.Vault, PinHotTB: e.Order.PinHot,
		Period: string(e.Order.Period), PaidThrough: e.PaidThrough.UTC(), Renewal: string(e.Renewal), Provider: e.Provider,
		ProviderRef: e.ProviderRef, LapsedAt: e.LapsedAt}
}

type ledgerRowJSON struct {
	ID            string     `json:"id"`
	InvoiceNumber string     `json:"invoice_number,omitempty"`
	Kind          string     `json:"kind"`
	Purpose       string     `json:"purpose"`
	Status        string     `json:"status"`
	Provider      string     `json:"provider"`
	ProviderID    string     `json:"provider_id"`
	AmountCents   int64      `json:"amount_cents"`
	PeriodStart   *time.Time `json:"period_start,omitempty"`
	PeriodEnd     *time.Time `json:"period_end,omitempty"`
	SettledAt     *time.Time `json:"settled_at,omitempty"`
	RefundOf      string     `json:"refund_of,omitempty"`
	House         string     `json:"house,omitempty"`
}

func ledgerView(r payments.Receipt) ledgerRowJSON {
	v := ledgerRowJSON{ID: r.ID, InvoiceNumber: r.InvoiceNumber, Kind: r.Kind, Purpose: r.Purpose, Status: r.Status,
		Provider: r.Provider, ProviderID: r.ProviderID, AmountCents: r.AmountCents, RefundOf: r.RefundOf,
		PeriodStart: timePtr(r.PeriodStart, !r.PeriodStart.IsZero()), PeriodEnd: timePtr(r.PeriodEnd, !r.PeriodEnd.IsZero()),
		SettledAt: timePtr(r.PaidAt, !r.PaidAt.IsZero())}
	if r.Order != nil {
		v.House = r.Order.Words()
	}
	return v
}

func (s *Server) handleAdminTenantPayments(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenant_id")
	ctx := r.Context()
	out := map[string]any{"object": "tenant_payments", "tenant_id": tenantID, "entitlement": nil}
	e, err := payments.Get(ctx, s.db, tenantID)
	switch {
	case errors.Is(err, payments.ErrNoEntitlement):
	case err != nil:
		s.logger.Error("admin payments: read entitlement", zap.String("tenant", tenantID), zap.Error(err))
		http.Error(w, "could not read the entitlement", http.StatusInternalServerError)
		return
	default:
		out["entitlement"] = entitlementView(e)
	}
	st, err := payments.StatusOf(ctx, s.db, tenantID, s.payments.Grace(), time.Now())
	if err != nil {
		http.Error(w, "could not read the entitlement", http.StatusInternalServerError)
		return
	}
	out["status"] = map[string]any{"found": st.Found, "granted": st.Granted, "in_grace": st.InGrace, "grace_ends": st.GraceEnds.UTC()}
	rows, err := payments.ListLedger(ctx, s.db, tenantID, 200)
	if err != nil {
		s.logger.Error("admin payments: list ledger", zap.String("tenant", tenantID), zap.Error(err))
		http.Error(w, "could not read the ledger", http.StatusInternalServerError)
		return
	}
	list := make([]ledgerRowJSON, 0, len(rows))
	for _, x := range rows {
		list = append(list, ledgerView(x))
	}
	out["payments"] = list
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

type orderJSON struct {
	StdTB    int    `json:"std_tb"`
	VaultTB  int    `json:"vault_tb"`
	PinHotTB int    `json:"pin_hot_tb"`
	Period   string `json:"period,omitempty"`
}

func (o *orderJSON) order() *payments.Order {
	if o == nil {
		return nil
	}
	return &payments.Order{Std: o.StdTB, Vault: o.VaultTB, PinHot: o.PinHotTB, Period: payments.Period(o.Period)}
}

// adminMoneyError maps the payments package's refusals onto statuses.
func adminMoneyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, payments.ErrReasonRequired):
		http.Error(w, "reason is required", http.StatusBadRequest)
	case errors.Is(err, payments.ErrOrderRequired), errors.Is(err, payments.ErrInvalidOrder), errors.Is(err, payments.ErrRefundNoPeriod):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, payments.ErrNoEntitlement), errors.Is(err, payments.ErrNoSuchPayment):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, payments.ErrRefundExceeds):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func (s *Server) handleAdminManualPayment(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenant_id")
	actor, _ := r.Context().Value(userIDKey).(string)
	var req struct {
		AmountCents int64      `json:"amount_cents"`
		Period      string     `json:"period"`
		Purpose     string     `json:"purpose"`
		Note        string     `json:"note"`
		Reason      string     `json:"reason"`
		Order       *orderJSON `json:"order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.Purpose != "" && req.Purpose != payments.PurposeManual {
		http.Error(w, `purpose must be "manual"`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		http.Error(w, "reason is required", http.StatusBadRequest)
		return
	}
	res, err := payments.RecordManualPayment(r.Context(), s.db, payments.ManualPayment{
		TenantID: tenantID, AmountCents: req.AmountCents, Period: payments.Period(req.Period), Order: req.Order.order(),
		Note: req.Note, Actor: actor, Reason: req.Reason})
	if err != nil {
		if !errors.Is(err, payments.ErrOrderRequired) && !errors.Is(err, payments.ErrInvalidOrder) && !errors.Is(err, payments.ErrReasonRequired) {
			s.logger.Error("admin manual payment", zap.String("tenant", tenantID), zap.Error(err))
		}
		adminMoneyError(w, err)
		return
	}
	s.logger.Info("admin manual payment recorded", zap.String("tenant", tenantID), zap.String("actor", actor),
		zap.String("invoice_number", res.InvoiceNumber), zap.Int64("amount_cents", req.AmountCents), zap.Bool("granted", res.Granted))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"payment_id": res.PaymentID, "invoice_number": res.InvoiceNumber,
		"paid_through": res.PaidThrough.UTC(), "granted": res.Granted})
}

// handleAdminAdjustEntitlement is THE ONE WRITER THAT MAY SHORTEN A DATE:
// every grant takes the later of two dates and a refund shortens only
// through RecordRefund; here the operator sets paid_through as given, with
// a reason, audited billing.paid_through_adjusted.
func (s *Server) handleAdminAdjustEntitlement(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenant_id")
	actor, _ := r.Context().Value(userIDKey).(string)
	var req struct {
		PaidThrough *time.Time `json:"paid_through"`
		Order       *orderJSON `json:"order"`
		Reason      string     `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body (paid_through is RFC 3339)", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		http.Error(w, "reason is required", http.StatusBadRequest)
		return
	}
	e, err := payments.AdjustEntitlement(r.Context(), s.db, payments.Adjustment{
		TenantID: tenantID, PaidThrough: req.PaidThrough, Order: req.Order.order(), Actor: actor, Reason: req.Reason})
	if err != nil {
		adminMoneyError(w, err)
		return
	}
	s.logger.Warn("admin entitlement adjusted", zap.String("tenant", tenantID), zap.String("actor", actor),
		zap.Time("paid_through", e.PaidThrough), zap.String("reason", req.Reason))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"entitlement": entitlementView(e)})
}

func (s *Server) handleAdminRefund(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenant_id")
	actor, _ := r.Context().Value(userIDKey).(string)
	var req struct {
		PaymentID          string `json:"payment_id"`
		AmountCents        int64  `json:"amount_cents"`
		ProviderRef        string `json:"provider_ref"`
		ShortenPaidThrough bool   `json:"shorten_paid_through"`
		Note               string `json:"note"`
		Reason             string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		http.Error(w, "reason is required", http.StatusBadRequest)
		return
	}
	res, err := payments.RecordRefund(r.Context(), s.db, payments.Refund{
		TenantID: tenantID, PaymentID: req.PaymentID, AmountCents: req.AmountCents, ProviderRef: req.ProviderRef,
		ShortenPaidThrough: req.ShortenPaidThrough, Note: req.Note, Actor: actor, Reason: req.Reason})
	if err != nil {
		adminMoneyError(w, err)
		return
	}
	s.logger.Warn("admin refund recorded", zap.String("tenant", tenantID), zap.String("actor", actor),
		zap.String("refund_of", req.PaymentID), zap.Int64("amount_cents", req.AmountCents), zap.Duration("shortened_by", res.ShortenedBy))
	out := map[string]any{"payment_id": res.PaymentID, "invoice_number": res.InvoiceNumber, "fully_refunded": res.FullyRefunded,
		"shortened_by": res.ShortenedBy.String()}
	if req.ShortenPaidThrough {
		out["paid_through"] = res.PaidThrough.UTC()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(out)
}
