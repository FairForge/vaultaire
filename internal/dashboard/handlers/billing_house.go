package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

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
	// SwitchToPrepaid is "Switch to bitcoin at renewal" (WP-PAY-5 item 1).
	SwitchToPrepaid(ctx context.Context, tenantID, to, actor string) (payments.Entitlement, error)
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
	// Providers: the ways this tenant may pay now (flag on, ready).
	Providers []string
	// CanRenew: a prepaid or lapsed house a provider can take money for
	// (WP-PAY-3 item 3; RP21-2's way out) — the Renew button, labelled
	// RenewLabel ("Renew now" / "Add a year" / "Add a month"), on
	// RenewProvider (the row's provider when it may, else any enabled).
	CanRenew      bool
	RenewLabel    string
	RenewProvider string
	// The switch offers (WP-PAY-5): SwitchToCrypto — Stripe renews and
	// bitcoin is on ("Switch to bitcoin at renewal"); SwitchToCard — a
	// prepaid house and card is on ("Switch to card"), its first charge on
	// the paid date unless that is under 48 h away (SwitchToCardStartsNow).
	SwitchToCrypto, SwitchToCard, SwitchToCardStartsNow bool

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
// (payments.Status) and nothing else (WP-PAY-7: the tenants columns are
// no longer read — a house without a row is no house).
func populateHouse(ctx context.Context, db *sql.DB, svc HouseBilling, fl *flags.Service, data map[string]any, tenantID, add string) {
	v := HouseView{Period: string(payments.PeriodAnnual), Prices: landing.Get()}
	data["House"] = v
	var st payments.Status
	if svc != nil && db != nil {
		var err error
		if st, err = svc.Status(ctx, tenantID); err == nil {
			lapseOn := fl != nil && fl.Enabled(payments.FlagEntitlementLapse, tenantID)
			data["HouseSentence"] = houseSentence(st, endsAtDate(st, lapseOn))
			if p, label := renewOffer(ctx, svc, tenantID, st); p != "" {
				v.CanRenew, v.RenewProvider, v.RenewLabel = true, p, label
			}
			switchOffers(ctx, svc, tenantID, st, &v)
			if v.CanRenew && v.RenewProvider == payments.ProviderStripe {
				v.SwitchToCard = false // the Renew button is that same card checkout
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
	v.HasHouse = st.Granted
	order, period := st.Entitlement.Order, string(st.Entitlement.Order.Period)
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
// itself, and only inside the reminder window — within
// payments.ReminderLead (14 d) of the date, in grace, or lapsed (RP24-1:
// "Add a year" by card on a freshly paid row is WP-PAY-5's switch, not a
// renew). Not gated by quota_checkout: the house exists already.
func renewOffer(ctx context.Context, svc HouseBilling, tenantID string, st payments.Status) (provider, label string) {
	if !st.Found || (st.Granted && st.Entitlement.Renewal != payments.RenewalPrepaid) {
		return "", ""
	}
	if st.Granted && !st.InGrace && time.Until(st.Entitlement.PaidThrough) > payments.ReminderLead {
		return "", ""
	}
	e := st.Entitlement
	if e.PendingProvider != "" {
		// A switch the customer asked for (WP-PAY-5): the next period is paid
		// the new way, from the date already paid.
		if ok, _ := svc.Ready(ctx, tenantID, e.PendingProvider); ok {
			return e.PendingProvider, "Pay the next " + periodNoun(e.Order.Period) + " in " + payWord(e.PendingProvider)
		}
	}
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
// endsAtDate: the house ends ON its paid date — a card house whose renewal
// is off while entitlement_lapse is off for the tenant: Stripe deletes the
// subscription at the date and the webhook clears the house, with no grace
// (RP31-7). lapseOn is the flag for the tenant.
func endsAtDate(st payments.Status, lapseOn bool) bool {
	e := st.Entitlement
	return !lapseOn && st.Found && st.Granted && e.Provider == payments.ProviderStripe && e.Renewal == payments.RenewalPrepaid
}

func houseSentence(st payments.Status, endsAtDate bool) string {
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
		if e.Provider == payments.ProviderBTCPay || e.Provider == payments.ProviderManual {
			renew = "renewal stopped (prepaid: you pay the next period here, before the date)"
		}
	}
	s := fmt.Sprintf("Paid through %s via %s, %s.", e.PaidThrough.UTC().Format("2 Jan 2006"), via, renew)
	if e.PendingProvider != "" && e.PendingProvider != e.Provider {
		// WP-PAY-5: the switch the customer asked for.
		s = fmt.Sprintf("Paid through %s via %s; its renewal is off — you chose to pay in %s from then on. The %s invoice opens here on %s and its period starts on the paid date.",
			e.PaidThrough.UTC().Format("2 Jan 2006"), via, payWord(e.PendingProvider), payWord(e.PendingProvider),
			e.PaidThrough.Add(-payments.ReminderLead).UTC().Format("2 Jan 2006"))
		if endsAtDate {
			s += fmt.Sprintf(" Your house ends on %s unless the %s payment arrives.", e.PaidThrough.UTC().Format("2 Jan 2006"), payWord(e.PendingProvider))
		}
	} else if endsAtDate {
		s += fmt.Sprintf(" Your house ends on %s unless you renew.", e.PaidThrough.UTC().Format("2 Jan 2006"))
	}
	if st.InGrace {
		s += fmt.Sprintf(" The date has passed: the house is kept until %s while payment catches up.", st.GraceEnds.UTC().Format("2 Jan 2006"))
	}
	return s
}

// tenantHouse is what the billing page and the checkout handler read
// beside the entitlement record: the house the customer built on the site,
// the plan label and what each floor holds.
type tenantHouse struct {
	intent             landing.HouseIntent
	plan               string
	legacy             bool // tenants.plan is a legacy pack id (legacyPlan)
	house              usage.House
	usedStd, usedVault int64
}

// loadHouse reads the tenant's intent, plan label and floors. Whether a
// house is granted is the entitlement record's (payments.Status), never a
// tenants column (WP-PAY-7). A subscription a provider still bills beside
// no granted house (an incomplete 3-D Secure checkout) is the provider's
// guard to refuse — payments.ErrLiveSubscription (R10-05).
func loadHouse(ctx context.Context, db *sql.DB, tenantID string) (tenantHouse, error) {
	var h tenantHouse
	var std, vault int
	var room string
	if err := db.QueryRowContext(ctx,
		`SELECT intent_std_tb, intent_vault_tb, intent_room, COALESCE(plan, '') FROM tenants WHERE id = $1`, tenantID).
		Scan(&std, &vault, &room, &h.plan); err != nil {
		return h, fmt.Errorf("load house for %s: %w", tenantID, err)
	}
	h.intent = landing.HouseIntent{StdTB: std, VaultTB: vault, Room: room}
	// A legacy pack plan (WP-PAY-0) is moved to a house by support, never
	// by a checkout beside it: its quota is the label's, and a pack bought
	// before may still bill.
	h.legacy = legacyPlan(h.plan)

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
		active, period := st.Granted, string(st.Entitlement.Order.Period)
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
			if refusedForProvider(w, r, err) {
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

		if h.legacy {
			back(w, r, "error", fmt.Sprintf("This account is on a legacy plan (%s). Email %s and we will move it to a house — nothing changes until we do.", h.plan, hosts.Get().SupportEmail()))
			return
		}

		base := strings.TrimRight(baseURL, "/")
		checkoutURL, err := svc.StartCheckout(r.Context(), provider, payments.Checkout{
			TenantID: sd.TenantID, Email: sd.Email, Order: order, Purpose: payments.PurposeNew,
			ReturnURL: base + "/dashboard/billing?upgraded=1", CancelURL: base + "/dashboard/billing",
		})
		if refusedForProvider(w, r, err) {
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
		if asked := strings.TrimSpace(r.FormValue("provider")); asked != "" && asked != provider {
			// RP24-5: only a provider the page could have offered — never an
			// arbitrary form string into Registry.For (it was logged at
			// Error with the user's own value).
			if !slices.Contains(svc.Enabled(r.Context(), sd.TenantID), asked) {
				logger.Warn("house renewal: the provider form value is not an enabled provider", zap.String("tenant", sd.TenantID))
				back(w, r, "error", "That way of paying isn't available. Email "+hosts.Get().SupportEmail()+" and we will help.")
				return
			}
			provider = asked
		}
		co := payments.Checkout{
			TenantID: sd.TenantID, Email: sd.Email, Order: st.Entitlement.Order, Purpose: payments.PurposeRenew,
		}
		if provider == st.Entitlement.PendingProvider {
			co.Purpose = payments.PurposeSwitch // WP-PAY-5: the switch asked for
		}
		// A renew of a GRANTED row is a top-up: the first charge lands on
		// the date already paid through (RP24-1; Stripe: trial_end). Inside
		// payments.MinDeferredStart (48 h) the provider starts now, so the
		// page says the remaining prepaid days are folded in.
		forfeits := false
		if st.Granted {
			if pt := st.Entitlement.PaidThrough; pt.After(time.Now()) {
				co.StartAt = pt
				forfeits = time.Until(pt) < payments.MinDeferredStart
			}
		}
		order := co.Order
		base := strings.TrimRight(baseURL, "/")
		co.ReturnURL, co.CancelURL = base+"/dashboard/billing?renewed=1", base+"/dashboard/billing"
		checkoutURL, err := svc.StartCheckout(r.Context(), provider, co)
		if refusedForProvider(w, r, err) {
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
			zap.Int("std_tb", order.Std), zap.Int("vault_tb", order.Vault), zap.Int("pin_hot_tb", order.PinHot), zap.String("period", string(order.Period)),
			zap.Time("start_at", co.StartAt), zap.Bool("starts_now", forfeits))
		if forfeits {
			// Seen when the customer comes back to the billing page — the
			// design's sentence (docs/PAYMENTS.md § 4.2).
			middleware.SetFlash(w, "info", "The paid-through date is less than two days away, so the new period starts today and the remaining prepaid days are forfeited.")
		}
		http.Redirect(w, r, checkoutURL, http.StatusSeeOther) // #nosec G710 -- URL from the payment provider
	}
}

// refusedForProvider answers the provider refusals as 409 with the
// page's message (docs/PAYMENTS.md § 2.3, § 2.5) — rendered in the
// dashboard's error look (WP-PAY-3; the status and X-Error-Code are what
// WP-PAY-2's tests pin) — and reports whether it did. The
// crypto_invoice_open page of a checkout the customer can continue offers
// "Cancel the bitcoin invoice" (RP31-5, cryptoCancelForm); one whose invoice
// holds a payment says so and keeps only its date.
func refusedForProvider(w http.ResponseWriter, r *http.Request, err error) bool {
	var code, title, msg string
	var extra template.HTML
	switch {
	case errors.Is(err, payments.ErrProviderDisabled):
		code, title, msg = "provider_disabled", "This way of paying is switched off", "This way of paying is switched off right now, so nothing was started or changed. Email "+hosts.Get().SupportEmail()+" and we will help you pay another way."
	case errors.Is(err, payments.ErrOtherProviderLive):
		code, title, msg = "other_provider_live", "Your house is renewed another way", "Your house is renewed by another way of paying. Stop that renewal first (Manage Payment), then switch — otherwise you would pay twice."
	case errors.Is(err, payments.ErrCryptoInvoiceOpen):
		until := "a day after it was opened"
		var open *payments.OpenInvoiceError
		if errors.As(err, &open) {
			until = open.Until.UTC().Format("2 Jan 2006 15:04 UTC")
		}
		code, title = "crypto_invoice_open", "A bitcoin invoice is still open"
		switch {
		case open != nil && open.Paid:
			msg = "The bitcoin invoice on your account already holds a payment, so it can't be cancelled — it is left to settle, and its period is added to your house. " +
				"It can settle until " + until + "; wait until then before paying by card, otherwise you could pay twice. Nothing was started or charged."
		default:
			msg = "A bitcoin invoice on your account can still be paid until " + until +
				". Pay it, or wait until then, before paying by card — otherwise you could pay twice. Nothing was started or charged."
			if open != nil {
				extra = cryptoCancelForm(r, open)
			}
		}
	default:
		return false
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Error-Code", code)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusConflict)
	_, _ = fmt.Fprintf(w, refusalPageHTML, template.HTMLEscapeString(title), template.HTMLEscapeString(title), template.HTMLEscapeString(msg), extra)
	return true
}

// cryptoContinuations are the forms a crypto_invoice_open refusal can come
// from, by the last element of their path: after the invoice is cancelled
// the same request continues there (HandleCancelCryptoInvoice).
var cryptoContinuations = []string{"house", "renew", "switch"}

// cryptoCancelForm is the "Cancel the bitcoin invoice" button of the 409
// page (RP31-5): it posts the invoice, where to continue, and the refused
// form's own fields back, so that after the cancel the checkout proceeds
// as the customer asked. Empty when the refused request is not one the
// cancel can continue.
func cryptoCancelForm(r *http.Request, open *payments.OpenInvoiceError) template.HTML {
	if r == nil {
		return ""
	}
	then := path.Base(r.URL.Path)
	if !slices.Contains(cryptoContinuations, then) {
		return ""
	}
	_ = r.ParseForm()
	var b strings.Builder
	field := func(name, value string) {
		fmt.Fprintf(&b, `<input type="hidden" name="%s" value="%s">`, template.HTMLEscapeString(name), template.HTMLEscapeString(value))
	}
	b.WriteString(`<form method="post" action="/dashboard/billing/crypto-invoice/cancel">`)
	field("csrf_token", middleware.Token(r.Context()))
	field(cancelInvoiceField, open.Invoice)
	field(cancelProviderField, open.Provider)
	field(cancelThenField, then)
	names := make([]string, 0, len(r.PostForm))
	for k := range r.PostForm {
		if k != "csrf_token" && !strings.HasPrefix(k, "cancel_") {
			names = append(names, k)
		}
	}
	slices.Sort(names)
	for _, k := range names {
		for _, v := range r.PostForm[k] {
			field(k, v)
		}
	}
	b.WriteString(`<button type="submit">Cancel the bitcoin invoice and continue</button></form>`)
	b.WriteString(`<p class="small">Only an invoice nobody has paid can be cancelled. If a payment is on its way, wait for it instead.</p>`)
	return template.HTML(b.String()) // #nosec G203 -- every value escaped above
}

// The cancel form's own fields (prefixed so they never collide with the
// refused form's).
const (
	cancelInvoiceField  = "cancel_invoice"
	cancelProviderField = "cancel_provider"
	cancelThenField     = "cancel_then"
)

// InvoiceCancelling is what the cancel needs (*payments.Registry).
type InvoiceCancelling interface {
	CancelUnpaidInvoice(ctx context.Context, tenantID, provider, invoiceID, actor string) error
}

// HandleCancelCryptoInvoice is POST /dashboard/billing/crypto-invoice/cancel
// (RP31-5): the session's tenant withdraws its open bitcoin invoice — only
// one nobody paid (the provider fetches it: New or Expired, nothing paid;
// it is marked Invalid and our row closed, audited billing.invoice_marked)
// — and the checkout the 409 refused continues with the same form (next,
// by cancel_then). An invoice holding a payment keeps the 409 with its
// date.
func HandleCancelCryptoInvoice(svc InvoiceCancelling, next map[string]http.Handler, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		back := func(category, msg string) {
			middleware.SetFlash(w, category, msg)
			http.Redirect(w, r, "/dashboard/billing", http.StatusSeeOther)
		}
		invoice, provider, then := r.FormValue(cancelInvoiceField), r.FormValue(cancelProviderField), r.FormValue(cancelThenField)
		h, ok := next[then]
		if svc == nil || invoice == "" || provider == "" || !ok {
			back("error", "That invoice can't be cancelled from here. Email "+hosts.Get().SupportEmail()+" and we will help.")
			return
		}
		err := svc.CancelUnpaidInvoice(r.Context(), sd.TenantID, provider, invoice, sd.UserID)
		if refusedForProvider(w, r, err) {
			return
		}
		switch {
		case errors.Is(err, payments.ErrNoSuchPayment):
			back("error", "That invoice is no longer open — nothing to cancel.")
			return
		case errors.Is(err, payments.ErrInvoiceCancelPending):
			// BTCPay has it cancelled; our row closes with its delivery
			// (RP33-8b). The checkout would still be refused until then.
			logger.Warn("crypto invoice cancelled at the provider, row closes with its webhook", zap.Error(err),
				zap.String("tenant", sd.TenantID), zap.String("invoice", invoice))
			back("info", "The bitcoin invoice was cancelled. This page updates within a minute or two — then choose card again.")
			return
		case err != nil:
			logger.Error("cancel crypto invoice", zap.Error(err), zap.String("tenant", sd.TenantID), zap.String("invoice", invoice))
			back("error", "The invoice couldn't be cancelled right now. Nothing was changed; please try again in a minute.")
			return
		}
		logger.Info("crypto invoice cancelled by the customer", zap.String("tenant", sd.TenantID), zap.String("invoice", invoice),
			zap.String("then", then))
		h.ServeHTTP(w, r)
	}
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
button{background:#2563eb;color:#fff;border:0;border-radius:8px;padding:0.6rem 1rem;font-size:0.95rem;cursor:pointer}
.small{font-size:0.85rem}
</style>
</head>
<body>
<div class="card">
<h1>%s</h1>
<p>%s</p>
%s
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
