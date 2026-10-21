package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stripe/stripe-go/v75"
	checkoutsession "github.com/stripe/stripe-go/v75/checkout/session"
	"github.com/stripe/stripe-go/v75/price"
	"github.com/stripe/stripe-go/v75/subscription"
	"go.uber.org/zap"
)

// Whole-TB house checkout (docs/DASHBOARD_PLAN.md, Phase 1).
//
// The public site sells storage as a house: downstairs = Standard, attic =
// Vault, whole TB per floor, plus the pin-hot add-on on Standard. Billing
// sells exactly that: one Stripe subscription per tenant with one item per
// line (price = floor × period, quantity = TB). The dollar amounts live in
// internal/api/landing/prices.json only; the Stripe prices are created by
// hand in the dashboard, their ids arrive from env, and VerifyHousePrices
// asserts each one's amount, currency and interval against the file before
// checkout is offered — a mismatch disables checkout loudly rather than
// charging the wrong number.
//
// Stripe setup (once per account, test and live):
//
//	STRIPE_PRICE_STANDARD_ANNUAL   recurring yearly,  per unit, USD 53.88  (= 4.49 × 12)
//	STRIPE_PRICE_STANDARD_MONTHLY  recurring monthly, per unit, USD  4.99
//	STRIPE_PRICE_VAULT_ANNUAL      recurring yearly,  per unit, USD 24.00  (= 2.00 × 12)
//	STRIPE_PRICE_VAULT_MONTHLY     recurring monthly, VOLUME tiers: 1 unit → flat USD 4.99;
//	                               2+ units → USD 2.55 per unit (the "$4.99 monthly minimum")
//	STRIPE_PRICE_PINHOT_ANNUAL     recurring yearly,  per unit, USD 36.00  (= 3.00 × 12)
//	STRIPE_PRICE_PINHOT_MONTHLY    recurring monthly, per unit, USD  3.00

// Line identifies a subscription line; Period its billing interval — the
// provider-neutral ones of internal/payments (WP-PAY-1; the pricing moved
// there in WP-PAY-2 so the dashboard prices a house without importing the
// Stripe SDK).
type (
	Line   = payments.Line
	Period = payments.Period
)

const (
	LineStandard = payments.LineStandard
	LineVault    = payments.LineVault
	LinePinHot   = payments.LinePinHot

	PeriodAnnual  = payments.PeriodAnnual
	PeriodMonthly = payments.PeriodMonthly
)

// HousePriceIDs holds the six Stripe price ids.
type HousePriceIDs struct {
	StdAnnual, StdMonthly     string
	VaultAnnual, VaultMonthly string
	PinAnnual, PinMonthly     string
}

// HousePriceIDsFromEnv reads the STRIPE_PRICE_* house ids.
func HousePriceIDsFromEnv(getenv func(string) string) HousePriceIDs {
	return HousePriceIDs{
		StdAnnual: getenv("STRIPE_PRICE_STANDARD_ANNUAL"), StdMonthly: getenv("STRIPE_PRICE_STANDARD_MONTHLY"),
		VaultAnnual: getenv("STRIPE_PRICE_VAULT_ANNUAL"), VaultMonthly: getenv("STRIPE_PRICE_VAULT_MONTHLY"),
		PinAnnual: getenv("STRIPE_PRICE_PINHOT_ANNUAL"), PinMonthly: getenv("STRIPE_PRICE_PINHOT_MONTHLY"),
	}
}

// Complete reports whether every id is set.
func (p HousePriceIDs) Complete() bool {
	for _, id := range p.all() {
		if id.id == "" {
			return false
		}
	}
	return true
}

type priceRef struct {
	line   Line
	period Period
	id     string
}

func (p HousePriceIDs) all() []priceRef {
	return []priceRef{
		{LineStandard, PeriodAnnual, p.StdAnnual}, {LineStandard, PeriodMonthly, p.StdMonthly},
		{LineVault, PeriodAnnual, p.VaultAnnual}, {LineVault, PeriodMonthly, p.VaultMonthly},
		{LinePinHot, PeriodAnnual, p.PinAnnual}, {LinePinHot, PeriodMonthly, p.PinMonthly},
	}
}

// For returns the price id of a line in a period ("" when unset).
func (p HousePriceIDs) For(line Line, period Period) string {
	for _, r := range p.all() {
		if r.line == line && r.period == period {
			return r.id
		}
	}
	return ""
}

// Lookup maps a Stripe price id back to its line and period.
func (p HousePriceIDs) Lookup(priceID string) (Line, Period, bool) {
	if priceID == "" {
		return "", "", false
	}
	for _, r := range p.all() {
		if r.id == priceID {
			return r.line, r.period, true
		}
	}
	return "", "", false
}

// HouseOrder is what the customer asked for: whole TB per line and a period
// — payments.Order, the shape the entitlement record holds (WP-PAY-1).
type HouseOrder = payments.Order

// ValidateHouseOrder, Quote and QuoteHouse are payments' (pricing.go).
var (
	ValidateHouseOrder = payments.ValidateHouseOrder
	QuoteHouse         = payments.QuoteHouse
)

type (
	Quote     = payments.Quote
	QuoteLine = payments.QuoteLine
)

func cents(usd float64) int64 { return payments.Cents(usd) }

func unitCents(line Line, period Period) int64 { return payments.UnitCents(line, period) }

// --- Verification against prices.json ---

// priceFetcher is price.Get behind an interface for tests.
type priceFetcher interface {
	Get(ctx context.Context, id string) (*stripe.Price, error)
}

type stripePrices struct{}

func (stripePrices) Get(ctx context.Context, id string) (*stripe.Price, error) {
	params := &stripe.PriceParams{}
	params.Context = ctx
	params.AddExpand("tiers")
	return price.Get(id, params)
}

// expected is the Stripe shape a house price must have.
type expected struct {
	interval  string
	unitCents int64 // per-unit prices
	tiers     []stripe.PriceTier
}

func expectedShape(line Line, period Period) expected {
	if line == LineVault && period == PeriodMonthly {
		return expected{interval: "month", tiers: []stripe.PriceTier{
			{UpTo: 1, FlatAmount: cents(landing.Get().Vault.MonthlyMinimum), UnitAmount: 0},
			{UpTo: 0, UnitAmount: unitCents(LineVault, PeriodMonthly)},
		}}
	}
	if period == PeriodMonthly {
		return expected{interval: "month", unitCents: unitCents(line, period)}
	}
	return expected{interval: "year", unitCents: unitCents(line, period) * 12}
}

func checkPrice(p *stripe.Price, id string, want expected) error {
	if !p.Active {
		return fmt.Errorf("%s is not active", id)
	}
	if p.Currency != "usd" {
		return fmt.Errorf("%s is in %s, want usd", id, p.Currency)
	}
	if p.Recurring == nil || string(p.Recurring.Interval) != want.interval || (p.Recurring.IntervalCount != 0 && p.Recurring.IntervalCount != 1) {
		return fmt.Errorf("%s must recur every 1 %s", id, want.interval)
	}
	if want.tiers == nil {
		if p.BillingScheme != stripe.PriceBillingSchemePerUnit || p.UnitAmount != want.unitCents {
			return fmt.Errorf("%s is %d cents per unit, want %d (prices.json)", id, p.UnitAmount, want.unitCents)
		}
		return nil
	}
	if p.BillingScheme != stripe.PriceBillingSchemeTiered || p.TiersMode != stripe.PriceTiersModeVolume || len(p.Tiers) != len(want.tiers) {
		return fmt.Errorf("%s must be volume-tiered with %d tiers (monthly minimum from prices.json)", id, len(want.tiers))
	}
	for i, w := range want.tiers {
		g := p.Tiers[i]
		if g.UpTo != w.UpTo || g.FlatAmount != w.FlatAmount || g.UnitAmount != w.UnitAmount {
			return fmt.Errorf("%s tier %d is up_to=%d flat=%d unit=%d, want up_to=%d flat=%d unit=%d (prices.json)",
				id, i+1, g.UpTo, g.FlatAmount, g.UnitAmount, w.UpTo, w.FlatAmount, w.UnitAmount)
		}
	}
	return nil
}

// ErrPricesUnreachable: a verification that could not REACH Stripe (a
// network error, a timeout, a 5xx, a 429) — no verdict on the prices, the
// previous one is kept (RP22-5). A Stripe 4xx (no such price, a revoked
// key) is a verdict: unready.
var ErrPricesUnreachable = errors.New("stripe not reachable")

// transientPriceError says whether a price fetch failure is Stripe not
// reached (kept verdict) rather than Stripe's answer (a verdict).
func transientPriceError(err error) bool {
	var se *stripe.Error
	if errors.As(err, &se) {
		return se.HTTPStatusCode == 0 || se.HTTPStatusCode >= 500 || se.HTTPStatusCode == http.StatusTooManyRequests
	}
	return true // a dial error, a timeout, a cancelled context
}

// VerifyHousePrices fetches every configured price and asserts it matches
// prices.json. The first mismatch is returned, naming the price id and the
// expected value so the fix is a Stripe dashboard edit, never a code change.
// A fetch that could not reach Stripe wraps ErrPricesUnreachable.
func VerifyHousePrices(ctx context.Context, fetch priceFetcher, ids HousePriceIDs) error {
	if !ids.Complete() {
		return errors.New("house prices: not every STRIPE_PRICE_{STANDARD,VAULT,PINHOT}_{ANNUAL,MONTHLY} is set")
	}
	for _, r := range ids.all() {
		p, err := fetch.Get(ctx, r.id)
		if err != nil {
			if transientPriceError(err) {
				return fmt.Errorf("house prices: fetch %s (%s %s): %w: %w", r.id, r.line, r.period, ErrPricesUnreachable, err)
			}
			return fmt.Errorf("house prices: fetch %s (%s %s): %w", r.id, r.line, r.period, err)
		}
		if err := checkPrice(p, r.id, expectedShape(r.line, r.period)); err != nil {
			return fmt.Errorf("house prices: %s %s: %w", r.line, r.period, err)
		}
	}
	return nil
}

// --- StripeService: house state ---

type houseState struct {
	mu     sync.RWMutex
	ids    HousePriceIDs
	prices priceFetcher
	// decided: a verification has completed with a verdict (verified or
	// not); until then the provider is Pending (RP22-1). A verification
	// that could not reach Stripe decides nothing (RP22-5).
	decided  bool
	verified bool
	err      string
}

// ConfigureHouse installs the price ids (and the price source; nil = the
// Stripe API). Checkout stays closed until VerifyHousePrices succeeds.
func (s *StripeService) ConfigureHouse(ids HousePriceIDs, prices priceFetcher) {
	if prices == nil {
		prices = stripePrices{}
	}
	s.house.mu.Lock()
	defer s.house.mu.Unlock()
	s.house.ids = ids
	s.house.prices = prices
	s.house.decided, s.house.verified = false, false
	s.house.err = "house prices not verified yet"
}

// setPriceSource swaps the price source (tests: a source that fails).
func (s *StripeService) setPriceSource(prices priceFetcher) {
	s.house.mu.Lock()
	defer s.house.mu.Unlock()
	s.house.prices = prices
}

// HousePriceIDs returns the configured ids.
func (s *StripeService) HousePriceIDs() HousePriceIDs {
	s.house.mu.RLock()
	defer s.house.mu.RUnlock()
	return s.house.ids
}

// VerifyHousePrices runs the check against the configured source and
// records the verdict for HouseCheckoutReady / HouseReadiness. A check
// that could not reach Stripe (ErrPricesUnreachable) keeps the previous
// verdict — ready stays ready, pending stays pending — and is logged at
// Warn (RP22-5); only a mismatch, a missing price id or a Stripe 4xx makes
// the provider unready.
func (s *StripeService) VerifyHousePrices(ctx context.Context) error {
	s.house.mu.RLock()
	ids, prices := s.house.ids, s.house.prices
	s.house.mu.RUnlock()
	if prices == nil {
		prices = stripePrices{}
	}
	err := VerifyHousePrices(ctx, prices, ids)
	s.house.mu.Lock()
	defer s.house.mu.Unlock()
	switch {
	case err == nil:
		s.house.decided, s.house.verified, s.house.err = true, true, ""
	case errors.Is(err, ErrPricesUnreachable):
		s.logger.Warn("house prices: Stripe could not be reached — the previous verdict is kept",
			zap.Bool("verified", s.house.verified), zap.Bool("decided", s.house.decided), zap.Error(err))
		if !s.house.decided {
			s.house.err = "house prices not verified yet: " + err.Error()
		}
	default:
		s.house.decided, s.house.verified, s.house.err = true, false, err.Error()
	}
	return err
}

// StartHouseVerification verifies at boot and, until it passes, retries
// every 5 minutes (a Stripe blip at boot must not keep checkout closed for
// the whole process lifetime); once verified it re-checks hourly so a
// price edited in the Stripe dashboard closes checkout within the hour.
// Stripe not reached is retried in 5 minutes too, on the kept verdict.
func (s *StripeService) StartHouseVerification(ctx context.Context) {
	go func() {
		for {
			err := s.VerifyHousePrices(ctx)
			wait := time.Hour
			switch {
			case errors.Is(err, ErrPricesUnreachable):
				wait = 5 * time.Minute // logged at Warn by VerifyHousePrices
			case err != nil:
				s.logger.Error("house checkout DISABLED: Stripe prices do not match prices.json", zap.Error(err))
				wait = 5 * time.Minute
			default:
				s.logger.Info("house prices verified against prices.json; quota checkout ready")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()
}

// HouseCheckoutReady reports whether whole-TB checkout may be offered, and
// why not when it may not (pending is "not yet", with its reason).
func (s *StripeService) HouseCheckoutReady() (bool, string) {
	st, why := s.HouseReadiness()
	return st == payments.Ready, why
}

// HouseReadiness is the three-state answer (D-P20-1): Unready with no
// price ids, after a mismatch or a missing price; Pending until a
// verification has completed; Ready once the prices matched.
func (s *StripeService) HouseReadiness() (payments.Readiness, string) {
	s.house.mu.RLock()
	defer s.house.mu.RUnlock()
	switch {
	case s.house.prices == nil:
		return payments.Unready, "house prices not configured (STRIPE_PRICE_*_ANNUAL/_MONTHLY)"
	case !s.house.decided:
		return payments.Pending, s.house.err
	case s.house.verified:
		return payments.Ready, ""
	default:
		return payments.Unready, s.house.err
	}
}

// --- Checkout ---

func (s *StripeService) houseLineItems(ids HousePriceIDs, o HouseOrder) []*stripe.CheckoutSessionLineItemParams {
	var items []*stripe.CheckoutSessionLineItemParams
	for _, l := range []struct {
		line Line
		tb   int
	}{{LineStandard, o.Std}, {LineVault, o.Vault}, {LinePinHot, o.PinHot}} {
		if l.tb > 0 {
			items = append(items, &stripe.CheckoutSessionLineItemParams{
				Price: stripe.String(ids.For(l.line, o.Period)), Quantity: stripe.Int64(int64(l.tb))})
		}
	}
	return items
}

func (s *StripeService) houseCheckoutParams(customerID, tenantID string, o HouseOrder, purpose string, startAt time.Time, successURL, cancelURL string) *stripe.CheckoutSessionParams {
	meta := map[string]string{"tenant_id": tenantID, "house": "1"}
	if purpose != "" {
		meta["purpose"] = purpose // the ledger's purpose (WP-PAY-2)
	}
	params := &stripe.CheckoutSessionParams{
		Customer:            stripe.String(customerID),
		Mode:                stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems:           s.houseLineItems(s.HousePriceIDs(), o),
		SuccessURL:          stripe.String(successURL),
		CancelURL:           stripe.String(cancelURL),
		ClientReferenceID:   stripe.String(tenantID),
		AllowPromotionCodes: stripe.Bool(true),
		SubscriptionData:    &stripe.CheckoutSessionSubscriptionDataParams{Metadata: meta},
	}
	// A renew of a GRANTED prepaid row defers the first charge to the date
	// already paid through (RP24-1, docs/PAYMENTS.md § 4.2): the
	// subscription is `trialing` until then, so the paid remainder is never
	// forfeited and the anniversary is the paid date. Stripe refuses a
	// trial_end under 48 h ahead — inside that the checkout starts now and
	// the page says the remaining days are forfeited.
	if !startAt.IsZero() && time.Until(startAt) >= payments.MinDeferredStart {
		params.SubscriptionData.TrialEnd = stripe.Int64(startAt.UTC().Unix())
	}
	params.Metadata = meta
	return params
}

// houseCheckoutIdempotencyKey keys a Checkout creation on (tenant, order,
// 5-minute window): Stripe returns the SAME session for a repeated request
// with the same key (Idempotent requests: keys are kept 24 h), so a double
// submit of the form cannot open two sessions (Review R10-05).
func houseCheckoutIdempotencyKey(tenantID string, o HouseOrder, now time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("house-checkout|%s|%d|%d|%d|%s|%d",
		tenantID, o.Std, o.Vault, o.PinHot, o.Period, now.Unix()/300)))
	return "house-" + hex.EncodeToString(sum[:16])
}

// HasLiveSubscription reports whether the customer already holds a
// subscription Stripe would keep billing or can resume (active, trialing,
// past_due, incomplete, unpaid, paused — paused since WP-R10-2: no invoice
// while it lasts, but a resume brings it back). A second Checkout for such a
// customer would open a second subscription beside the first (Review R10-05).
func (s *StripeService) HasLiveSubscription(ctx context.Context, customerID string) (bool, error) {
	params := &stripe.SubscriptionListParams{Customer: stripe.String(customerID), Status: stripe.String("all")}
	params.Context = ctx
	params.Limit = stripe.Int64(50)
	it := subscription.List(params)
	for it.Next() {
		switch it.Subscription().Status {
		case stripe.SubscriptionStatusActive, stripe.SubscriptionStatusTrialing, stripe.SubscriptionStatusPastDue,
			stripe.SubscriptionStatusIncomplete, stripe.SubscriptionStatusUnpaid, stripe.SubscriptionStatusPaused:
			return true, nil
		}
	}
	if err := it.Err(); err != nil {
		return false, fmt.Errorf("list subscriptions for %s: %w", customerID, err)
	}
	return false, nil
}

// CreateHouseCheckout starts a Stripe Checkout for a house and returns the
// hosted page URL. URLs must be absolute (Stripe rejects relative ones).
// startAt (zero = now) defers the first charge (payments.Checkout.StartAt).
func (s *StripeService) CreateHouseCheckout(ctx context.Context, customerID, tenantID string, o HouseOrder, purpose string, startAt time.Time, successURL, cancelURL string) (string, error) {
	if ready, why := s.HouseCheckoutReady(); !ready {
		return "", fmt.Errorf("house checkout unavailable: %s", why)
	}
	if err := ValidateHouseOrder(o); err != nil {
		return "", fmt.Errorf("house order: %w", err)
	}
	params := s.houseCheckoutParams(customerID, tenantID, o, purpose, startAt, successURL, cancelURL)
	params.Context = ctx
	params.SetIdempotencyKey(houseCheckoutIdempotencyKey(tenantID, o, time.Now()))
	sess, err := checkoutsession.New(params)
	if err != nil {
		return "", fmt.Errorf("create house checkout session: %w", err)
	}
	return sess.URL, nil
}

// --- Existing subscription → order, and resize ---

// HouseFromSubscription reads a subscription's items back into an order.
// ok is false when no item is a house price (a legacy pack subscription).
func (s *StripeService) HouseFromSubscription(sub *stripe.Subscription) (HouseOrder, bool) {
	if sub == nil || sub.Items == nil {
		return HouseOrder{}, false
	}
	ids := s.HousePriceIDs()
	var o HouseOrder
	found := false
	for _, it := range sub.Items.Data {
		if it == nil || it.Price == nil {
			continue
		}
		line, period, ok := ids.Lookup(it.Price.ID)
		if !ok {
			continue
		}
		found = true
		o.Period = period
		switch line {
		case LineStandard:
			o.Std += int(it.Quantity)
		case LineVault:
			o.Vault += int(it.Quantity)
		case LinePinHot:
			o.PinHot += int(it.Quantity)
		}
	}
	return o, found
}

// houseUpdateParams diffs the current items against the wanted order:
// quantities change in place, a line at 0 is deleted, a new line is added,
// all prorated. The period is fixed for the life of a subscription.
func (s *StripeService) houseUpdateParams(current *stripe.Subscription, o HouseOrder) (*stripe.SubscriptionParams, error) {
	if err := ValidateHouseOrder(o); err != nil {
		return nil, err
	}
	cur, ok := s.HouseFromSubscription(current)
	if !ok {
		return nil, errors.New("the current subscription is not a house")
	}
	if cur.Period != o.Period {
		return nil, fmt.Errorf("the billing period is %s for the life of this subscription", cur.Period)
	}
	ids := s.HousePriceIDs()
	want := map[Line]int{LineStandard: o.Std, LineVault: o.Vault, LinePinHot: o.PinHot}
	params := &stripe.SubscriptionParams{ProrationBehavior: stripe.String("create_prorations")}
	seen := map[Line]bool{}
	for _, it := range current.Items.Data {
		if it == nil || it.Price == nil {
			continue
		}
		line, _, known := ids.Lookup(it.Price.ID)
		if !known {
			continue
		}
		seen[line] = true
		if tb := want[line]; tb > 0 {
			params.Items = append(params.Items, &stripe.SubscriptionItemsParams{ID: stripe.String(it.ID), Quantity: stripe.Int64(int64(tb))})
		} else {
			params.Items = append(params.Items, &stripe.SubscriptionItemsParams{ID: stripe.String(it.ID), Deleted: stripe.Bool(true)})
		}
	}
	for _, line := range []Line{LineStandard, LineVault, LinePinHot} {
		if !seen[line] && want[line] > 0 {
			params.Items = append(params.Items, &stripe.SubscriptionItemsParams{
				Price: stripe.String(ids.For(line, o.Period)), Quantity: stripe.Int64(int64(want[line]))})
		}
	}
	return params, nil
}

// UpdateHouseSubscription resizes a house in place (prorated) and returns
// the updated subscription; the webhook applies the quotas, and the caller
// may apply them immediately from the returned object.
func (s *StripeService) UpdateHouseSubscription(ctx context.Context, subID string, o HouseOrder) (*stripe.Subscription, error) {
	if ready, why := s.HouseCheckoutReady(); !ready {
		return nil, fmt.Errorf("house checkout unavailable: %s", why)
	}
	current, err := s.subs().Get(ctx, subID)
	if err != nil {
		return nil, fmt.Errorf("fetch subscription %s: %w", subID, err)
	}
	params, err := s.houseUpdateParams(current, o)
	if err != nil {
		return nil, fmt.Errorf("resize house: %w", err)
	}
	params.Context = ctx
	sub, err := subscription.Update(subID, params)
	if err != nil {
		return nil, fmt.Errorf("update subscription %s: %w", subID, err)
	}
	return sub, nil
}

// subscriptionFetcher is subscription.Get behind an interface for tests.
type subscriptionFetcher interface {
	Get(ctx context.Context, id string) (*stripe.Subscription, error)
}

type stripeSubscriptions struct{}

func (stripeSubscriptions) Get(ctx context.Context, id string) (*stripe.Subscription, error) {
	params := &stripe.SubscriptionParams{}
	params.Context = ctx
	return subscription.Get(id, params)
}

func (s *StripeService) subs() subscriptionFetcher {
	s.house.mu.RLock()
	defer s.house.mu.RUnlock()
	if s.subFetcher != nil {
		return s.subFetcher
	}
	return stripeSubscriptions{}
}

// SubscriptionPeriodEnd is the end of a subscription's current period as
// Stripe has it now — the paid_through the entitlement backfill records for
// a house granted before migration 097 (WP-PAY-1) — and whether its renewal
// was stopped there (cancel_at_period_end: the backfill records renewal
// prepaid, RP20-5).
func (s *StripeService) SubscriptionPeriodEnd(ctx context.Context, subscriptionID string) (end time.Time, cancelAtPeriodEnd bool, err error) {
	sub, err := s.subs().Get(ctx, subscriptionID)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("fetch subscription %s: %w", subscriptionID, err)
	}
	if sub.CurrentPeriodEnd <= 0 {
		return time.Time{}, false, fmt.Errorf("subscription %s carries no period", subscriptionID)
	}
	return time.Unix(sub.CurrentPeriodEnd, 0).UTC(), sub.CancelAtPeriodEnd, nil
}
