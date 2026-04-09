package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"

	"github.com/FairForge/vaultaire/internal/billing"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

// HandleBilling renders the billing page with plan, subscription status,
// invoice history, value stack, and cost comparison.
func HandleBilling(tmpl *template.Template, stripe *billing.StripeService, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "billing")
		withCSRF(r.Context(), data)
		ctx := r.Context()

		populateBillingPlan(ctx, db, data, sd.TenantID)
		populateBillingPlans(stripe, data)
		populateValueStack(ctx, db, data, sd.TenantID)
		populateCostComparison(ctx, db, data, sd.TenantID)

		if r.URL.Query().Get("upgraded") == "1" {
			data["Upgraded"] = true
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render billing page", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// HandleUpgrade redirects to a Stripe Checkout session for the chosen plan.
func HandleUpgrade(stripe *billing.StripeService, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
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

		customerID, err := stripe.GetCustomerID(r.Context(), sd.TenantID)
		if err != nil {
			logger.Error("get customer id for upgrade", zap.Error(err))
			http.Error(w, "Billing account not found. Please contact support.", http.StatusBadRequest)
			return
		}

		checkoutURL, err := stripe.CreateCheckoutSession(
			customerID, planID,
			"/dashboard/billing?upgraded=1",
			"/dashboard/billing",
		)
		if err != nil {
			logger.Error("create checkout session", zap.Error(err))
			http.Error(w, "Failed to start checkout. Please try again.", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, checkoutURL, http.StatusSeeOther)
	}
}

// HandleManageBilling redirects to the Stripe Billing Portal.
func HandleManageBilling(stripe *billing.StripeService, logger *zap.Logger) http.HandlerFunc {
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
			r.Context(), sd.TenantID, "/dashboard/billing",
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

func populateBillingPlans(stripe *billing.StripeService, data map[string]any) {
	if stripe == nil {
		data["AvailablePlans"] = nil
		return
	}
	data["AvailablePlans"] = stripe.Plans()
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

func populateCostComparison(ctx context.Context, db *sql.DB, data map[string]any, tenantID string) {
	// Calculate what the same storage would cost on AWS S3 Standard.
	// AWS S3: ~$23/TB/mo. stored.ge: $3.99/TB/mo.
	data["AWSCost"] = "$0.00"
	data["StoredCost"] = "$0.00"
	data["Savings"] = "$0.00"

	if db == nil {
		return
	}

	var usedBytes int64
	err := db.QueryRowContext(ctx,
		`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, tenantID).
		Scan(&usedBytes)
	if err != nil || usedBytes == 0 {
		return
	}

	tbUsed := float64(usedBytes) / (1024 * 1024 * 1024 * 1024)
	awsCost := tbUsed * 23.0
	storedCost := tbUsed * 3.99
	savings := awsCost - storedCost

	data["AWSCost"] = fmt.Sprintf("$%.2f/mo", awsCost)
	data["StoredCost"] = fmt.Sprintf("$%.2f/mo", storedCost)
	data["Savings"] = fmt.Sprintf("$%.2f/mo", savings)
}
