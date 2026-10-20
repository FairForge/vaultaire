package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

// fakeCheckoutAPI is Stripe's REST API as far as a house checkout uses it,
// behind stripe-go's own backend (no network): customers, the live-
// subscription list, checkout sessions.
type fakeCheckoutAPI struct {
	customers atomic.Int32
	sessions  atomic.Int32
	live      bool
}

func (f *fakeCheckoutAPI) install(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/customers":
			f.customers.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "cus_fake", "object": "customer"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions":
			data := []any{}
			if f.live {
				data = append(data, map[string]any{"id": "sub_live", "object": "subscription", "status": "active"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "has_more": false, "url": "/v1/subscriptions"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
			f.sessions.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "cs_fake", "object": "checkout.session", "url": "https://checkout.stripe.test/cs_fake"})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"fake: ` + r.Method + ` ` + r.URL.Path + `"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	prev := stripe.GetBackend(stripe.APIBackend)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0), LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull},
	}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prev) })
}

func readyStripe(t *testing.T, f *houseFixture) *StripeService {
	t.Helper()
	svc := NewStripeService("sk_test_fake", f.db, zap.NewNop())
	svc.ConfigureHouse(testIDs, goodPrices())
	require.NoError(t, svc.VerifyHousePrices(context.Background()))
	return svc
}

func checkoutFor(tenantID string) payments.Checkout {
	return payments.Checkout{TenantID: tenantID, Email: tenantID + "@test.local", Purpose: payments.PurposeNew,
		Order:     payments.Order{Std: 2, Period: payments.PeriodAnnual},
		ReturnURL: "https://stored.test/dashboard/billing?upgraded=1", CancelURL: "https://stored.test/dashboard/billing"}
}

// The Stripe customer is created at the first checkout, recorded, and
// reused by the next one — never at registration (43.2).
func TestCheckout_LazyCustomer_CreatedOnce(t *testing.T) {
	// Arrange: a tenant that never bought (no customer id).
	f := setupHouseFixture(t)
	_, err := f.db.Exec(`UPDATE tenants SET stripe_customer_id = NULL WHERE id = $1`, f.tenantID)
	require.NoError(t, err)
	api := &fakeCheckoutAPI{}
	api.install(t)
	svc := readyStripe(t, f)

	// Act: two checkouts.
	for range 2 {
		url, err := svc.StartCheckout(context.Background(), checkoutFor(f.tenantID))
		require.NoError(t, err)
		assert.Equal(t, "https://checkout.stripe.test/cs_fake", url)
	}

	// Assert
	assert.Equal(t, int32(1), api.customers.Load(), "one customer, created at the first checkout")
	assert.Equal(t, int32(2), api.sessions.Load())
	var cus string
	require.NoError(t, f.db.QueryRow(`SELECT stripe_customer_id FROM tenants WHERE id = $1`, f.tenantID).Scan(&cus))
	assert.Equal(t, "cus_fake", cus)
}

func TestCheckout_LiveSubscription_Refused(t *testing.T) {
	f := setupHouseFixture(t)
	api := &fakeCheckoutAPI{live: true}
	api.install(t)
	svc := readyStripe(t, f)

	_, err := svc.StartCheckout(context.Background(), checkoutFor(f.tenantID))

	assert.True(t, errors.Is(err, payments.ErrLiveSubscription), "got %v", err)
	assert.Zero(t, api.sessions.Load())
}

func TestStripeProvider_StopRenewal_SetsCancelAtPeriodEnd(t *testing.T) {
	stop := renewalParams(context.Background(), true)
	require.NotNil(t, stop.CancelAtPeriodEnd)
	assert.True(t, *stop.CancelAtPeriodEnd)
	resume := renewalParams(context.Background(), false)
	require.NotNil(t, resume.CancelAtPeriodEnd)
	assert.False(t, *resume.CancelAtPeriodEnd, "resume sends false, never omits it")
	assert.Nil(t, stop.Items, "nothing but the renewal changes")
}

// Without STRIPE_SECRET_KEY the provider exists (a page, a flag, the
// deletion runner can name it) and is never ready; every call says why.
func TestStripeProvider_Unconfigured(t *testing.T) {
	svc := NewUnconfiguredStripe(nil)
	st, why := svc.Ready(context.Background())
	assert.Equal(t, payments.Unready, st)
	assert.True(t, strings.Contains(why, "STRIPE_SECRET_KEY"))
	for _, err := range []error{
		svc.Cancel(context.Background(), "t"),
		svc.StopRenewal(context.Background(), "t"),
		func() error { _, err := svc.StartCheckout(context.Background(), checkoutFor("t")); return err }(),
	} {
		assert.ErrorIs(t, err, payments.ErrProviderUnconfigured)
	}
	assert.Equal(t, "stripe", svc.Name())
	assert.Equal(t, payments.RenewalAuto, svc.Renewal())
}

// --- readiness (Part 0 of Prompt 20: RP22-1, RP22-5) ---

// failingPrices is a price source Stripe cannot be reached through.
type failingPrices struct{ err error }

func (f failingPrices) Get(context.Context, string) (*stripe.Price, error) { return nil, f.err }

// Pending while a key is set and no verification has completed; a verify
// that could not reach Stripe keeps the previous verdict (ready stays
// ready, pending stays pending) and never makes the provider unready.
func TestReadiness_TransientVerifyKeepsVerdict(t *testing.T) {
	// Arrange: a configured provider, never verified.
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	svc.ConfigureHouse(testIDs, goodPrices())
	st, why := svc.Ready(context.Background())
	assert.Equal(t, payments.Pending, st, why)

	// Act 1: Stripe unreachable before any verdict → still pending.
	svc.setPriceSource(failingPrices{err: errors.New("dial tcp: connection refused")})
	err := svc.VerifyHousePrices(context.Background())
	require.ErrorIs(t, err, ErrPricesUnreachable)
	st, _ = svc.Ready(context.Background())
	assert.Equal(t, payments.Pending, st, "no verdict yet: pending, not unready")

	// Act 2: verified, then a Stripe 503 → the verdict stays ready.
	svc.setPriceSource(goodPrices())
	require.NoError(t, svc.VerifyHousePrices(context.Background()))
	st, _ = svc.Ready(context.Background())
	require.Equal(t, payments.Ready, st)
	svc.setPriceSource(failingPrices{err: &stripe.Error{HTTPStatusCode: 503, Msg: "upstream"}})
	err = svc.VerifyHousePrices(context.Background())
	assert.ErrorIs(t, err, ErrPricesUnreachable)
	st, _ = svc.Ready(context.Background())
	assert.Equal(t, payments.Ready, st, "a transient failure keeps the previous verdict (RP22-5)")
	ok, _ := svc.HouseCheckoutReady()
	assert.True(t, ok, "checkout stays open on the kept verdict")
}

// A mismatch, a missing price id or a Stripe 4xx (no such price, bad key)
// is a verdict: unready, checkout closed.
func TestReadiness_MismatchIsUnready(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	svc.ConfigureHouse(testIDs, goodPrices())
	require.NoError(t, svc.VerifyHousePrices(context.Background()))

	bad := goodPrices()
	bad["price_std_m"] = perUnit(498, "month")
	svc.setPriceSource(bad)
	err := svc.VerifyHousePrices(context.Background())
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPricesUnreachable)
	st, why := svc.Ready(context.Background())
	assert.Equal(t, payments.Unready, st)
	assert.Contains(t, why, "price_std_m")

	// A Stripe 4xx is a verdict too (a wrong id, a revoked key).
	svc.setPriceSource(goodPrices())
	require.NoError(t, svc.VerifyHousePrices(context.Background()))
	svc.setPriceSource(failingPrices{err: &stripe.Error{HTTPStatusCode: 404, Msg: "No such price"}})
	err = svc.VerifyHousePrices(context.Background())
	assert.NotErrorIs(t, err, ErrPricesUnreachable)
	st, _ = svc.Ready(context.Background())
	assert.Equal(t, payments.Unready, st)

	// No key at all: unready (unchanged).
	st, why = NewUnconfiguredStripe(nil).Ready(context.Background())
	assert.Equal(t, payments.Unready, st)
	assert.Contains(t, why, "STRIPE_SECRET_KEY")
	// A key but no price ids: unready (nothing to verify).
	st, _ = NewStripeService("sk_test_fake", nil, zap.NewNop()).Ready(context.Background())
	assert.Equal(t, payments.Unready, st)
}
