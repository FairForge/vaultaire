package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/payments"
	"go.uber.org/zap"
)

// Switching how the house is paid (WP-PAY-5, docs/PAYMENTS.md § 4): no gap,
// no overlap — the date already paid is never touched.
//
//	card → bitcoin: "Switch to bitcoin at renewal" stops the card renewal
//	                (cancel_at_period_end) and marks bitcoin as the next way to
//	                pay; from 14 days before the date the page offers the
//	                bitcoin invoice (renewOffer), whose period starts at the
//	                paid date.
//	bitcoin → card: a Stripe checkout for the same house whose first charge
//	                is the paid date (trial_end); inside 48 h it starts now
//	                and the page says the remaining prepaid days are forfeited.

// switchOffers fills the switch fields of the page's house: which way the
// customer may switch now (the target provider on and ready for them).
func switchOffers(ctx context.Context, svc HouseBilling, tenantID string, st payments.Status, v *HouseView) {
	if !st.Found || !st.Granted {
		return
	}
	e := st.Entitlement
	enabled := svc.Enabled(ctx, tenantID)
	switch {
	case e.Provider == payments.ProviderStripe && e.Renewal == payments.RenewalAuto && slices.Contains(enabled, payments.ProviderBTCPay):
		v.SwitchToCrypto = true
	case e.Provider != payments.ProviderStripe && e.Renewal == payments.RenewalPrepaid && e.PendingProvider == "" &&
		slices.Contains(enabled, payments.ProviderStripe):
		v.SwitchToCard = true
		v.SwitchToCardStartsNow = time.Until(e.PaidThrough) < payments.MinDeferredStart
	}
}

// HandleSwitchProvider is POST /dashboard/billing/switch (form `to`): to a
// prepaid provider (bitcoin) = payments.SwitchToPrepaid; to Stripe = a
// checkout with purpose "switch" whose first charge is paid_through.
func HandleSwitchProvider(svc HouseBilling, db *sql.DB, baseURL string, logger *zap.Logger) http.HandlerFunc {
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
			back(w, r, "error", "Switching isn't open yet — billing is not configured. Email "+hosts.Get().SupportEmail()+" and we will do it by hand.")
			return
		}
		to := strings.TrimSpace(r.FormValue("to"))
		if !slices.Contains(svc.Enabled(r.Context(), sd.TenantID), to) {
			// Only a provider the page could have offered (RP24-5's rule).
			back(w, r, "error", "That way of paying isn't available. Email "+hosts.Get().SupportEmail()+" and we will help.")
			return
		}
		st, err := svc.Status(r.Context(), sd.TenantID)
		if err != nil {
			logger.Error("read entitlement for switch", zap.Error(err), zap.String("tenant", sd.TenantID))
			back(w, r, "error", "Something went wrong reading your account. Please try again.")
			return
		}
		if !st.Found || !st.Granted {
			back(w, r, "error", "There is no house to switch yet — buy one below.")
			return
		}
		if to == payments.ProviderStripe {
			switchToCard(w, r, svc, sd, st, baseURL, logger, back)
			return
		}
		e, err := svc.SwitchToPrepaid(r.Context(), sd.TenantID, to, sd.UserID)
		if refusedForProvider(w, err) {
			return
		}
		switch {
		case errors.Is(err, payments.ErrNothingToSwitch):
			back(w, r, "error", "Your house is already set to be paid that way.")
			return
		case errors.Is(err, payments.ErrProviderUnavailable):
			back(w, r, "error", "Switching isn't possible right now. Nothing was changed; please try again in a minute.")
			return
		case err != nil:
			logger.Error("switch to prepaid", zap.Error(err), zap.String("tenant", sd.TenantID), zap.String("to", to))
			back(w, r, "error", "The switch didn't go through. Nothing was changed; please try again in a minute.")
			return
		}
		logger.Info("house switch requested", zap.String("tenant", sd.TenantID), zap.String("from", e.Provider), zap.String("to", to))
		opens := e.PaidThrough.Add(-payments.ReminderLead)
		back(w, r, "success", fmt.Sprintf("Card renewal is off. Your house is paid through %s; from %s you can pay the next %s in %s here — it starts on that date, so nothing is lost or paid twice.",
			e.PaidThrough.UTC().Format("2 Jan 2006"), opens.UTC().Format("2 Jan 2006"), periodNoun(e.Order.Period), payWord(to)))
	}
}

// switchToCard starts the Stripe checkout of a switch from a prepaid house.
func switchToCard(w http.ResponseWriter, r *http.Request, svc HouseBilling, sd *dashauth.SessionData, st payments.Status,
	baseURL string, logger *zap.Logger, back func(http.ResponseWriter, *http.Request, string, string)) {
	e := st.Entitlement
	if e.Provider == payments.ProviderStripe {
		back(w, r, "error", "Your house is already paid by card. Manage Payment changes the card or the renewal.")
		return
	}
	base := strings.TrimRight(baseURL, "/")
	co := payments.Checkout{TenantID: sd.TenantID, Email: sd.Email, Order: e.Order, Purpose: payments.PurposeSwitch,
		ReturnURL: base + "/dashboard/billing?renewed=1", CancelURL: base + "/dashboard/billing"}
	forfeits := true
	if e.PaidThrough.After(time.Now()) {
		co.StartAt = e.PaidThrough
		forfeits = time.Until(e.PaidThrough) < payments.MinDeferredStart
	}
	url, err := svc.StartCheckout(r.Context(), payments.ProviderStripe, co)
	if refusedForProvider(w, err) {
		return
	}
	switch {
	case errors.Is(err, payments.ErrProviderUnavailable):
		back(w, r, "error", "Card payment isn't open yet. Email "+hosts.Get().SupportEmail()+" and we will set it up by hand.")
		return
	case errors.Is(err, payments.ErrLiveSubscription):
		back(w, r, "error", "You already have a card subscription on this account. If it does not show here, email "+hosts.Get().SupportEmail()+".")
		return
	case err != nil:
		logger.Error("start switch to card", zap.Error(err), zap.String("tenant", sd.TenantID))
		back(w, r, "error", "The switch didn't start. Nothing was charged; please try again in a minute.")
		return
	}
	logger.Info("house switch to card started", zap.String("tenant", sd.TenantID), zap.String("from", e.Provider),
		zap.Time("start_at", co.StartAt), zap.Bool("starts_now", forfeits))
	if forfeits {
		middleware.SetFlash(w, "info", "The paid-through date is less than two days away, so card billing starts today and the remaining prepaid days are forfeited.")
	} else {
		middleware.SetFlash(w, "info", "Your card is charged first on "+e.PaidThrough.UTC().Format("2 Jan 2006")+", the day your prepaid time ends.")
	}
	http.Redirect(w, r, url, http.StatusSeeOther) // #nosec G710 -- URL from the payment provider
}

func periodNoun(p payments.Period) string {
	if p == payments.PeriodMonthly {
		return "month"
	}
	return "year"
}

// payWord names a provider the way the page's sentences do.
func payWord(provider string) string {
	switch provider {
	case payments.ProviderBTCPay:
		return "bitcoin"
	case payments.ProviderStripe:
		return "card"
	}
	return provider
}
