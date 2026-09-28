package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/api/landing"
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

// Line identifies a subscription line; Period its billing interval.
type (
	Line   string
	Period string
)

const (
	LineStandard Line = "standard"
	LineVault    Line = "vault"
	LinePinHot   Line = "pin_hot"

	PeriodAnnual  Period = "annual"
	PeriodMonthly Period = "monthly"
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

// HouseOrder is what the customer asked for: whole TB per line and a period.
type HouseOrder struct {
	Std, Vault, PinHot int
	Period             Period
}

// ValidateHouseOrder applies the product rules: at least one floor, the
// builder's bound per floor, pin-hot only on (and within) downstairs, a
// known period. Attic-only houses are sold (decision 2026-09-27).
func ValidateHouseOrder(o HouseOrder) error {
	if o.Period != PeriodAnnual && o.Period != PeriodMonthly {
		return fmt.Errorf("unknown billing period %q", o.Period)
	}
	if o.Std < 0 || o.Vault < 0 || o.PinHot < 0 {
		return errors.New("a floor cannot be negative")
	}
	if o.Std > landing.MaxIntentTB || o.Vault > landing.MaxIntentTB {
		return fmt.Errorf("a floor holds at most %d TB", landing.MaxIntentTB)
	}
	if o.Std == 0 && o.Vault == 0 {
		return errors.New("the house is empty: add at least one box")
	}
	if o.PinHot > o.Std {
		return errors.New("pin-hot covers downstairs only, so it cannot exceed the downstairs TB")
	}
	return nil
}

// --- Quote (what the page prints; what Stripe charges) ---

// QuoteLine is one receipt line.
type QuoteLine struct {
	Label  string // "Downstairs", "Attic", "Pin-hot"
	TB     int
	Cents  int64 // per month
	Detail string
}

// Quote is a house priced from prices.json.
type Quote struct {
	Lines        []QuoteLine
	MonthlyCents int64 // per month, what the site's receipt shows
	ChargeCents  int64 // per billing period (×12 when annual)
	Period       Period
}

func money(cents int64) string { return fmt.Sprintf("$%d.%02d", cents/100, cents%100) }

// Money formats the line's per-month amount ("$26.94").
func (l QuoteLine) Money() string { return money(l.Cents) }

// Monthly formats the per-month total.
func (q Quote) Monthly() string { return money(q.MonthlyCents) }

// Charge formats the per-period charge.
func (q Quote) Charge() string { return money(q.ChargeCents) }

func cents(usd float64) int64 { return int64(math.Round(usd * 100)) }

// unitCents returns the per-TB-per-month unit for a line/period in whole
// cents, as Stripe stores it — so the sum of lines is exactly the invoice.
func unitCents(line Line, period Period) int64 {
	p := landing.Get()
	switch line {
	case LineStandard:
		if period == PeriodMonthly {
			return cents(p.Standard.Monthly)
		}
		return cents(p.Standard.Annual)
	case LineVault:
		if period == PeriodMonthly {
			return cents(p.Vault.Monthly)
		}
		return cents(p.Vault.Annual)
	case LinePinHot:
		return cents(p.PinHot)
	}
	return 0
}

// vaultMonthlyCents applies the site's "$4.99 monthly minimum" to the
// attic line: it is a volume-tiered price in Stripe (1 TB = the minimum
// flat, 2+ TB = per unit), so the maths here mirrors that shape.
func vaultMonthlyCents(tb int) int64 {
	if tb <= 0 {
		return 0
	}
	if tb == 1 {
		return cents(landing.Get().Vault.MonthlyMinimum)
	}
	return int64(tb) * unitCents(LineVault, PeriodMonthly)
}

// QuoteHouse prices an order. Annual rates are per TB per month billed
// yearly (ChargeCents = 12 × monthly); monthly is charged as shown.
func QuoteHouse(std, vault, pinHot int, period Period) Quote {
	q := Quote{Period: period}
	add := func(label string, tb int, c int64, detail string) {
		if tb <= 0 {
			return
		}
		q.Lines = append(q.Lines, QuoteLine{Label: label, TB: tb, Cents: c, Detail: detail})
		q.MonthlyCents += c
	}
	add("Downstairs", std, int64(std)*unitCents(LineStandard, period), "Standard")
	if period == PeriodMonthly {
		add("Attic", vault, vaultMonthlyCents(vault), "Vault, back in minutes")
	} else {
		add("Attic", vault, int64(vault)*unitCents(LineVault, period), "Vault, back in minutes")
	}
	add("Pin-hot", pinHot, int64(pinHot)*unitCents(LinePinHot, period), "never moves to tape")
	q.ChargeCents = q.MonthlyCents
	if period == PeriodAnnual {
		q.ChargeCents = q.MonthlyCents * 12
	}
	return q
}

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

// VerifyHousePrices fetches every configured price and asserts it matches
// prices.json. The first mismatch is returned, naming the price id and the
// expected value so the fix is a Stripe dashboard edit, never a code change.
func VerifyHousePrices(ctx context.Context, fetch priceFetcher, ids HousePriceIDs) error {
	if !ids.Complete() {
		return errors.New("house prices: not every STRIPE_PRICE_{STANDARD,VAULT,PINHOT}_{ANNUAL,MONTHLY} is set")
	}
	for _, r := range ids.all() {
		p, err := fetch.Get(ctx, r.id)
		if err != nil {
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
	mu       sync.RWMutex
	ids      HousePriceIDs
	prices   priceFetcher
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
	s.house.verified = false
	s.house.err = "house prices not verified yet"
}

// HousePriceIDs returns the configured ids.
func (s *StripeService) HousePriceIDs() HousePriceIDs {
	s.house.mu.RLock()
	defer s.house.mu.RUnlock()
	return s.house.ids
}

// VerifyHousePrices runs the check against the configured source and
// records the outcome for HouseCheckoutReady.
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
	if err != nil {
		s.house.verified, s.house.err = false, err.Error()
		return err
	}
	s.house.verified, s.house.err = true, ""
	return nil
}

// StartHouseVerification verifies at boot and, until it passes, retries
// every 5 minutes (a Stripe blip at boot must not keep checkout closed for
// the whole process lifetime); once verified it re-checks hourly so a
// price edited in the Stripe dashboard closes checkout within the hour.
func (s *StripeService) StartHouseVerification(ctx context.Context) {
	go func() {
		for {
			err := s.VerifyHousePrices(ctx)
			wait := time.Hour
			if err != nil {
				s.logger.Error("house checkout DISABLED: Stripe prices do not match prices.json", zap.Error(err))
				wait = 5 * time.Minute
			} else {
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
// why not when it may not.
func (s *StripeService) HouseCheckoutReady() (bool, string) {
	s.house.mu.RLock()
	defer s.house.mu.RUnlock()
	if s.house.prices == nil {
		return false, "house prices not configured (STRIPE_PRICE_*_ANNUAL/_MONTHLY)"
	}
	return s.house.verified, s.house.err
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

func (s *StripeService) houseCheckoutParams(customerID, tenantID string, o HouseOrder, successURL, cancelURL string) *stripe.CheckoutSessionParams {
	meta := map[string]string{"tenant_id": tenantID, "house": "1"}
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
// subscription Stripe would keep billing (active, trialing, past_due,
// incomplete, unpaid). A second Checkout for such a customer would open a
// second subscription while the first keeps invoicing (Review R10-05).
func (s *StripeService) HasLiveSubscription(ctx context.Context, customerID string) (bool, error) {
	params := &stripe.SubscriptionListParams{Customer: stripe.String(customerID), Status: stripe.String("all")}
	params.Context = ctx
	params.Limit = stripe.Int64(50)
	it := subscription.List(params)
	for it.Next() {
		switch it.Subscription().Status {
		case stripe.SubscriptionStatusActive, stripe.SubscriptionStatusTrialing, stripe.SubscriptionStatusPastDue,
			stripe.SubscriptionStatusIncomplete, stripe.SubscriptionStatusUnpaid:
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
func (s *StripeService) CreateHouseCheckout(ctx context.Context, customerID, tenantID string, o HouseOrder, successURL, cancelURL string) (string, error) {
	if ready, why := s.HouseCheckoutReady(); !ready {
		return "", fmt.Errorf("house checkout unavailable: %s", why)
	}
	if err := ValidateHouseOrder(o); err != nil {
		return "", fmt.Errorf("house order: %w", err)
	}
	params := s.houseCheckoutParams(customerID, tenantID, o, successURL, cancelURL)
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
