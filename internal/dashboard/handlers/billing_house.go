package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/FairForge/vaultaire/internal/billing"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

// Whole-TB checkout on the billing page (docs/DASHBOARD_PLAN.md, Phase 1).
// The customer builds the same house the site sells — boxes per floor,
// a period — and pays for exactly that. Behind the quota_checkout flag,
// which is checked per tenant so it can be opened for one account first.

// FlagQuotaCheckout gates the house checkout (registered in api/server.go
// with default off; flipped per tenant, then globally, from /admin/flags).
const FlagQuotaCheckout = "quota_checkout"

// HouseBilling is the slice of *billing.StripeService the house page uses.
type HouseBilling interface {
	HouseCheckoutReady() (bool, string)
	GetCustomerID(ctx context.Context, tenantID string) (string, error)
	CreateCustomer(ctx context.Context, email, tenantID string) (string, error)
	CreateHouseCheckout(ctx context.Context, customerID, tenantID string, o billing.HouseOrder, successURL, cancelURL string) (string, error)
	UpdateHouseSubscription(ctx context.Context, subID string, o billing.HouseOrder) (*stripe.Subscription, error)
	HouseFromSubscription(sub *stripe.Subscription) (billing.HouseOrder, bool)
}

// BillingService is what the billing page needs from Stripe: the legacy
// pack plans and the house checkout. *billing.StripeService implements it.
type BillingService interface {
	HouseBilling
	Plans() []billing.Plan
}

// HouseView is the billing page's house section.
type HouseView struct {
	Enabled     bool   // quota_checkout is on for this tenant
	Ready       bool   // Stripe prices verified against prices.json
	NotReadyWhy string // shown to admins/logs; customers see "not open yet"

	Std, Vault, PinHot int    // stepper starting values
	Period             string // "annual" | "monthly"
	HasHouse           bool   // an active house subscription → resize mode
	Status             string
	SubscriptionID     string

	UsedStd, UsedVault       int64
	UsedStdFmt, UsedVaultFmt string
	MinStd, MinVault         int // whole-TB floors the steppers cannot go below (usage)

	Quote   billing.Quote
	Prices  landing.Prices
	RoomURL string
}

// populateHouse fills data["House"]. With the flag off the section is
// disabled and the page falls back to the legacy plan grid. add is the
// overview's "add a box" nudge: "downstairs" or "attic" pre-increments that
// floor's stepper by one.
func populateHouse(ctx context.Context, db *sql.DB, svc HouseBilling, fl *flags.Service, data map[string]any, tenantID, add string) {
	v := HouseView{Period: string(billing.PeriodAnnual), Prices: landing.Get()}
	data["House"] = v
	if fl == nil || !fl.Enabled(FlagQuotaCheckout, tenantID) || db == nil {
		return
	}
	v.Enabled = true
	if svc == nil {
		v.NotReadyWhy = "billing not configured"
	} else {
		v.Ready, v.NotReadyWhy = svc.HouseCheckoutReady()
	}

	h, err := loadHouse(ctx, db, tenantID)
	if err != nil {
		data["House"] = v
		return
	}
	v.HasHouse, v.Status, v.SubscriptionID = h.active, h.status, h.subID
	v.RoomURL = landing.HouseIntent{Room: h.intent.Room}.RoomURL()
	v.UsedStd, v.UsedVault = h.usedStd, h.usedVault
	v.UsedStdFmt, v.UsedVaultFmt = formatBytes(h.usedStd), formatBytes(h.usedVault)
	if h.active {
		v.Std, v.Vault, v.PinHot = h.house.StdTB(), h.house.VaultTB(), h.house.PinHotTB()
		if h.period != "" {
			v.Period = h.period
		}
		v.MinStd, v.MinVault = wholeTBUp(h.usedStd), wholeTBUp(h.usedVault)
	} else {
		v.Std, v.Vault = h.intent.StdTB, h.intent.VaultTB
		if v.Std == 0 && v.Vault == 0 {
			v.Std = 1 // the site's first box
		}
	}
	switch add {
	case "downstairs":
		v.Std++
	case "attic":
		v.Vault++
	}
	v.Quote = billing.QuoteHouse(v.Std, v.Vault, v.PinHot, billing.Period(v.Period))
	data["House"] = v
}

// tenantHouse is what the billing page and the checkout handler read.
type tenantHouse struct {
	intent             landing.HouseIntent
	plan, status       string
	subID, period      string
	active             bool
	house              usage.House
	usedStd, usedVault int64
}

func loadHouse(ctx context.Context, db *sql.DB, tenantID string) (tenantHouse, error) {
	var h tenantHouse
	var sub sql.NullString
	var std, vault int
	var room string
	if err := db.QueryRowContext(ctx,
		`SELECT intent_std_tb, intent_vault_tb, intent_room, plan, subscription_status,
		        stripe_subscription_id, house_period
		   FROM tenants WHERE id = $1`, tenantID).
		Scan(&std, &vault, &room, &h.plan, &h.status, &sub, &h.period); err != nil {
		return h, fmt.Errorf("load house for %s: %w", tenantID, err)
	}
	h.intent = landing.HouseIntent{StdTB: std, VaultTB: vault, Room: room}
	h.subID = sub.String
	h.active = h.plan == "house" && h.subID != "" &&
		(h.status == "active" || h.status == "past_due" || h.status == "trialing")

	qm := usage.NewQuotaManager(db)
	house, ok, err := qm.GetHouse(ctx, tenantID)
	if err != nil {
		return h, err
	}
	if ok {
		h.house = house
		floors, err := qm.GetFloors(ctx, tenantID)
		if err != nil {
			return h, err
		}
		for _, f := range floors {
			switch f.Floor {
			case usage.FloorStandard:
				h.usedStd = f.UsedBytes
			case usage.FloorVault:
				h.usedVault = f.UsedBytes
			}
		}
	}
	return h, nil
}

// wholeTBUp is the smallest whole-TB quota that still holds n bytes.
func wholeTBUp(n int64) int {
	if n <= 0 {
		return 0
	}
	return int((n + usage.TB - 1) / usage.TB)
}

func formInt(r *http.Request, name string) int {
	v := strings.TrimSpace(r.FormValue(name))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1 // ValidateHouseOrder rejects it
	}
	return n
}

// HandleHouseCheckout is POST /dashboard/billing/house: a new house goes
// to Stripe Checkout (one subscription, one item per line, quantity = TB);
// an existing house is resized in place with proration and the quotas are
// applied straight from the returned subscription (the webhook replays
// the same, idempotent write moments later).
func HandleHouseCheckout(svc HouseBilling, db *sql.DB, quotas billing.HouseQuotas, fl *flags.Service, baseURL string, logger *zap.Logger) http.HandlerFunc {
	back := func(w http.ResponseWriter, r *http.Request, category, msg string) {
		middleware.SetFlash(w, category, msg)
		http.Redirect(w, r, "/dashboard/billing", http.StatusSeeOther)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if fl == nil || !fl.Enabled(FlagQuotaCheckout, sd.TenantID) {
			http.NotFound(w, r)
			return
		}
		order := billing.HouseOrder{
			Std: formInt(r, "std"), Vault: formInt(r, "vault"), PinHot: formInt(r, "pin_hot"),
			Period: billing.Period(strings.TrimSpace(r.FormValue("period"))),
		}
		if err := billing.ValidateHouseOrder(order); err != nil {
			back(w, r, "error", "That house can't be ordered: "+err.Error()+".")
			return
		}
		if svc == nil || db == nil {
			back(w, r, "error", "Checkout isn't open yet — billing is not configured. Email support@stored.ge and we will set it up by hand.")
			return
		}
		if ready, why := svc.HouseCheckoutReady(); !ready {
			logger.Warn("house checkout refused: not ready", zap.String("why", why), zap.String("tenant", sd.TenantID))
			back(w, r, "error", "Checkout isn't open yet. Email support@stored.ge and we will set it up by hand.")
			return
		}

		h, err := loadHouse(r.Context(), db, sd.TenantID)
		if err != nil {
			logger.Error("load house for checkout", zap.Error(err), zap.String("tenant", sd.TenantID))
			back(w, r, "error", "Something went wrong reading your account. Please try again.")
			return
		}
		quote := billing.QuoteHouse(order.Std, order.Vault, order.PinHot, order.Period)

		if h.active {
			// Resize. The period is fixed for the life of the subscription
			// and a floor cannot shrink below what is stored on it.
			if string(order.Period) != h.period {
				back(w, r, "error", fmt.Sprintf("Your house is billed %s; to switch, email support@stored.ge.", periodWord(h.period)))
				return
			}
			if order.Std < wholeTBUp(h.usedStd) {
				back(w, r, "error", fmt.Sprintf("Downstairs holds %s right now, so it can't go below %d TB. Put things in the attic or delete them first.",
					formatBytes(h.usedStd), wholeTBUp(h.usedStd)))
				return
			}
			if order.Vault < wholeTBUp(h.usedVault) {
				back(w, r, "error", fmt.Sprintf("The attic holds %s right now, so it can't go below %d TB. Bring things downstairs or delete them first.",
					formatBytes(h.usedVault), wholeTBUp(h.usedVault)))
				return
			}
			sub, err := svc.UpdateHouseSubscription(r.Context(), h.subID, order)
			if err != nil {
				logger.Error("resize house", zap.Error(err), zap.String("tenant", sd.TenantID))
				back(w, r, "error", "The resize didn't go through. Nothing was changed; please try again.")
				return
			}
			if got, ok := svc.HouseFromSubscription(sub); ok && quotas != nil {
				if err := quotas.SetHouse(r.Context(), sd.TenantID, usage.HouseFromTB(got.Std, got.Vault, got.PinHot)); err != nil {
					logger.Error("apply resized house", zap.Error(err), zap.String("tenant", sd.TenantID))
				}
			}
			middleware.SetFlash(w, "success", fmt.Sprintf("Your house is now %s · %s/mo billed %s.",
				houseWords(order), quote.Monthly(), periodWord(string(order.Period))))
			http.Redirect(w, r, "/dashboard/billing?resized=1", http.StatusSeeOther)
			return
		}

		customerID, err := svc.GetCustomerID(r.Context(), sd.TenantID)
		if err != nil {
			// Registered while Stripe was down: create the customer now.
			customerID, err = svc.CreateCustomer(r.Context(), sd.Email, sd.TenantID)
			if err != nil {
				logger.Error("create stripe customer for house checkout", zap.Error(err), zap.String("tenant", sd.TenantID))
				back(w, r, "error", "We couldn't open a billing account for you. Email support@stored.ge and we will sort it out.")
				return
			}
		}
		base := strings.TrimRight(baseURL, "/")
		checkoutURL, err := svc.CreateHouseCheckout(r.Context(), customerID, sd.TenantID, order,
			base+"/dashboard/billing?upgraded=1", base+"/dashboard/billing")
		if err != nil {
			logger.Error("create house checkout", zap.Error(err), zap.String("tenant", sd.TenantID))
			back(w, r, "error", "Checkout didn't start. Please try again.")
			return
		}
		logger.Info("house checkout started", zap.String("tenant", sd.TenantID),
			zap.Int("std_tb", order.Std), zap.Int("vault_tb", order.Vault), zap.Int("pin_hot_tb", order.PinHot),
			zap.String("period", string(order.Period)), zap.Int64("monthly_cents", quote.MonthlyCents))
		http.Redirect(w, r, checkoutURL, http.StatusSeeOther) // #nosec G710 -- URL from Stripe API
	}
}

func periodWord(p string) string {
	if p == string(billing.PeriodMonthly) {
		return "monthly"
	}
	return "yearly"
}

// houseWords says a house the way the site does: "5 TB downstairs, 2 TB in the attic".
func houseWords(o billing.HouseOrder) string {
	var parts []string
	if o.Std > 0 {
		parts = append(parts, fmt.Sprintf("%d TB downstairs", o.Std))
	}
	if o.Vault > 0 {
		parts = append(parts, fmt.Sprintf("%d TB in the attic", o.Vault))
	}
	if o.PinHot > 0 {
		parts = append(parts, fmt.Sprintf("%d TB pinned hot", o.PinHot))
	}
	return strings.Join(parts, ", ")
}
