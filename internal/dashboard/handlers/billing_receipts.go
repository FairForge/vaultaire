package handlers

import (
	"context"
	"database/sql"
	"errors"
	"html/template"
	"net/http"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// Receipts (WP-PAY-3 item 1, docs/PAYMENTS.md § 3.8): the billing page lists
// the tenant's settled rows (number, date, amount, link) and
// /dashboard/billing/receipts/{id} renders one as a print-ready page from
// the ledger row — never stored twice. The tenant is the session's; another
// tenant's id is 404.

// ReceiptRow is one line of the billing page's receipts list.
type ReceiptRow struct {
	ID, InvoiceNumber, Date, Amount, Kind, Status, House string
}

func receiptRows(list []payments.Receipt) []ReceiptRow {
	out := make([]ReceiptRow, 0, len(list))
	for _, r := range list {
		row := ReceiptRow{ID: r.ID, InvoiceNumber: r.InvoiceNumber, Date: r.PaidAt.UTC().Format("2 Jan 2006"),
			Amount: r.Amount(), Kind: r.Kind, Status: r.Status}
		if r.Order != nil {
			row.House = r.Order.Words()
		}
		out = append(out, row)
	}
	return out
}

// populateReceipts fills data["Receipts"] (nil when there are none).
func populateReceipts(ctx context.Context, db *sql.DB, data map[string]any, tenantID string, logger *zap.Logger) {
	if db == nil {
		return
	}
	list, err := payments.ListReceipts(ctx, db, tenantID, 24)
	if err != nil {
		logger.Warn("billing: receipts not listed", zap.String("tenant", tenantID), zap.Error(err))
		return
	}
	if len(list) > 0 {
		data["Receipts"] = receiptRows(list)
	}
}

// receiptLine is one printed line.
type receiptLine struct {
	Label  string
	TB     int
	Money  string
	Detail string
}

// HandleReceipt is GET /dashboard/billing/receipts/{id}: the print-ready
// receipt of one of the tenant's settled rows (404 for anyone else's).
func HandleReceipt(tmpl *template.Template, db *sql.DB, legal hosts.LegalEntity, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if db == nil {
			http.NotFound(w, r)
			return
		}
		rc, err := payments.ReadReceipt(r.Context(), db, sd.TenantID, chi.URLParam(r, "id"))
		if errors.Is(err, payments.ErrNoReceipt) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			logger.Error("receipt: read", zap.String("tenant", sd.TenantID), zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		if tmpl == nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		lines := make([]receiptLine, 0, len(rc.Lines))
		for _, l := range rc.Lines {
			lines = append(lines, receiptLine{Label: l.Label, TB: l.TB, Money: l.Money(), Detail: l.Detail})
		}
		via := providerLabels[rc.Provider]
		if via == "" {
			via = rc.Provider
		}
		if rc.Provider == payments.ProviderManual {
			via = "recorded by Stored support"
		}
		house := ""
		if rc.Order != nil {
			house = rc.Order.Words()
		}
		data := map[string]any{
			"Legal": legal, "InvoiceNumber": rc.InvoiceNumber, "Kind": rc.Kind, "IsRefund": rc.Kind == payments.KindRefund,
			"PaidAt": rc.PaidAt.UTC().Format("2 Jan 2006"), "Email": rc.Email, "Amount": rc.Amount(), "Lines": lines,
			"Period": rc.PeriodWords(), "House": house, "Via": via, "ProviderID": rc.ProviderID,
			"CryptoAmount": rc.CryptoAmount, "CryptoCurrency": rc.CryptoCurrency, "Rate": rc.Rate,
			"IsCrypto": rc.CryptoAmount != "", "Status": rc.Status, "Brand": hosts.BrandName,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := tmpl.ExecuteTemplate(w, "receipt", data); err != nil {
			logger.Error("render receipt", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}
