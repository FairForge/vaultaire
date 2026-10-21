package billing

// WP-R10-2 (R10-15, R10-16): Stripe does not guarantee delivery order, and
// it may deliver one event more than once — concurrently. A subscription
// event is applied from the subscription as Stripe has it NOW (fetched), the
// payload naming only the ids; an event id is claimed before its handler
// runs, and the claim is released when the handler fails.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"

	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

// stripeHas makes sub the subscription's current state at Stripe (what a
// fetch returns), and returns a copy to deliver as an event payload.
func (f *houseFixture) stripeHas(sub *stripe.Subscription) *stripe.Subscription {
	cur := *sub
	f.subs[sub.ID] = &cur
	payload := *sub
	return &payload
}

func (f *houseFixture) houseTB(t *testing.T) (std int, ok bool) {
	t.Helper()
	house, ok, err := f.qm.GetHouse(context.Background(), f.tenantID)
	require.NoError(t, err)
	return house.StdTB(), ok
}

func TestWebhook_StaleActiveUpdateAfterCancel_DoesNotRegrantTheHouse(t *testing.T) {
	// Arrange: a paid house, then Stripe cancels it.
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_std_y", 3))
	sub.Customer.ID = cus
	activePayload := f.stripeHas(sub)
	require.Equal(t, http.StatusOK, f.postCode(t, "created", "customer.subscription.created", activePayload))
	_, ok := f.houseTB(t)
	require.True(t, ok)

	canceled := *sub
	canceled.Status = stripe.SubscriptionStatusCanceled
	canceledPayload := f.stripeHas(&canceled)
	require.Equal(t, http.StatusOK, f.postCode(t, "upd_canceled", "customer.subscription.updated", canceledPayload))
	_, ok = f.houseTB(t)
	require.False(t, ok, "the cancel clears the house")

	// Act: the OLDER updated(active) is delivered last (Stripe's retry of a
	// delivery that failed before the cancel).
	code := f.postCode(t, "upd_active_stale", "customer.subscription.updated", activePayload)

	// Assert: Stripe's current state is applied, not the payload's.
	assert.Equal(t, http.StatusOK, code)
	_, ok = f.houseTB(t)
	assert.False(t, ok, "a stale active payload re-granted a cancelled house")
	_, state, _, _ := f.record(t)
	assert.Equal(t, "lapsed", state)
}

func TestWebhook_StaleUnpaidUpdate_DoesNotTakeAPaidHouseAway(t *testing.T) {
	// Arrange: the subscription went unpaid, then was paid and resized.
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	unpaid := subWith("sub_"+f.tenantID, "unpaid", item("si_1", "price_std_y", 3))
	unpaid.Customer.ID = cus
	unpaidPayload := *unpaid
	paid := subWith(unpaid.ID, "active", item("si_1", "price_std_y", 5))
	paid.Customer.ID = cus
	require.Equal(t, http.StatusOK, f.postCode(t, "upd_paid", "customer.subscription.updated", f.stripeHas(paid)))
	std, ok := f.houseTB(t)
	require.True(t, ok)
	require.Equal(t, 5, std)

	// Act: the older unpaid event arrives after.
	code := f.postCode(t, "upd_unpaid_stale", "customer.subscription.updated", &unpaidPayload)

	// Assert
	assert.Equal(t, http.StatusOK, code)
	std, ok = f.houseTB(t)
	assert.True(t, ok, "a stale unpaid payload took a paid house away")
	assert.Equal(t, 5, std)
}

func TestWebhook_PausedSubscription_ClearsTheHouse(t *testing.T) {
	// Arrange: a paid house.
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_std_y", 3))
	sub.Customer.ID = cus
	require.Equal(t, http.StatusOK, f.postCode(t, "created", "customer.subscription.created", f.stripeHas(sub)))

	// Act: Stripe pauses it (a trial that ended without a payment method:
	// no invoice is created any more — nothing is being paid).
	paused := *sub
	paused.Status = stripe.SubscriptionStatusPaused
	code := f.postCode(t, "upd_paused", "customer.subscription.updated", f.stripeHas(&paused))

	// Assert: as unpaid — the house goes, the data stays.
	assert.Equal(t, http.StatusOK, code)
	_, ok := f.houseTB(t)
	assert.False(t, ok, "a paused subscription pays nothing and grants nothing")
	_, lim, err := f.qm.GetUsage(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.Equal(t, usage.FreeTierLimits.StorageBytes, lim)
	_, state, _, _ := f.record(t)
	assert.Equal(t, "lapsed", state)
	assert.Equal(t, payments.RenewalPrepaid, f.entitlement(t).Renewal)
}

// gatedSubs is a subscription source that counts its calls and holds each
// one until a second call arrives (or a short wait passes): two handlers in
// flight at once are both seen.
type gatedSubs struct {
	fakeSubs
	calls atomic.Int32
	both  chan struct{}
	once  sync.Once
}

func (g *gatedSubs) Get(ctx context.Context, id string) (*stripe.Subscription, error) {
	if g.calls.Add(1) == 2 {
		g.once.Do(func() { close(g.both) })
	}
	select {
	case <-g.both:
	case <-time.After(300 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.fakeSubs.Get(ctx, id)
}

func TestWebhook_ConcurrentDeliveriesOfOneEvent_AreHandledOnce(t *testing.T) {
	// Arrange
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_std_y", 2))
	sub.Customer.ID = cus
	f.stripeHas(sub)
	gate := &gatedSubs{fakeSubs: f.subs, both: make(chan struct{})}
	f.handler.subs = gate
	body := eventBody(t, "evt_"+f.tenantID+"_dup", "checkout.session.completed", stripe.CheckoutSession{
		ID: "cs_dup", Customer: &stripe.Customer{ID: cus}, Subscription: &stripe.Subscription{ID: sub.ID}})

	// Act: Stripe delivers the same event twice at once.
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, signedRequest(t, testWebhookSecret, time.Now(), body))
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()

	// Assert: one handler ran; both deliveries are acknowledged; one row.
	assert.EqualValues(t, 1, gate.calls.Load(), "both concurrent deliveries reached the handler")
	assert.Equal(t, []int{http.StatusOK, http.StatusOK}, codes)
	assert.True(t, f.eventRecorded(t, "dup"))
	std, ok := f.houseTB(t)
	assert.True(t, ok)
	assert.Equal(t, 2, std)
}

// failingOnceSubs fails its first call (Stripe unreachable), then serves.
type failingOnceSubs struct {
	fakeSubs
	calls atomic.Int32
}

func (s *failingOnceSubs) Get(ctx context.Context, id string) (*stripe.Subscription, error) {
	if s.calls.Add(1) == 1 {
		return nil, &stripe.Error{HTTPStatusCode: http.StatusServiceUnavailable, Msg: "stripe is down"}
	}
	return s.fakeSubs.Get(ctx, id)
}

func TestWebhook_FailedHandlerReleasesTheClaim_TheRetryIsApplied(t *testing.T) {
	// Arrange
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_std_y", 4))
	sub.Customer.ID = cus
	payload := f.stripeHas(sub)
	f.handler.subs = &failingOnceSubs{fakeSubs: f.subs}

	// Act: the first delivery cannot fetch; Stripe retries the same event.
	first := f.postCode(t, "retry", "customer.subscription.created", payload)
	recordedAfterFailure := f.eventRecorded(t, "retry")
	second := f.postCode(t, "retry", "customer.subscription.created", payload)

	// Assert
	assert.Equal(t, http.StatusInternalServerError, first, "a fetch failure is a retry")
	assert.False(t, recordedAfterFailure, "the claim of a failed handler is released")
	assert.Equal(t, http.StatusOK, second)
	assert.True(t, f.eventRecorded(t, "retry"))
	std, ok := f.houseTB(t)
	assert.True(t, ok)
	assert.Equal(t, 4, std)
}

func TestWebhook_DeliveryCutMidHandler_LeavesNoClaim(t *testing.T) {
	// Arrange: Stripe hangs up while the handler waits on its fetch.
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_std_y", 1))
	sub.Customer.ID = cus
	payload := f.stripeHas(sub)
	ctx, cancel := context.WithCancel(context.Background())
	f.handler.subs = cancellingSubs{fakeSubs: f.subs, cancel: cancel}

	// Act
	body := eventBody(t, "evt_"+f.tenantID+"_cut", "customer.subscription.created", payload)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, signedRequest(t, testWebhookSecret, time.Now(), body).WithContext(ctx))

	// Assert: nothing recorded — the retry will be handled.
	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.False(t, f.eventRecorded(t, "cut"))
}

type cancellingSubs struct {
	fakeSubs
	cancel context.CancelFunc
}

func (s cancellingSubs) Get(ctx context.Context, _ string) (*stripe.Subscription, error) {
	s.cancel()
	<-ctx.Done()
	return nil, ctx.Err()
}

// fakeStripeAPI serves the two subscription calls billing makes —
// GET /v1/subscriptions/{id} and GET /v1/subscriptions?customer=… — from
// subs, with Stripe's own 404 body for an unknown id, behind stripe-go's API
// backend for the length of the test.
func fakeStripeAPI(t *testing.T, subs map[string]*stripe.Subscription) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if id, ok := strings.CutPrefix(r.URL.Path, "/v1/subscriptions/"); ok {
			s, found := subs[id]
			if !found {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"resource_missing","param":"id","message":"No such subscription: '` + id + `'"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(s)
			return
		}
		if r.URL.Path == "/v1/subscriptions" {
			var data []*stripe.Subscription
			for _, s := range subs {
				if s.Customer != nil && s.Customer.ID == r.URL.Query().Get("customer") {
					data = append(data, s)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "has_more": false, "url": "/v1/subscriptions"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	prev := stripe.GetBackend(stripe.APIBackend)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0),
		LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull},
	}))
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, prev) })
}

func TestHasLiveSubscription_CountsAPausedSubscription(t *testing.T) {
	// Arrange: the customer's only subscription is paused — resumable, so a
	// second Checkout next to it would be a second subscription (R10 note).
	fakeStripeAPI(t, map[string]*stripe.Subscription{
		"sub_p": {ID: "sub_p", Status: stripe.SubscriptionStatusPaused, Customer: &stripe.Customer{ID: "cus_p"}},
		"sub_c": {ID: "sub_c", Status: stripe.SubscriptionStatusCanceled, Customer: &stripe.Customer{ID: "cus_c"}},
	})
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())

	// Act
	paused, errP := svc.HasLiveSubscription(context.Background(), "cus_p")
	canceled, errC := svc.HasLiveSubscription(context.Background(), "cus_c")

	// Assert
	require.NoError(t, errP)
	require.NoError(t, errC)
	assert.True(t, paused, "a paused subscription blocks a second checkout, as unpaid does")
	assert.False(t, canceled)
}

func TestWebhook_SubscriptionStripeNoLongerHas_IsAcknowledged(t *testing.T) {
	// Arrange: the real fetch path against a Stripe that answers 404.
	f := setupHouseFixture(t)
	f.handler.subs = nil
	fakeStripeAPI(t, map[string]*stripe.Subscription{})
	sub := subWith("sub_gone_"+f.tenantID, "active", item("si_1", "price_std_y", 2))
	sub.Customer.ID = "cus_" + f.tenantID

	// Act
	code := f.postCode(t, "gone", "customer.subscription.updated", sub)

	// Assert: nothing to apply, never three days of retries; no house.
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, f.eventRecorded(t, "gone"))
	_, ok := f.houseTB(t)
	assert.False(t, ok)
}
