package billing

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/FairForge/vaultaire/internal/usage"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

// Whole-TB house checkout (dashboard plan, Phase 1). Prices come from
// internal/api/landing/prices.json only; Stripe price ids come from env and
// are asserted against the file before checkout is offered.

var testIDs = HousePriceIDs{
	StdAnnual: "price_std_y", StdMonthly: "price_std_m",
	VaultAnnual: "price_vault_y", VaultMonthly: "price_vault_m",
	PinAnnual: "price_pin_y", PinMonthly: "price_pin_m",
}

func TestHousePriceIDs_FromEnvAndLookup(t *testing.T) {
	env := map[string]string{
		"STRIPE_PRICE_STANDARD_ANNUAL": "price_std_y", "STRIPE_PRICE_STANDARD_MONTHLY": "price_std_m",
		"STRIPE_PRICE_VAULT_ANNUAL": "price_vault_y", "STRIPE_PRICE_VAULT_MONTHLY": "price_vault_m",
		"STRIPE_PRICE_PINHOT_ANNUAL": "price_pin_y", "STRIPE_PRICE_PINHOT_MONTHLY": "price_pin_m",
	}
	ids := HousePriceIDsFromEnv(func(k string) string { return env[k] })
	assert.Equal(t, testIDs, ids)
	assert.True(t, ids.Complete())
	assert.False(t, HousePriceIDs{StdAnnual: "x"}.Complete())

	line, period, ok := ids.Lookup("price_vault_m")
	require.True(t, ok)
	assert.Equal(t, LineVault, line)
	assert.Equal(t, PeriodMonthly, period)
	_, _, ok = ids.Lookup("price_other")
	assert.False(t, ok)
	assert.Equal(t, "price_pin_y", ids.For(LinePinHot, PeriodAnnual))
}

// The quote is what the billing page prints and what Stripe will charge:
// annual rates are per TB per month billed yearly; the Vault monthly price
// carries the $4.99 minimum the site states.
func TestQuoteHouse_MatchesThePriceFile(t *testing.T) {
	q := QuoteHouse(6, 1, 0, PeriodAnnual)
	assert.Equal(t, int64(2894), q.MonthlyCents, "6×4.49 + 1×2.00 = $28.94/mo, the receipt on the site")
	assert.Equal(t, int64(2894*12), q.ChargeCents, "billed yearly")
	assert.Equal(t, "$28.94", q.Monthly())
	assert.Equal(t, "$347.28", q.Charge())

	q = QuoteHouse(6, 1, 2, PeriodMonthly)
	assert.Equal(t, int64(6*499+499+2*300), q.MonthlyCents, "1 TB attic monthly hits the $4.99 minimum; pin-hot $3/TB")
	assert.Equal(t, q.MonthlyCents, q.ChargeCents)

	q = QuoteHouse(0, 2, 0, PeriodMonthly)
	assert.Equal(t, int64(510), q.MonthlyCents, "2 TB attic monthly = 2×2.55, above the minimum")

	q = QuoteHouse(0, 3, 0, PeriodAnnual)
	assert.Equal(t, int64(600), q.MonthlyCents, "attic-only houses are a real product")
	assert.Len(t, q.Lines, 1)
	assert.Equal(t, "Attic", q.Lines[0].Label)
}

func TestValidateHouseOrder(t *testing.T) {
	assert.NoError(t, ValidateHouseOrder(HouseOrder{Std: 1, Period: PeriodAnnual}))
	assert.NoError(t, ValidateHouseOrder(HouseOrder{Vault: 1, Period: PeriodMonthly}), "attic-only is allowed")
	assert.Error(t, ValidateHouseOrder(HouseOrder{Period: PeriodAnnual}), "an empty house")
	assert.Error(t, ValidateHouseOrder(HouseOrder{Std: 1, Period: "weekly"}))
	assert.Error(t, ValidateHouseOrder(HouseOrder{Std: 1, PinHot: 2, Period: PeriodAnnual}), "pin-hot beyond downstairs")
	assert.Error(t, ValidateHouseOrder(HouseOrder{Vault: 1, PinHot: 1, Period: PeriodAnnual}), "pin-hot needs a downstairs")
	assert.Error(t, ValidateHouseOrder(HouseOrder{Std: 301, Period: PeriodAnnual}), "above the builder's bound")
	assert.Error(t, ValidateHouseOrder(HouseOrder{Std: -1, Vault: 2, Period: PeriodAnnual}))
}

// fakePrices answers price.Get for the verification test.
type fakePrices map[string]*stripe.Price

func (f fakePrices) Get(_ context.Context, id string) (*stripe.Price, error) {
	p, ok := f[id]
	if !ok {
		return nil, errors.New("No such price: " + id)
	}
	return p, nil
}

func perUnit(cents int64, interval string) *stripe.Price {
	return &stripe.Price{Active: true, Currency: "usd", BillingScheme: stripe.PriceBillingSchemePerUnit,
		UnitAmount: cents, Recurring: &stripe.PriceRecurring{Interval: stripe.PriceRecurringInterval(interval), IntervalCount: 1}}
}

func goodPrices() fakePrices {
	vaultMonthly := &stripe.Price{Active: true, Currency: "usd", BillingScheme: stripe.PriceBillingSchemeTiered,
		TiersMode: stripe.PriceTiersModeVolume,
		Recurring: &stripe.PriceRecurring{Interval: "month", IntervalCount: 1},
		Tiers:     []*stripe.PriceTier{{UpTo: 1, FlatAmount: 499, UnitAmount: 0}, {UpTo: 0, UnitAmount: 255}}}
	return fakePrices{
		"price_std_y": perUnit(5388, "year"), "price_std_m": perUnit(499, "month"),
		"price_vault_y": perUnit(2400, "year"), "price_vault_m": vaultMonthly,
		"price_pin_y": perUnit(3600, "year"), "price_pin_m": perUnit(300, "month"),
	}
}

func TestVerifyHousePrices(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, VerifyHousePrices(ctx, goodPrices(), testIDs))

	bad := goodPrices()
	bad["price_std_y"] = perUnit(4788, "year") // $3.99 — the old metered rate
	err := VerifyHousePrices(ctx, bad, testIDs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "price_std_y")
	assert.Contains(t, err.Error(), "5388")

	bad = goodPrices()
	bad["price_vault_m"] = perUnit(255, "month") // no minimum tier
	err = VerifyHousePrices(ctx, bad, testIDs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "price_vault_m")

	bad = goodPrices()
	bad["price_pin_m"].Currency = "eur"
	assert.Error(t, VerifyHousePrices(ctx, bad, testIDs))

	bad = goodPrices()
	delete(bad, "price_pin_y")
	assert.Error(t, VerifyHousePrices(ctx, bad, testIDs), "a missing price disables checkout")

	assert.Error(t, VerifyHousePrices(ctx, goodPrices(), HousePriceIDs{}), "unconfigured ids")
}

func TestStripeService_HouseReadiness(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	ready, why := svc.HouseCheckoutReady()
	assert.False(t, ready)
	assert.NotEmpty(t, why)

	svc.ConfigureHouse(testIDs, goodPrices())
	ready, _ = svc.HouseCheckoutReady()
	assert.False(t, ready, "not until the prices are verified")
	require.NoError(t, svc.VerifyHousePrices(context.Background()))
	ready, _ = svc.HouseCheckoutReady()
	assert.True(t, ready)

	// A later mismatch (someone edits the Stripe price) takes checkout down.
	svc.ConfigureHouse(testIDs, fakePrices{})
	assert.Error(t, svc.VerifyHousePrices(context.Background()))
	ready, why = svc.HouseCheckoutReady()
	assert.False(t, ready)
	assert.Contains(t, why, "price_std_y")
}

func TestHouseCheckoutParams(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	svc.ConfigureHouse(testIDs, goodPrices())
	params := svc.houseCheckoutParams("cus_1", "tenant-1", HouseOrder{Std: 6, Vault: 1, PinHot: 2, Period: PeriodMonthly},
		"https://stored.ge/dashboard/billing?upgraded=1", "https://stored.ge/dashboard/billing")

	require.Len(t, params.LineItems, 3)
	assert.Equal(t, "price_std_m", *params.LineItems[0].Price)
	assert.Equal(t, int64(6), *params.LineItems[0].Quantity)
	assert.Equal(t, "price_vault_m", *params.LineItems[1].Price)
	assert.Equal(t, int64(1), *params.LineItems[1].Quantity)
	assert.Equal(t, "price_pin_m", *params.LineItems[2].Price)
	assert.Equal(t, int64(2), *params.LineItems[2].Quantity)
	assert.Equal(t, "subscription", *params.Mode)
	assert.Equal(t, "cus_1", *params.Customer)
	assert.Equal(t, "tenant-1", *params.ClientReferenceID)
	assert.True(t, *params.AllowPromotionCodes, "LET coupons")
	assert.Equal(t, "1", params.SubscriptionData.Metadata["house"])
	assert.Equal(t, "tenant-1", params.SubscriptionData.Metadata["tenant_id"])
	assert.Equal(t, "https://stored.ge/dashboard/billing?upgraded=1", *params.SuccessURL)

	// Attic-only: one line.
	params = svc.houseCheckoutParams("cus_1", "tenant-1", HouseOrder{Vault: 2, Period: PeriodAnnual}, "https://x/ok", "https://x/no")
	require.Len(t, params.LineItems, 1)
	assert.Equal(t, "price_vault_y", *params.LineItems[0].Price)
}

func subWith(id, status string, items ...*stripe.SubscriptionItem) *stripe.Subscription {
	return &stripe.Subscription{ID: id, Status: stripe.SubscriptionStatus(status),
		Customer: &stripe.Customer{ID: "cus_h"},
		Items:    &stripe.SubscriptionItemList{Data: items}}
}

func item(itemID, priceID string, qty int64) *stripe.SubscriptionItem {
	return &stripe.SubscriptionItem{ID: itemID, Price: &stripe.Price{ID: priceID}, Quantity: qty}
}

func TestHouseFromSubscription(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	svc.ConfigureHouse(testIDs, goodPrices())

	order, ok := svc.HouseFromSubscription(subWith("sub_1", "active",
		item("si_1", "price_std_y", 6), item("si_2", "price_vault_y", 1), item("si_3", "price_pin_y", 1)))
	require.True(t, ok)
	assert.Equal(t, HouseOrder{Std: 6, Vault: 1, PinHot: 1, Period: PeriodAnnual}, order)

	_, ok = svc.HouseFromSubscription(subWith("sub_old", "active", item("si_9", "price_vault18_pack", 1)))
	assert.False(t, ok, "a legacy pack subscription is not a house")

	_, ok = svc.HouseFromSubscription(&stripe.Subscription{ID: "sub_empty"})
	assert.False(t, ok)
}

func TestHouseUpdateParams(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	svc.ConfigureHouse(testIDs, goodPrices())
	current := subWith("sub_1", "active", item("si_std", "price_std_y", 6), item("si_vault", "price_vault_y", 1))

	params, err := svc.houseUpdateParams(current, HouseOrder{Std: 8, Vault: 0, PinHot: 1, Period: PeriodAnnual})
	require.NoError(t, err)
	assert.Equal(t, "create_prorations", *params.ProrationBehavior)
	require.Len(t, params.Items, 3)
	assert.Equal(t, "si_std", *params.Items[0].ID)
	assert.Equal(t, int64(8), *params.Items[0].Quantity)
	assert.Equal(t, "si_vault", *params.Items[1].ID)
	assert.True(t, *params.Items[1].Deleted, "a floor at 0 TB leaves the subscription; data is untouched")
	assert.Equal(t, "price_pin_y", *params.Items[2].Price)
	assert.Equal(t, int64(1), *params.Items[2].Quantity)

	_, err = svc.houseUpdateParams(current, HouseOrder{Std: 8, Period: PeriodMonthly})
	assert.Error(t, err, "the period cannot change on an existing subscription")
}

// --- Webhook → floor quotas (DB-backed) ---

type fakeSubs map[string]*stripe.Subscription

func (f fakeSubs) Get(_ context.Context, id string) (*stripe.Subscription, error) {
	s, ok := f[id]
	if !ok {
		return nil, errors.New("No such subscription: " + id)
	}
	return s, nil
}

type houseFixture struct {
	db       *sql.DB
	qm       *usage.QuotaManager
	handler  *WebhookHandler
	subs     fakeSubs
	tenantID string
}

func setupHouseFixture(t *testing.T) *houseFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tenantID := fmt.Sprintf("house-wh-%d", time.Now().UnixNano())
	_, err = db.Exec(`INSERT INTO tenants (id, name, email, stripe_customer_id) VALUES ($1, 'House', $2, $3)`,
		tenantID, tenantID+"@test.local", "cus_"+tenantID)
	require.NoError(t, err)
	qm := usage.NewQuotaManager(db)
	require.NoError(t, qm.CreateTenant(context.Background(), tenantID, "free", usage.FreeTierLimits.StorageBytes))
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM stripe_events WHERE event_id LIKE $1`, "evt_"+tenantID+"%")
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
	})

	svc := NewStripeService("sk_test_fake", db, zap.NewNop())
	svc.ConfigureHouse(testIDs, goodPrices())
	subs := fakeSubs{}
	h := NewWebhookHandler("", svc, zap.NewNop())
	h.SetHouseQuotas(qm)
	h.subs = subs
	return &houseFixture{db: db, qm: qm, handler: h, subs: subs, tenantID: tenantID}
}

func (f *houseFixture) post(t *testing.T, suffix, typ string, obj any) {
	t.Helper()
	raw, err := json.Marshal(obj)
	require.NoError(t, err)
	ev := stripe.Event{ID: "evt_" + f.tenantID + "_" + suffix, Type: stripe.EventType(typ),
		Data: &stripe.EventData{Raw: raw}}
	body, err := json.Marshal(ev)
	require.NoError(t, err)
	req := httptest.NewRequest("POST", "/webhook/stripe", bytes.NewReader(body))
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
}

func (f *houseFixture) tenantRow(t *testing.T) (plan, status, subID, period string) {
	t.Helper()
	var sub sql.NullString
	require.NoError(t, f.db.QueryRow(
		`SELECT plan, subscription_status, stripe_subscription_id, house_period FROM tenants WHERE id = $1`,
		f.tenantID).Scan(&plan, &status, &sub, &period))
	return plan, status, sub.String, period
}

func TestWebhook_CheckoutCompletedSetsFloorQuotas(t *testing.T) {
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active",
		item("si_1", "price_std_y", 6), item("si_2", "price_vault_y", 1), item("si_3", "price_pin_y", 1))
	sub.Customer.ID = cus
	f.subs[sub.ID] = sub

	f.post(t, "checkout", "checkout.session.completed", stripe.CheckoutSession{
		ID: "cs_1", Customer: &stripe.Customer{ID: cus}, Subscription: &stripe.Subscription{ID: sub.ID}})

	house, ok, err := f.qm.GetHouse(context.Background(), f.tenantID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, usage.HouseFromTB(6, 1, 1), house)
	used, limit, err := f.qm.GetUsage(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.Equal(t, int64(7*usage.TB), limit)
	assert.Equal(t, int64(0), used)
	plan, status, subID, period := f.tenantRow(t)
	assert.Equal(t, "house", plan)
	assert.Equal(t, "active", status)
	assert.Equal(t, sub.ID, subID)
	assert.Equal(t, "annual", period)

	// Replay (Stripe retries) is a no-op.
	f.post(t, "checkout", "checkout.session.completed", stripe.CheckoutSession{
		ID: "cs_1", Customer: &stripe.Customer{ID: cus}, Subscription: &stripe.Subscription{ID: sub.ID}})
	house, _, _ = f.qm.GetHouse(context.Background(), f.tenantID)
	assert.Equal(t, usage.HouseFromTB(6, 1, 1), house)
}

func TestWebhook_SubscriptionUpdatedResizes(t *testing.T) {
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_std_m", 2), item("si_2", "price_vault_m", 1))
	sub.Customer.ID = cus
	f.post(t, "created", "customer.subscription.created", sub)
	house, ok, _ := f.qm.GetHouse(context.Background(), f.tenantID)
	require.True(t, ok)
	assert.Equal(t, usage.HouseFromTB(2, 1, 0), house)
	_, _, _, period := f.tenantRow(t)
	assert.Equal(t, "monthly", period)

	// Resize: the attic goes away, downstairs grows.
	sub = subWith(sub.ID, "active", item("si_1", "price_std_m", 5))
	sub.Customer.ID = cus
	f.post(t, "updated", "customer.subscription.updated", sub)
	house, _, _ = f.qm.GetHouse(context.Background(), f.tenantID)
	assert.Equal(t, usage.HouseFromTB(5, 0, 0), house)
	floors, _ := f.qm.GetFloors(context.Background(), f.tenantID)
	require.Len(t, floors, 2, "both floor rows stay; the attic is simply 0")
	assert.Equal(t, int64(0), floors[1].LimitBytes)

	// Past due keeps the house (grace); unpaid clears it.
	sub.Status = "past_due"
	f.post(t, "pastdue", "customer.subscription.updated", sub)
	_, ok, _ = f.qm.GetHouse(context.Background(), f.tenantID)
	assert.True(t, ok)
	sub.Status = "unpaid"
	f.post(t, "unpaid", "customer.subscription.updated", sub)
	_, ok, _ = f.qm.GetHouse(context.Background(), f.tenantID)
	assert.False(t, ok, "an unpaid subscription grants nothing")
	_, _, limit, _ := f.tenantRow(t)
	_ = limit
	_, lim, _ := f.qm.GetUsage(context.Background(), f.tenantID)
	assert.Equal(t, usage.FreeTierLimits.StorageBytes, lim)
}

func TestWebhook_SubscriptionDeletedClearsOnlyTheCurrentHouse(t *testing.T) {
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_std_y", 3))
	sub.Customer.ID = cus
	f.post(t, "created", "customer.subscription.created", sub)
	_, ok, _ := f.qm.GetHouse(context.Background(), f.tenantID)
	require.True(t, ok)

	// A stale, different subscription being deleted must not touch the house.
	old := subWith("sub_old_"+f.tenantID, "canceled", item("si_x", "price_std_y", 1))
	old.Customer.ID = cus
	f.post(t, "delold", "customer.subscription.deleted", old)
	_, ok, _ = f.qm.GetHouse(context.Background(), f.tenantID)
	assert.True(t, ok, "deleting an old subscription leaves the current house alone")

	sub.Status = "canceled"
	f.post(t, "del", "customer.subscription.deleted", sub)
	_, ok, _ = f.qm.GetHouse(context.Background(), f.tenantID)
	assert.False(t, ok)
	plan, status, subID, period := f.tenantRow(t)
	assert.Equal(t, "starter", plan)
	assert.Equal(t, "canceled", status)
	assert.Equal(t, "", subID)
	assert.Equal(t, "", period)
}

func TestWebhook_LegacyPackSubscriptionLeavesQuotasAlone(t *testing.T) {
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_pack_"+f.tenantID, "active", item("si_1", "price_vault18_pack", 1))
	sub.Customer.ID = cus
	f.post(t, "pack", "customer.subscription.updated", sub)
	_, ok, _ := f.qm.GetHouse(context.Background(), f.tenantID)
	assert.False(t, ok)
	_, status, _, _ := f.tenantRow(t)
	assert.Equal(t, "active", status, "the legacy status sync still runs")
}
