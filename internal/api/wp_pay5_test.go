package api

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-PAY-5 (Stage 1a Prompt 25).

// refStripe is a Stripe provider that cancels named subscriptions.
type refStripe struct {
	payments.Provider
	mu   sync.Mutex
	refs []string
}

func (f *refStripe) Name() string { return payments.ProviderStripe }
func (f *refStripe) CancelRef(_ context.Context, _, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs = append(f.refs, ref)
	return nil
}
func (f *refStripe) Cancel(context.Context, string) error { return nil }

// RP30-2 b): a BTCPay settle took over a house Stripe renewed by itself;
// the Stripe stop is not confirmed yet. The entitlement names the invoice,
// so only the renewal_stop row still names the subscription — and the
// runner must cancel it: never a subscription that survives the erasure.
func TestDeletionRunner_ImplicitSwitchPending_CancelsTheStripeSub(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	sub := "sub_live_" + f.suffix
	f.exec(`UPDATE entitlements SET provider = 'btcpay', provider_ref = $2, renewal = 'prepaid' WHERE tenant_id = $1`,
		f.tenantID, "BtcInv_"+f.suffix)
	require.NoError(t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		return payments.RecordRenewalStop(context.Background(), tx, f.tenantID, payments.ProviderStripe, sub,
			payments.ReasonImplicitSwitch, payments.RenewalStopPending, nil)
	}))
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM payment_events WHERE tenant_id = $1`, f.tenantID) })
	stripe := &refStripe{}
	f.runner.Payments = fakeProviders{payments.ProviderStripe: stripe}

	var te TenantErasure
	err := f.runner.cancelSubscription(context.Background(), f.tenantID, &te)

	require.NoError(t, err)
	assert.Equal(t, []string{sub}, stripe.refs, "the subscription the stop row names is cancelled")
	assert.True(t, te.StripeCancelled)
}

// WP-PAY-5 item 1: a customer who asked to switch to crypto and never pays
// keeps the house to paid_through + grace, then the lapse job clears it —
// no gap is invented, none is forgiven.
func TestSwitchToCrypto_NoPayment_GraceThenLapse(t *testing.T) {
	f := setupLapseFixture(t)
	ctx := context.Background()
	inGrace, past := f.tenant(), f.tenant()
	reg := payments.NewRegistry(f.db, switchFlags{}, 0, nil)
	reg.Register(&autoProvider{name: payments.ProviderStripe})
	reg.Register(&prepaidProvider{name: payments.ProviderBTCPay})
	for _, id := range []string{inGrace, past} {
		f.granted(id, time.Now().Add(20*24*time.Hour), payments.ProviderStripe)
		_, err := payments.SwitchToPrepaid(ctx, reg, id, payments.ProviderBTCPay, "customer")
		require.NoError(t, err)
		require.NoError(t, f.fl.Set(ctx, flagEntitlementLapse, id, true, "test"))
	}
	// Time passes with no bitcoin payment: one is 5 days into grace, one 15.
	_, err := f.db.Exec(`UPDATE entitlements SET paid_through = NOW() - INTERVAL '5 days' WHERE tenant_id = $1`, inGrace)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE entitlements SET paid_through = NOW() - INTERVAL '15 days' WHERE tenant_id = $1`, past)
	require.NoError(t, err)

	res := f.run()

	assert.Equal(t, 1, res.Cleared)
	_, ok := f.house(inGrace)
	assert.True(t, ok, "inside grace: kept")
	_, ok = f.house(past)
	assert.False(t, ok, "past grace: cleared")
	assert.Equal(t, payments.RenewalPrepaid, f.entitlement(past).Renewal)
}

type switchFlags struct{}

func (switchFlags) Enabled(string, string) bool { return true }

// autoProvider / prepaidProvider are the two kinds of provider, ready.
type autoProvider struct {
	payments.Provider
	name string
}

func (p *autoProvider) Name() string                                       { return p.name }
func (p *autoProvider) Renewal() payments.Renewal                          { return payments.RenewalAuto }
func (p *autoProvider) Ready(context.Context) (payments.Readiness, string) { return payments.Ready, "" }
func (p *autoProvider) StopRenewal(context.Context, string) error          { return nil }

type prepaidProvider struct {
	payments.Provider
	name string
}

func (p *prepaidProvider) Name() string              { return p.name }
func (p *prepaidProvider) Renewal() payments.Renewal { return payments.RenewalPrepaid }
func (p *prepaidProvider) Ready(context.Context) (payments.Readiness, string) {
	return payments.Ready, ""
}
