package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/FairForge/vaultaire/internal/billing"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/flags"
	"go.uber.org/zap"
)

// HandleBilling renders the billing page with plan, subscription status,
// invoice history, value stack, and cost comparison — and, behind the
// quota_checkout flag, the whole-TB house checkout. svc may be a nil
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
		populateAccruedCharges(ctx, db, data, sd.TenantID)
		populateBillingPlans(svc, data)
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

// HandleUpgrade redirects to a Stripe Checkout session for the chosen
// (legacy pack) plan. baseURL makes the return URLs absolute — Stripe
// rejects relative ones.
func HandleUpgrade(stripe *billing.StripeService, db *sql.DB, baseURL string, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		planID := r.FormValue("plan")
		if planID == "" {
			http.Error(w, "Missing plan", http.StatusBadRequest)
			return
		}

		if stripe == nil {
			http.Error(w, "Billing not configured", http.StatusServiceUnavailable)
			return
		}

		// A tenant with a house (or any subscription Stripe still bills)
		// must not open a second subscription through the legacy page
		// (Review R10-05).
		if db != nil {
			if h, err := loadHouse(r.Context(), db, sd.TenantID); err == nil && (h.active || h.liveOther) {
				http.Error(w, "This account already has a subscription. Resize your house on the billing page or email support@stored.ge.", http.StatusConflict)
				return
			}
		}

		customerID, err := stripe.GetCustomerID(r.Context(), sd.TenantID)
		if err != nil {
			logger.Error("get customer id for upgrade", zap.Error(err))
			http.Error(w, "Billing account not found. Please contact support.", http.StatusBadRequest)
			return
		}

		base := strings.TrimRight(baseURL, "/")
		checkoutURL, err := stripe.CreateCheckoutSession(
			customerID, planID,
			base+"/dashboard/billing?upgraded=1",
			base+"/dashboard/billing",
		)
		if err != nil {
			logger.Error("create checkout session", zap.Error(err))
			http.Error(w, "Failed to start checkout. Please try again.", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, checkoutURL, http.StatusSeeOther) // #nosec G710 -- URL from Stripe API
	}
}

// HandleManageBilling redirects to the Stripe Billing Portal. baseURL makes
// the return URL absolute — Stripe rejects a relative one (Review R10-06).
func HandleManageBilling(stripe *billing.StripeService, baseURL string, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		if stripe == nil {
			http.Error(w, "Billing not configured", http.StatusServiceUnavailable)
			return
		}

		portalURL, err := stripe.CreateBillingPortalSession(
			r.Context(), sd.TenantID, strings.TrimRight(baseURL, "/")+"/dashboard/billing",
		)
		if err != nil {
			logger.Error("create billing portal session", zap.Error(err))
			http.Error(w, "Failed to open billing portal. Please try again.", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, portalURL, http.StatusSeeOther)
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

// populateAccruedCharges shows the month-to-date metered charge for Standard /
// Performance tenants. Fixed-price Vault packs and the free tier show nothing.
// Storage is the live gauge; egress is the current month's sum — the same live
// tables the rest of the page reads, so the estimate tracks current usage.
func populateAccruedCharges(ctx context.Context, db *sql.DB, data map[string]any, tenantID string) {
	data["IsMetered"] = false
	if db == nil {
		return
	}

	var tier string
	var storageBytes int64
	var hasHouse bool
	if err := db.QueryRowContext(ctx,
		`SELECT tier, storage_used_bytes,
		        EXISTS (SELECT 1 FROM tenant_floor_quotas f WHERE f.tenant_id = tenant_quotas.tenant_id)
		   FROM tenant_quotas WHERE tenant_id = $1`, tenantID).
		Scan(&tier, &storageBytes, &hasHouse); err != nil {
		return
	}
	if hasHouse || (tier != "standard" && tier != "performance") {
		return // quota-sold house or fixed-price pack — never metered
	}

	var egressBytes int64
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(egress_bytes), 0) FROM bandwidth_usage_daily
		 WHERE tenant_id = $1 AND date >= date_trunc('month', NOW() AT TIME ZONE 'UTC')::date`,
		tenantID).Scan(&egressBytes)

	cents := billing.AccruedCents(tier, storageBytes, egressBytes)
	data["IsMetered"] = true
	data["MeteredTier"] = tier
	data["AccruedCharges"] = fmt.Sprintf("$%.2f", float64(cents)/100)
}

func populateBillingPlans(svc BillingService, data map[string]any) {
	if svc == nil {
		data["AvailablePlans"] = nil
		return
	}
	data["AvailablePlans"] = svc.Plans()
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

// Pricing — stored.ge Standard quota rate (annual, 2026-09-21 quota-sold
// decision) vs competitor list prices as published August 2026 (Wasabi raised
// to $7.99 Jul 1, B2 to $6.95 May 1). Egress on stored.ge is allowance-based
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
			{Name: "stored.ge", StorageCost: "$0.00", EgressCost: "$0.00", TotalCost: "$0.00", Highlight: true},
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

	stored := providerCost("stored.ge", tbStored, tbEgress, storedStoragePerTB, storedEgressPerTB, true)
	aws := providerCost("AWS S3", tbStored, tbEgress, awsStoragePerTB, awsEgressPerTB, false)
	b2 := providerCost("Backblaze B2", tbStored, tbEgress, b2StoragePerTB, b2EgressPerTB, false)
	wasabi := providerCost("Wasabi", tbStored, tbEgress, wasabiStoragePerTB, wasabiEgressPerTB, false)

	awsTotal := tbStored*awsStoragePerTB + tbEgress*awsEgressPerTB
	storedTotal := tbStored*storedStoragePerTB + tbEgress*storedEgressPerTB

	data["Providers"] = []ProviderCost{stored, aws, b2, wasabi}
	data["TotalSavingsVsAWS"] = fmt.Sprintf("$%.2f", awsTotal-storedTotal)
	data["EgressThisMonth"] = formatBytes(egressBytes)
}
