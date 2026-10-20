package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/FairForge/vaultaire/internal/api/landing"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/payments"
	"go.uber.org/zap"
)

// HandleBilling renders the billing page with plan, subscription status,
// value stack, and cost comparison — and, behind the quota_checkout flag,
// the whole-TB house checkout, the one product (WP-PAY-0: no legacy pack
// grid, no metered estimate). svc may be a nil
// interface (billing not configured); fl may be nil (no flags = off).
func HandleBilling(tmpl *template.Template, svc BillingService, db *sql.DB, fl *flags.Service, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "billing")
		withCSRF(r.Context(), data)
		withFlash(r.Context(), data)
		ctx := r.Context()

		populateBillingPlan(ctx, db, data, sd.TenantID)
		populateHouseIntent(ctx, db, data, sd.TenantID)
		populateValueStack(ctx, db, data, sd.TenantID)
		populateCostComparison(ctx, db, data, sd.TenantID)
		var houseSvc HouseBilling
		if svc != nil {
			houseSvc = svc
		}
		populateHouse(ctx, db, houseSvc, fl, data, sd.TenantID, r.URL.Query().Get("add"))

		if r.URL.Query().Get("upgraded") == "1" {
			data["Upgraded"] = true
		}
		if r.URL.Query().Get("resized") == "1" {
			data["Resized"] = true
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render billing page", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// HandleManageBilling redirects to the provider's self-service page (the
// Stripe Billing Portal). It takes the provider, never a provider SDK type
// (WP-PAY-2); managing a payment is never flag-gated. baseURL makes the
// return URL absolute — Stripe rejects a relative one (Review R10-06).
func HandleManageBilling(p payments.Provider, baseURL string, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		if p == nil {
			http.Error(w, "Billing not configured", http.StatusServiceUnavailable)
			return
		}

		portalURL, err := p.ManageURL(
			r.Context(), sd.TenantID, strings.TrimRight(baseURL, "/")+"/dashboard/billing",
		)
		if errors.Is(err, payments.ErrProviderUnconfigured) {
			http.Error(w, "Billing not configured", http.StatusServiceUnavailable)
			return
		}
		if err != nil {
			logger.Error("create billing portal session", zap.Error(err))
			http.Error(w, "Failed to open billing portal. Please try again.", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, portalURL, http.StatusSeeOther) // #nosec G710 -- URL from the payment provider
	}
}

func populateBillingPlan(ctx context.Context, db *sql.DB, data map[string]any, tenantID string) {
	data["Plan"] = "starter"
	data["SubscriptionStatus"] = "none"
	data["HasSubscription"] = false
	data["StatusClass"] = "default"
	data["StatusLabel"] = "Free"

	if db == nil {
		return
	}

	var plan, status sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT plan, subscription_status FROM tenants WHERE id = $1`, tenantID).
		Scan(&plan, &status)
	if err != nil {
		return
	}

	if plan.Valid && plan.String != "" {
		data["Plan"] = plan.String
	}
	// A tenant still labelled with a legacy pack plan (an admin tier
	// label, or a pack bought before WP-PAY-0) is told to ask support:
	// one product, and never a second subscription beside a first.
	if legacyPlan(plan.String) {
		data["LegacyPlan"] = plan.String
	}
	if status.Valid && status.String != "" {
		data["SubscriptionStatus"] = status.String
		data["HasSubscription"] = status.String == "active" || status.String == "past_due"
	}

	// Status display helpers.
	st := "none"
	if status.Valid {
		st = status.String
	}
	switch st {
	case "active":
		data["StatusClass"] = "success"
		data["StatusLabel"] = "Active"
	case "past_due":
		data["StatusClass"] = "warning"
		data["StatusLabel"] = "Past Due"
	case "canceled":
		data["StatusClass"] = "danger"
		data["StatusLabel"] = "Canceled"
	default:
		data["StatusClass"] = "default"
		data["StatusLabel"] = "Free"
	}
}

// populateHouseIntent surfaces the house the customer built on the landing
// page (stored at signup) as the proposed plan, until they hold a subscription.
func populateHouseIntent(ctx context.Context, db *sql.DB, data map[string]any, tenantID string) {
	if db == nil {
		return
	}
	if has, _ := data["HasSubscription"].(bool); has {
		return
	}
	var std, vault int
	var room string
	if err := db.QueryRowContext(ctx,
		`SELECT intent_std_tb, intent_vault_tb, intent_room FROM tenants WHERE id = $1`, tenantID).
		Scan(&std, &vault, &room); err != nil {
		return
	}
	h := landing.HouseIntent{StdTB: std, VaultTB: vault, Room: room}
	if !h.Empty() {
		data["HouseIntent"] = h
	}
}

func populateValueStack(ctx context.Context, db *sql.DB, data map[string]any, tenantID string) {
	data["StorageUsedFmt"] = "0 B"

	if db == nil {
		return
	}

	var used int64
	err := db.QueryRowContext(ctx,
		`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, tenantID).
		Scan(&used)
	if err != nil {
		return
	}
	data["StorageUsedFmt"] = formatBytes(used)
}

// Pricing — Stored Standard quota rate (annual, 2026-09-21 quota-sold
// decision) vs competitor list prices as published August 2026 (Wasabi raised
// to $7.99 Jul 1, B2 to $6.95 May 1). Egress on Stored is allowance-based
// (rate-limited past the allowance — internal/usage/egress.go, WP-R10-9),
// never billed.
const (
	storedStoragePerTB = 4.49
	storedEgressPerTB  = 0.0
	awsStoragePerTB    = 23.0
	awsEgressPerTB     = 90.0
	b2StoragePerTB     = 6.95
	b2EgressPerTB      = 10.0
	wasabiStoragePerTB = 7.99
	wasabiEgressPerTB  = 0.0
)

type ProviderCost struct {
	Name        string
	StorageCost string
	EgressCost  string
	TotalCost   string
	Highlight   bool
}

func providerCost(name string, storageTB, egressTB, storageRate, egressRate float64, highlight bool) ProviderCost {
	sc := storageTB * storageRate
	ec := egressTB * egressRate
	return ProviderCost{
		Name:        name,
		StorageCost: fmt.Sprintf("$%.2f", sc),
		EgressCost:  fmt.Sprintf("$%.2f", ec),
		TotalCost:   fmt.Sprintf("$%.2f", sc+ec),
		Highlight:   highlight,
	}
}

func populateCostComparison(ctx context.Context, db *sql.DB, data map[string]any, tenantID string) {
	zero := func() {
		data["Providers"] = []ProviderCost{
			{Name: "Stored", StorageCost: "$0.00", EgressCost: "$0.00", TotalCost: "$0.00", Highlight: true},
			{Name: "AWS S3", StorageCost: "$0.00", EgressCost: "$0.00", TotalCost: "$0.00"},
			{Name: "Backblaze B2", StorageCost: "$0.00", EgressCost: "$0.00", TotalCost: "$0.00"},
			{Name: "Wasabi", StorageCost: "$0.00", EgressCost: "$0.00", TotalCost: "$0.00"},
		}
		data["TotalSavingsVsAWS"] = "$0.00"
		data["EgressThisMonth"] = "0 B"
	}

	if db == nil {
		zero()
		return
	}

	var usedBytes int64
	err := db.QueryRowContext(ctx,
		`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, tenantID).
		Scan(&usedBytes)
	if err != nil {
		zero()
		return
	}

	var egressBytes int64
	err = db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(egress_bytes), 0) FROM bandwidth_usage_daily
		 WHERE tenant_id = $1 AND date >= date_trunc('month', NOW() AT TIME ZONE 'UTC')::date`,
		tenantID).Scan(&egressBytes)
	if err != nil {
		zero()
		return
	}

	if usedBytes == 0 && egressBytes == 0 {
		zero()
		return
	}

	tbStored := float64(usedBytes) / (1024 * 1024 * 1024 * 1024)
	tbEgress := float64(egressBytes) / (1024 * 1024 * 1024 * 1024)

	stored := providerCost("Stored", tbStored, tbEgress, storedStoragePerTB, storedEgressPerTB, true)
	aws := providerCost("AWS S3", tbStored, tbEgress, awsStoragePerTB, awsEgressPerTB, false)
	b2 := providerCost("Backblaze B2", tbStored, tbEgress, b2StoragePerTB, b2EgressPerTB, false)
	wasabi := providerCost("Wasabi", tbStored, tbEgress, wasabiStoragePerTB, wasabiEgressPerTB, false)

	awsTotal := tbStored*awsStoragePerTB + tbEgress*awsEgressPerTB
	storedTotal := tbStored*storedStoragePerTB + tbEgress*storedEgressPerTB

	data["Providers"] = []ProviderCost{stored, aws, b2, wasabi}
	data["TotalSavingsVsAWS"] = fmt.Sprintf("$%.2f", awsTotal-storedTotal)
	data["EgressThisMonth"] = formatBytes(egressBytes)
}
