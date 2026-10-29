package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// The admin tenant page's Billing panel (WP-PAY-3 item 4): three CSRF'd
// htmx forms — a manual payment, a paid_through / house adjustment, a
// refund — calling the same internal/payments operations as the admin API
// (internal/api/admin_payments.go), audited with the admin's user id as
// actor and the reason the form requires. Each answers an htmx fragment.

// adminFragment writes one alert fragment.
func adminFragment(w http.ResponseWriter, status int, class, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<div class="alert alert-%s">%s</div>`, class, template.HTMLEscapeString(msg))
}

// dollarsToCents parses "53.88" → 5388.
func dollarsToCents(s string) (int64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(s, "$")), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, fmt.Errorf("amount %q is not a dollar amount", s)
	}
	return int64(math.Round(v * 100)), nil
}

// formOrder reads std_tb / vault_tb / pin_hot_tb; nil when none is given.
func formOrder(r *http.Request, period payments.Period) (*payments.Order, bool) {
	std, vault, pin := formInt(r, "std_tb"), formInt(r, "vault_tb"), formInt(r, "pin_hot_tb")
	if std == 0 && vault == 0 && pin == 0 {
		return nil, true
	}
	if std < 0 || vault < 0 || pin < 0 {
		return nil, false
	}
	return &payments.Order{Std: std, Vault: vault, PinHot: pin, Period: period}, true
}

func adminMoneyStatus(err error) int {
	switch {
	case errors.Is(err, payments.ErrNoEntitlement), errors.Is(err, payments.ErrNoSuchPayment):
		return http.StatusNotFound
	case errors.Is(err, payments.ErrRefundExceeds), errors.Is(err, payments.ErrRefundShortened), errors.Is(err, payments.ErrRefundMismatch):
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

func adminSession(w http.ResponseWriter, r *http.Request, db *sql.DB) (*dashauth.SessionData, string, bool) {
	sd := dashauth.GetSession(r.Context())
	if sd == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return nil, "", false
	}
	tenantID := chi.URLParam(r, "id")
	if tenantID == "" || db == nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return nil, "", false
	}
	if strings.TrimSpace(r.FormValue("reason")) == "" {
		adminFragment(w, http.StatusBadRequest, "error", "A reason is required.")
		return nil, "", false
	}
	return sd, tenantID, true
}

// HandleAdminManualPayment is POST /admin/tenants/{id}/billing/payment.
func HandleAdminManualPayment(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd, tenantID, ok := adminSession(w, r, db)
		if !ok {
			return
		}
		cents, err := dollarsToCents(r.FormValue("amount"))
		if err != nil {
			adminFragment(w, http.StatusBadRequest, "error", err.Error())
			return
		}
		period := payments.Period(strings.TrimSpace(r.FormValue("period")))
		order, ok := formOrder(r, period)
		if !ok {
			adminFragment(w, http.StatusBadRequest, "error", "A floor cannot be negative.")
			return
		}
		res, err := payments.RecordManualPayment(r.Context(), db, payments.ManualPayment{
			TenantID: tenantID, AmountCents: cents, Period: period, Order: order,
			Note: strings.TrimSpace(r.FormValue("note")), Actor: sd.UserID, Reason: strings.TrimSpace(r.FormValue("reason"))})
		if err != nil {
			logger.Warn("admin manual payment refused", zap.String("tenant", tenantID), zap.Error(err))
			adminFragment(w, adminMoneyStatus(err), "error", err.Error())
			return
		}
		logger.Info("admin manual payment recorded", zap.String("tenant", tenantID), zap.String("by", sd.Email),
			zap.String("invoice_number", res.InvoiceNumber), zap.Int64("amount_cents", cents))
		what := "extended"
		if res.Granted {
			what = "granted"
		}
		adminFragment(w, http.StatusOK, "success", fmt.Sprintf("Recorded %s (%s): the plan is %s, paid through %s. Reload to see the ledger.",
			res.InvoiceNumber, moneyFromCents(cents), what, res.PaidThrough.UTC().Format("2 Jan 2006")))
	}
}

func moneyFromCents(c int64) string { return fmt.Sprintf("$%d.%02d", c/100, c%100) }

// HandleAdminAdjustEntitlement is POST /admin/tenants/{id}/billing/entitlement
// — the dashboard face of THE ONE WRITER THAT MAY SHORTEN A DATE.
func HandleAdminAdjustEntitlement(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd, tenantID, ok := adminSession(w, r, db)
		if !ok {
			return
		}
		a := payments.Adjustment{TenantID: tenantID, Actor: sd.UserID, Reason: strings.TrimSpace(r.FormValue("reason"))}
		if v := strings.TrimSpace(r.FormValue("paid_through")); v != "" {
			t, err := time.Parse("2006-01-02", v)
			if err != nil {
				adminFragment(w, http.StatusBadRequest, "error", "paid_through must be a date (YYYY-MM-DD).")
				return
			}
			t = t.UTC()
			a.PaidThrough = &t
		}
		order, ok := formOrder(r, payments.Period(strings.TrimSpace(r.FormValue("period"))))
		if !ok {
			adminFragment(w, http.StatusBadRequest, "error", "A floor cannot be negative.")
			return
		}
		a.Order = order
		e, err := payments.AdjustEntitlement(r.Context(), db, a)
		if err != nil {
			logger.Warn("admin entitlement adjustment refused", zap.String("tenant", tenantID), zap.Error(err))
			adminFragment(w, adminMoneyStatus(err), "error", err.Error())
			return
		}
		logger.Warn("admin entitlement adjusted", zap.String("tenant", tenantID), zap.String("by", sd.Email),
			zap.Time("paid_through", e.PaidThrough), zap.String("reason", a.Reason))
		adminFragment(w, http.StatusOK, "success", fmt.Sprintf("Adjusted: %s, paid through %s. Reload to see the ledger.",
			e.Order.Words(), e.PaidThrough.UTC().Format("2 Jan 2006")))
	}
}

// HandleAdminRefund is POST /admin/tenants/{id}/billing/refund.
func HandleAdminRefund(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd, tenantID, ok := adminSession(w, r, db)
		if !ok {
			return
		}
		// The amount may be left blank when the form names a refund already
		// in the ledger (to shorten from it — RP28-1).
		var cents int64
		if v := strings.TrimSpace(r.FormValue("amount")); v != "" {
			var err error
			if cents, err = dollarsToCents(v); err != nil {
				adminFragment(w, http.StatusBadRequest, "error", err.Error())
				return
			}
		}
		res, err := payments.RecordRefund(r.Context(), db, payments.Refund{
			TenantID: tenantID, PaymentID: strings.TrimSpace(r.FormValue("payment_id")), AmountCents: cents,
			ProviderRef: strings.TrimSpace(r.FormValue("provider_ref")), ShortenPaidThrough: r.FormValue("shorten") != "",
			Note: strings.TrimSpace(r.FormValue("note")), Actor: sd.UserID, Reason: strings.TrimSpace(r.FormValue("reason"))})
		if err != nil {
			logger.Warn("admin refund refused", zap.String("tenant", tenantID), zap.Error(err))
			adminFragment(w, adminMoneyStatus(err), "error", err.Error())
			return
		}
		logger.Warn("admin refund recorded", zap.String("tenant", tenantID), zap.String("by", sd.Email),
			zap.Int64("amount_cents", cents), zap.Bool("existing", res.Existing), zap.Duration("shortened_by", res.ShortenedBy))
		msg := fmt.Sprintf("Recorded refund %s of %s.", res.InvoiceNumber, moneyFromCents(cents))
		if res.Existing {
			msg = fmt.Sprintf("Refund %s is already in the ledger (recorded from the provider); nothing new recorded.", res.InvoiceNumber)
		}
		if res.FullyRefunded {
			msg += " The charge is now fully refunded."
		}
		if res.ShortenedBy > 0 {
			msg += fmt.Sprintf(" Paid through moved back %d days to %s.", int(res.ShortenedBy.Hours()/24), res.PaidThrough.UTC().Format("2 Jan 2006"))
		}
		adminFragment(w, http.StatusOK, "success", msg+" Reload to see the ledger.")
	}
}
