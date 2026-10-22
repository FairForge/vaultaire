package billing

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

// Stage 1a Prompt 27 Part 0 — RP33-7 (P3, = RP32-1): "Switch to bitcoin
// at renewal" from a subscription whose current period was never paid
// (past_due / unpaid / incomplete) cancels it now — RP31-4's path, the
// one StopRenewalRef takes — instead of cancel_at_period_end, which left
// Stripe's dunning retrying the card for the period the customer is about
// to pay in bitcoin.

// prepaidProvider is a prepaid provider (bitcoin) the switch can name.
type prepaidProvider struct{}

func (prepaidProvider) Name() string              { return payments.ProviderBTCPay }
func (prepaidProvider) Renewal() payments.Renewal { return payments.RenewalPrepaid }
func (prepaidProvider) Ready(context.Context) (payments.Readiness, string) {
	return payments.Ready, ""
}
func (prepaidProvider) StartCheckout(context.Context, payments.Checkout) (string, error) {
	return "", nil
}
func (prepaidProvider) Resize(context.Context, string, payments.Order) (payments.ResizeOutcome, error) {
	return payments.ResizeOutcome{}, nil
}
func (prepaidProvider) StopRenewal(context.Context, string) error   { return nil }
func (prepaidProvider) ResumeRenewal(context.Context, string) error { return nil }
func (prepaidProvider) Cancel(context.Context, string) error        { return nil }
func (prepaidProvider) ManageURL(context.Context, string, string) (string, error) {
	return "", nil
}
func (prepaidProvider) Webhook() http.Handler { return http.NotFoundHandler() }

type allOn struct{}

func (allOn) Enabled(string, string) bool { return true }

func TestSwitchToPrepaid_FromPastDue_CancelsNow(t *testing.T) {
	for _, status := range []string{"past_due", "unpaid", "incomplete"} {
		t.Run(status, func(t *testing.T) {
			// Arrange: Stripe renews the house; its current period is in
			// dunning.
			f := setupHouseFixture(t)
			api := &fakeSubAPI{status: status}
			api.install(t)
			svc := NewStripeService("sk_test_fake", f.db, zap.NewNop())
			sub := "sub_dunning_" + f.tenantID
			pt := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
			require.NoError(t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
				return payments.Grant(context.Background(), tx, f.tenantID, payments.Order{Std: 1, Period: payments.PeriodMonthly},
					pt, payments.ProviderStripe, sub, payments.RenewalAuto)
			}))
			r := payments.NewRegistry(f.db, allOn{}, 0, nil)
			r.Register(svc)
			r.Register(prepaidProvider{})

			// Act
			e, err := r.SwitchToPrepaid(context.Background(), f.tenantID, payments.ProviderBTCPay, "customer")

			// Assert: cancelled now (no proration, no final invoice), never
			// cancel_at_period_end; the switch stands.
			require.NoError(t, err)
			assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
			api.mu.Lock()
			defer api.mu.Unlock()
			require.Len(t, api.deletes, 1, "an unpaid period is cancelled now")
			assert.Equal(t, "false", api.deletes[0].Get("invoice_now"))
			assert.Equal(t, "false", api.deletes[0].Get("prorate"))
			assert.Empty(t, api.updates, "never cancel_at_period_end on a period in dunning")
		})
	}
}

// The switch's own stop row (RP33-3) does not make the entitlement's own
// subscription "switched away": the portal's un-cancel still withdraws the
// switch — and the stop it owed — instead of re-stopping Stripe.
func TestPortalUncancel_AfterDurableSwitch_WithdrawsTheStop(t *testing.T) {
	// Arrange: the customer asked to switch to bitcoin; the stop was owed
	// (pending) when they re-enabled the renewal in Stripe's portal.
	f := setupHouseFixture(t)
	start := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	sub := f.houseSub("active", start, end)
	sub.CancelAtPeriodEnd = true
	f.subs[sub.ID] = sub
	f.post(t, "granted", "customer.subscription.updated", sub)
	_, err := f.db.Exec(`UPDATE entitlements SET pending_provider = 'btcpay' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	require.NoError(t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		return payments.RecordRenewalStop(context.Background(), tx, f.tenantID, payments.ProviderStripe, sub.ID,
			payments.ReasonSwitch, payments.RenewalStopPending, nil)
	}))
	stops := &fakeStops{}
	f.handler.stops = stops

	// Act
	uncancelled := *sub
	uncancelled.CancelAtPeriodEnd = false
	f.post(t, "uncancel", "customer.subscription.updated", f.stripeHas(&uncancelled))

	// Assert
	e := f.entitlement(t)
	assert.Equal(t, payments.RenewalAuto, e.Renewal)
	_, found, err := payments.RenewalStopOf(context.Background(), f.db, f.tenantID, payments.ProviderStripe, sub.ID)
	require.NoError(t, err)
	assert.False(t, found, "the withdrawn switch owes no stop")
	stops.mu.Lock()
	defer stops.mu.Unlock()
	assert.Empty(t, stops.refs, "Stripe is not told to stop a renewal the customer just re-enabled")
}

// RP33-3: only a permanent Stripe answer reverts a switch.
func TestRefusedOr_PermanentOnlyForA4xx(t *testing.T) {
	for code, permanent := range map[int]bool{400: true, 401: true, 403: true, 402: true,
		404: false, 408: false, 409: false, 429: false, 500: false, 502: false, 0: false} {
		err := refusedOr(&stripe.Error{HTTPStatusCode: code, Msg: "fake"})
		assert.Equal(t, permanent, errors.Is(err, payments.ErrProviderRefused), "HTTP %d", code)
	}
	assert.False(t, errors.Is(refusedOr(context.DeadlineExceeded), payments.ErrProviderRefused), "no answer is not a refusal")
}
