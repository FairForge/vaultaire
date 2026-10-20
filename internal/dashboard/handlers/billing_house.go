package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/FairForge/vaultaire/internal/api/landing"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/usage"
	"go.uber.org/zap"
)

// Whole-TB checkout on the billing page (docs/DASHBOARD_PLAN.md, Phase 1).
// The customer builds the same house the site sells — boxes per floor,
// a period — and pays for exactly that. Behind the quota_checkout flag,
// which is checked per tenant so it can be opened for one account first.

// FlagQuotaCheckout gates the house checkout (registered in api/server.go
// with default off; flipped per tenant, then globally, from /admin/flags).
const FlagQuotaCheckout = "quota_checkout"

// HouseBilling is what the billing page needs from money — the slice of
// *payments.Registry it uses (WP-PAY-2: the dashboard imports payments,
// never a provider SDK — TestDashboardImportsNoStripe). Checkout goes
// through the double-billing guard; resize writes the record when the
// provider changed the house in place.
type HouseBilling interface {
	Status(ctx context.Context, tenantID string) (payments.Status, error)
	Ready(ctx context.Context, tenantID, provider string) (bool, string)
	Enabled(ctx context.Context, tenantID string) []string
	StartCheckout(ctx context.Context, provider string, c payments.Checkout) (string, error)
	Resize(ctx context.Context, tenantID string, o payments.Order) (payments.ResizeOutcome, error)
	ManageURL(ctx context.Context, tenantID, returnURL string) (string, error)
}

// BillingService is what the billing page needs: the house checkout, the
// one product (WP-PAY-0). *payments.Registry implements it.
type BillingService interface {
	HouseBilling
}

// defaultProvider is the provider a checkout uses when the form names
// none (the only one so far).
const defaultProvider = payments.ProviderStripe

// providerLabels are the providers as the page names them.
var providerLabels = map[string]string{payments.ProviderStripe: "card (Stripe)", payments.ProviderBTCPay: "bitcoin (BTCPay)"}

// HouseView is the billing page's house section.
type HouseView struct {
	Enabled     bool   // quota_checkout is on for this tenant
	Ready       bool   // Stripe prices verified against prices.json
	NotReadyWhy string // shown to admins/logs; customers see "not open yet"

	Std, Vault, PinHot int    // stepper starting values
	Period             string // "annual" | "monthly"
	HasHouse           bool   // a granted house → resize mode
	Status             string
	SubscriptionID     string
	// Providers: the ways this tenant may pay now (flag on, ready).
	Providers []string
	// CanRenew: a prepaid or lapsed house a provider can take money for
	// (WP-PAY-3 item 3; RP21-2's way out) — the Renew button, labelled
	// RenewLabel ("Renew now" / "Add a year" / "Add a month"), on
	// RenewProvider (the row's provider when it may, else any enabled).
	CanRenew      bool
	RenewLabel    string
	RenewProvider string

	UsedStd, UsedVault       int64
	UsedStdFmt, UsedVaultFmt string
	MinStd, MinVault         int // whole-TB floors the steppers cannot go below (usage)

	Quote   payments.Quote
	Prices  landing.Prices
	RoomURL string
}

// populateHouse fills data["House"] and data["HouseSentence"]. With the
// flag off the section is disabled (the sentence still shows). add is the
// overview's "add a box" nudge: "downstairs" or "attic" pre-increments that
// floor's stepper by one. The house comes from the entitlement record
// (payments.Status); a house granted before migration 097 without a row
// falls back to the tenants columns.
func populateHouse(ctx context.Context, db *sql.DB, svc HouseBilling, fl *flags.Service, data map[string]any, tenantID, add string) {
	v := HouseView{Period: string(payments.PeriodAnnual), Prices: landing.Get()}
	data["House"] = v
	var st payments.Status
	if svc != nil && db != nil {
		var err error
		if st, err = svc.Status(ctx, tenantID); err == nil {
			data["HouseSentence"] = houseSentence(st)
			if p, label := renewOffer(ctx, svc, tenantID, st); p != "" {
				v.CanRenew, v.RenewProvider, v.RenewLabel = true, p, label
			}
		}
	}
	data["House"] = v
	if fl == nil || !fl.Enabled(FlagQuotaCheckout, tenantID) || db == nil {
		return
	}
	v.Enabled = true
	if svc == nil {
		v.NotReadyWhy = "billing not configured"
	} else {
		v.Ready, v.NotReadyWhy = svc.Ready(ctx, tenantID, defaultProvider)
		for _, p := range svc.Enabled(ctx, tenantID) {
			if l, ok := providerLabels[p]; ok {
				v.Providers = append(v.Providers, l)
			} else {
				v.Providers = append(v.Providers, p)
			}
		}
	}

	h, err := loadHouse(ctx, db, tenantID)
	if err != nil {
		data["House"] = v
		return
	}
	v.HasHouse, v.Status, v.SubscriptionID = h.active, h.status, h.subID
	order, period := h.order(), h.period
	if st.Found {
		v.HasHouse = st.Granted
		order, period = st.Entitlement.Order, string(st.Entitlement.Order.Period)
	}
	v.RoomURL = landing.HouseIntent{Room: h.intent.Room}.RoomURL()
	v.UsedStd, v.UsedVault = h.usedStd, h.usedVault
	v.UsedStdFmt, v.UsedVaultFmt = formatBytes(h.usedStd), formatBytes(h.usedVault)
	if v.HasHouse {
		v.Std, v.Vault, v.PinHot = order.Std, order.Vault, order.PinHot
		if period != "" {
			v.Period = period
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
	v.Quote = payments.QuoteHouse(v.Std, v.Vault, v.PinHot, payments.Period(v.Period))
	data["House"] = v
}

// renewOffer says whether the page offers to renew the house by hand and
// through which provider: a granted house nobody renews (prepaid) or a
// lapsed one, with the row's provider when the tenant may use it, else any
// provider that is on (a manual house is renewed by card; a lapsed Stripe
// house may be bought again). Never for a house a provider renews by
// itself. Not gated by quota_checkout: the house exists already.
func renewOffer(ctx context.Context, svc HouseBilling, tenantID string, st payments.Status) (provider, label string) {
	if !st.Found || (st.Granted && st.Entitlement.Renewal != payments.RenewalPrepaid) {
		return "", ""
	}
	e := st.Entitlement
	if ok, _ := svc.Ready(ctx, tenantID, e.Provider); ok {
		provider = e.Provider
	} else if enabled := svc.Enabled(ctx, tenantID); len(enabled) > 0 {
		provider = enabled[0]
	}
	if provider == "" {
		return "", ""
	}
	switch {
	case !st.Granted || st.InGrace:
		label = "Renew now"
	case e.Order.Period == payments.PeriodMonthly:
		label = "Add a month"
	default:
		label = "Add a year"
	}
	return provider, label
}

// houseSentence says the entitlement the way the page does: "Paid through
// 1 Mar 2027 via card (Stripe), renews automatically." — "" for a tenant
// that never bought.
func houseSentence(st payments.Status) string {
	if !st.Found {
		return ""
	}
	e := st.Entitlement
	via := providerLabels[e.Provider]
	if via == "" {
		via = e.Provider
	}
	if !st.Granted {
		return fmt.Sprintf("Your house ended on %s; your data is untouched.", e.PaidThrough.UTC().Format("2 Jan 2006"))
	}
	renew := "renews automatically"
	if e.Renewal == payments.RenewalPrepaid {
		renew = "renewal stopped"
	}
	s := fmt.Sprintf("Paid through %s via %s, %s.", e.PaidThrough.UTC().Format("2 Jan 2006"), via, renew)
	if st.InGrace {
		s += fmt.Sprintf(" The date has passed: the house is kept until %s while payment catches up.", st.GraceEnds.UTC().Format("2 Jan 2006"))
	}
	return s
}

// tenantHouse is what the billing page and the checkout handler read.
type tenantHouse struct {
	intent             landing.HouseIntent
	plan, status       string
	subID, period      string
	active             bool
	liveOther          bool // a non-house subscription Stripe is still billing, or a legacy plan
	legacy             bool // tenants.plan is a legacy pack id (legacyPlan)
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
	// Anything else with a subscription id that Stripe is still billing
	// (a legacy pack, an incomplete 3-D Secure checkout, a past-due pack)
	// must not be joined by a second subscription (Review R10-05).
	h.liveOther = !h.active && h.subID != "" && !terminalSubscriptionStatus(h.status)
	// A legacy pack plan (WP-PAY-0) is moved to a house by support, never
	// by a checkout beside it: its quota is the label's, and a pack bought
	// before may still bill.
	h.legacy = !h.active && legacyPlan(h.plan)
	h.liveOther = h.liveOther || h.legacy

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

// legacyOnly: refused for the legacy label alone — no subscription that
// Stripe is still billing.
func (h tenantHouse) legacyOnly() bool {
	return h.legacy && (h.subID == "" || terminalSubscriptionStatus(h.status))
}

// legacyPlan reports a tenants.plan that is neither the free tier nor a
// house: a legacy pack id ('vault3' … 'vault100', 'standard') — after
// WP-PAY-0 only an admin tier label (Bench2 on prod: 'vault100') or a pack
// bought before. The labels stay valid quota labels (tenant_quotas.tier,
// the admin tier picker); only the pack checkout is gone.
func legacyPlan(plan string) bool {
	switch plan {
	case "", "starter", "free", "house":
		return false
	}
	return true
}

// order is the house the tenants columns describe (the pre-097 fallback).
func (h tenantHouse) order() payments.Order {
	return payments.Order{Std: h.house.StdTB(), Vault: h.house.VaultTB(), PinHot: h.house.PinHotTB(), Period: payments.Period(h.period)}
}

// terminalSubscriptionStatus is a Stripe subscription status that no
// longer bills: a new subscription may be opened next to it.
func terminalSubscriptionStatus(status string) bool {
	switch status {
	case "", "canceled", "incomplete_expired":
		return true
	}
	return false
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
// to the provider's checkout (Stripe: one subscription, one item per line,
// quantity = TB) behind the double-billing guard; an existing house is
// resized in place by the provider that bills it and the record written at
// once (the webhook replays the same, idempotent write moments later). A
// provider whose flag is off answers 409 provider_disabled; a house renewed
// by another provider, 409 other_provider_live (docs/PAYMENTS.md § 2.3,
// § 2.5).
func HandleHouseCheckout(svc HouseBilling, db *sql.DB, fl *flags.Service, baseURL string, logger *zap.Logger) http.HandlerFunc {
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
		order := payments.Order{
			Std: formInt(r, "std"), Vault: formInt(r, "vault"), PinHot: formInt(r, "pin_hot"),
			Period: payments.Period(strings.TrimSpace(r.FormValue("period"))),
		}
		if err := payments.ValidateHouseOrder(order); err != nil {
			back(w, r, "error", "That house can't be ordered: "+err.Error()+".")
			return
		}
		if svc == nil || db == nil {
			back(w, r, "error", "Checkout isn't open yet — billing is not configured. Email "+hosts.Get().SupportEmail()+" and we will set it up by hand.")
			return
		}
		provider := strings.TrimSpace(r.FormValue("provider"))
		if provider == "" {
			provider = defaultProvider
		}

		h, err := loadHouse(r.Context(), db, sd.TenantID)
		if err != nil {
			logger.Error("load house for checkout", zap.Error(err), zap.String("tenant", sd.TenantID))
			back(w, r, "error", "Something went wrong reading your account. Please try again.")
			return
		}
		st, err := svc.Status(r.Context(), sd.TenantID)
		if err != nil {
			logger.Error("read entitlement for checkout", zap.Error(err), zap.String("tenant", sd.TenantID))
			back(w, r, "error", "Something went wrong reading your account. Please try again.")
			return
		}
		active, period := h.active, h.period
		if st.Found {
			active, period = st.Granted, string(st.Entitlement.Order.Period)
		}
		quote := payments.QuoteHouse(order.Std, order.Vault, order.PinHot, order.Period)

		if active {
			// Resize. The period is fixed for the life of the subscription
			// and a floor cannot shrink below what is stored on it.
			if string(order.Period) != period {
				back(w, r, "error", fmt.Sprintf("Your house is billed %s; to switch, email %s.", periodWord(period), hosts.Get().SupportEmail()))
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
			out, err := svc.Resize(r.Context(), sd.TenantID, order)
			if refusedForProvider(w, err) {
				return
			}
			if err != nil {
				logger.Error("resize house", zap.Error(err), zap.String("tenant", sd.TenantID))
				back(w, r, "error", "The resize didn't go through. Nothing was changed; please try again.")
				return
			}
			if out.RedirectURL != "" {
				http.Redirect(w, r, out.RedirectURL, http.StatusSeeOther) // #nosec G710 -- URL from the payment provider
				return
			}
			middleware.SetFlash(w, "success", fmt.Sprintf("Your house is now %s · %s/mo billed %s.",
				houseWords(order), quote.Monthly(), periodWord(string(order.Period))))
			http.Redirect(w, r, "/dashboard/billing?resized=1", http.StatusSeeOther)
			return
		}

		if h.liveOther && !h.legacyOnly() {
			back(w, r, "error", fmt.Sprintf("This account already has a subscription (%s). Email %s and we will move it to a house — a second checkout would bill you twice.", h.status, hosts.Get().SupportEmail()))
			return
		}
		if h.legacy {
			back(w, r, "error", fmt.Sprintf("This account is on a legacy plan (%s). Email %s and we will move it to a house — nothing changes until we do.", h.plan, hosts.Get().SupportEmail()))
			return
		}

		base := strings.TrimRight(baseURL, "/")
		checkoutURL, err := svc.StartCheckout(r.Context(), provider, payments.Checkout{
			TenantID: sd.TenantID, Email: sd.Email, Order: order, Purpose: payments.PurposeNew,
			ReturnURL: base + "/dashboard/billing?upgraded=1", CancelURL: base + "/dashboard/billing",
		})
		if refusedForProvider(w, err) {
			return
		}
		switch {
		case errors.Is(err, payments.ErrProviderUnavailable):
			logger.Warn("house checkout refused: not ready", zap.Error(err), zap.String("tenant", sd.TenantID))
			back(w, r, "error", "Checkout isn't open yet. Email "+hosts.Get().SupportEmail()+" and we will set it up by hand.")
			return
		case errors.Is(err, payments.ErrLiveSubscription):
			// The provider is the source of truth for "already paying": a
			// checkout completed seconds ago (webhook not yet landed) must
			// not get a second subscription (Review R10-05).
			back(w, r, "error", "You already have a subscription on this account — give the page a minute to catch up, then resize your house below. If it does not appear, email "+hosts.Get().SupportEmail()+".")
			return
		case err != nil:
			logger.Error("start house checkout", zap.Error(err), zap.String("tenant", sd.TenantID), zap.String("provider", provider))
			back(w, r, "error", "Checkout didn't start. Nothing was charged; please try again in a minute.")
			return
		}
		logger.Info("house checkout started", zap.String("tenant", sd.TenantID), zap.String("provider", provider),
			zap.Int("std_tb", order.Std), zap.Int("vault_tb", order.Vault), zap.Int("pin_hot_tb", order.PinHot),
			zap.String("period", string(order.Period)), zap.Int64("monthly_cents", quote.MonthlyCents))
		http.Redirect(w, r, checkoutURL, http.StatusSeeOther) // #nosec G710 -- URL from the payment provider
	}
}

// HandleRenewHouse is POST /dashboard/billing/renew (WP-PAY-3 item 3): a
// checkout with purpose "renew" for the row's own house — a prepaid house
// gets a period added, a lapsed one is bought again (RP21-2 closed) — on
// the provider renewOffer chose (a form field `provider` may name one of
// the enabled ones). A house a provider renews by itself is refused.
func HandleRenewHouse(svc HouseBilling, db *sql.DB, baseURL string, logger *zap.Logger) http.HandlerFunc {
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
		if svc == nil || db == nil {
			back(w, r, "error", "Renewal isn't open yet — billing is not configured. Email "+hosts.Get().SupportEmail()+" and we will set it up by hand.")
			return
		}
		st, err := svc.Status(r.Context(), sd.TenantID)
		if err != nil {
			logger.Error("read entitlement for renewal", zap.Error(err), zap.String("tenant", sd.TenantID))
			back(w, r, "error", "Something went wrong reading your account. Please try again.")
			return
		}
		provider, _ := renewOffer(r.Context(), svc, sd.TenantID, st)
		if provider == "" {
			if st.Found && st.Granted && st.Entitlement.Renewal == payments.RenewalAuto {
				back(w, r, "error", "Your house renews by itself; there is nothing to renew by hand. Manage Payment changes the card or stops the renewal.")
			} else {
				back(w, r, "error", "Renewal isn't open right now. Email "+hosts.Get().SupportEmail()+" and we will help.")
			}
			return
		}
		if asked := strings.TrimSpace(r.FormValue("provider")); asked != "" {
			provider = asked
		}
		order := st.Entitlement.Order
		base := strings.TrimRight(baseURL, "/")
		checkoutURL, err := svc.StartCheckout(r.Context(), provider, payments.Checkout{
			TenantID: sd.TenantID, Email: sd.Email, Order: order, Purpose: payments.PurposeRenew,
			ReturnURL: base + "/dashboard/billing?renewed=1", CancelURL: base + "/dashboard/billing",
		})
		if refusedForProvider(w, err) {
			return
		}
		switch {
		case errors.Is(err, payments.ErrProviderUnavailable):
			logger.Warn("house renewal refused: not ready", zap.Error(err), zap.String("tenant", sd.TenantID))
			back(w, r, "error", "Renewal isn't open yet. Email "+hosts.Get().SupportEmail()+" and we will set it up by hand.")
			return
		case errors.Is(err, payments.ErrLiveSubscription):
			back(w, r, "error", "You already have a subscription on this account that renews the house. If it does not show here, email "+hosts.Get().SupportEmail()+".")
			return
		case err != nil:
			logger.Error("start house renewal", zap.Error(err), zap.String("tenant", sd.TenantID), zap.String("provider", provider))
			back(w, r, "error", "Renewal didn't start. Nothing was charged; please try again in a minute.")
			return
		}
		logger.Info("house renewal started", zap.String("tenant", sd.TenantID), zap.String("provider", provider),
			zap.Int("std_tb", order.Std), zap.Int("vault_tb", order.Vault), zap.Int("pin_hot_tb", order.PinHot), zap.String("period", string(order.Period)))
		http.Redirect(w, r, checkoutURL, http.StatusSeeOther) // #nosec G710 -- URL from the payment provider
	}
}

// refusedForProvider answers the two provider refusals as 409 with the
// page's message (docs/PAYMENTS.md § 2.3, § 2.5) — rendered in the
// dashboard's error look (WP-PAY-3; the status and X-Error-Code are what
// WP-PAY-2's tests pin) — and reports whether it did.
func refusedForProvider(w http.ResponseWriter, err error) bool {
	var code, title, msg string
	switch {
	case errors.Is(err, payments.ErrProviderDisabled):
		code, title, msg = "provider_disabled", "This way of paying is switched off", "This way of paying is switched off right now, so nothing was started or changed. Email "+hosts.Get().SupportEmail()+" and we will help you pay another way."
	case errors.Is(err, payments.ErrOtherProviderLive):
		code, title, msg = "other_provider_live", "Your house is renewed another way", "Your house is renewed by another way of paying. Stop that renewal first (Manage Payment), then switch — otherwise you would pay twice."
	default:
		return false
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Error-Code", code)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusConflict)
	_, _ = fmt.Fprintf(w, refusalPageHTML, template.HTMLEscapeString(title), template.HTMLEscapeString(title), template.HTMLEscapeString(msg))
	return true
}

// refusalPageHTML is the 409 page: self-contained, the dashboard's error
// look (errors.go), a way back to billing.
const refusalPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s — Stored</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#0f172a;color:#e2e8f0;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}
.card{background:#1e293b;border-radius:12px;padding:2.5rem;max-width:480px;text-align:center}
h1{font-size:1.4rem;margin:0 0 0.75rem;color:#fbbf24}
p{color:#94a3b8;line-height:1.6}
a{color:#60a5fa;text-decoration:none}
a:hover{text-decoration:underline}
</style>
</head>
<body>
<div class="card">
<h1>%s</h1>
<p>%s</p>
<p><a href="/dashboard/billing">&larr; Back to billing</a></p>
</div>
</body>
</html>
`

func periodWord(p string) string {
	if p == string(payments.PeriodMonthly) {
		return "monthly"
	}
	return "yearly"
}

// houseWords says a house the way the site does: "5 TB downstairs, 2 TB in the attic".
func houseWords(o payments.Order) string {
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
