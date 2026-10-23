package payments

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stage 1a Prompt 30 Part 0a.
//
// D-P30-1 (RP37-1's recomputation, RP34-3's class): a credit lowered by a
// recomputation never takes back what checkout already spent — the row
// keeps the spent part, records the shortfall in its metadata and says so
// in an Error line, so the ledger never holds a negative balance silently.
//
// RP37-5 (P4): the renewal_stops job acts on fresh truth — each row and the
// entitlement are read again under the tenant's lock right before the
// provider call; a permanent refusal reverts the switch as the request
// path does.

// rawCredit is the tenant's balance WITHOUT CreditBalance's floor at 0.
func (f *fixture) rawCredit(t *testing.T) int64 {
	t.Helper()
	var bal int64
	require.NoError(t, f.db.QueryRow(`
		SELECT COALESCE(SUM(CASE WHEN kind = 'credit' THEN amount_cents ELSE -credit_cents END), 0)
		  FROM payments WHERE tenant_id = $1 AND status IN ('settled', 'refunded') AND kind IN ('credit', 'charge')`,
		f.tenantID).Scan(&bal))
	return bal
}

func TestSetCredit_LoweredBelowWhatWasSpent_KeepsTheSpentPartAndSaysSo(t *testing.T) {
	// Arrange: a $5.00 credit (a partial payment of an expired invoice) …
	f := newFixture(t)
	ctx := context.Background()
	f.tx(t, func(tx *sql.Tx) error {
		_, err := SetCredit(ctx, tx, f.tenantID, ProviderBTCPay, "partial:inv_a", 500, "partial payment", time.Now())
		return err
	})
	// … spent in full at a later checkout ($10.00 order, $5.00 of it credit).
	order := Order{Std: 1, Period: PeriodMonthly}
	f.tx(t, func(tx *sql.Tx) error {
		_, err := Settle(ctx, tx, Payment{TenantID: f.tenantID, Provider: ProviderBTCPay, ProviderID: "inv_b",
			Kind: KindCharge, Purpose: PurposeNew, AmountCents: 500, CreditCents: 500, Order: &order}, time.Now())
		return err
	})
	require.Zero(t, f.rawCredit(t))

	// Act: invoice A settles late after all, so its credit is recomputed to 0.
	var delta int64
	f.tx(t, func(tx *sql.Tx) error {
		var err error
		delta, err = SetCredit(ctx, tx, f.tenantID, ProviderBTCPay, "partial:inv_a", 0, "", time.Now())
		return err
	})

	// Assert: never a negative balance; the row keeps what was spent and
	// says how much the recomputation could not take back.
	assert.Zero(t, f.rawCredit(t), "the ledger never holds a negative balance")
	assert.Zero(t, delta)
	var amount int64
	var meta []byte
	require.NoError(t, f.db.QueryRow(`SELECT amount_cents, COALESCE(metadata, '{}'::jsonb) FROM payments
		WHERE provider = 'btcpay' AND provider_id = 'partial:inv_a' AND kind = 'credit'`).Scan(&amount, &meta))
	assert.Equal(t, int64(500), amount)
	var m map[string]any
	require.NoError(t, json.Unmarshal(meta, &m))
	assert.EqualValues(t, 500, m["spent_kept_cents"], "the ledger says what it did")
	assert.EqualValues(t, 0, m["recomputed_cents"])
}

func TestStopPendingRenewals_RowWithdrawnMidRun_NoStripeCall(t *testing.T) {
	// Arrange: two owed stops of the tenant, read by the job in one batch.
	f := newFixture(t)
	f.cleanStops(t)
	ctx := context.Background()
	f.tx(t, func(tx *sql.Tx) error {
		if err := RecordRenewalStop(ctx, tx, f.tenantID, ProviderStripe, "sub_a", ReasonImplicitSwitch, RenewalStopPending, nil); err != nil {
			return err
		}
		return RecordRenewalStop(ctx, tx, f.tenantID, ProviderStripe, "sub_b", ReasonImplicitSwitch, RenewalStopPending, nil)
	})
	_, err := f.db.Exec(`UPDATE payment_events SET received_at = received_at - INTERVAL '1 minute'
		WHERE tenant_id = $1 AND event_id = 'renewal_stop:sub_a'`, f.tenantID)
	require.NoError(t, err)
	r, stripe := f.switchRegistry(t, true)
	// While Stripe stops sub_a, the customer's portal un-cancel of sub_b
	// withdraws its stop.
	stripe.onStop = func() {
		if len(stripe.refs) == 0 {
			_, _ = f.db.Exec(`DELETE FROM payment_events WHERE tenant_id = $1 AND event_id = 'renewal_stop:sub_b'`, f.tenantID)
		}
	}

	// Act
	run, err := r.StopPendingRenewals(ctx, f.tenantID)

	// Assert: sub_b was never sent to Stripe — it would have cancelled at
	// period end a house that renews again.
	require.NoError(t, err)
	assert.Equal(t, []string{"sub_a"}, stripe.refs)
	assert.Equal(t, 1, run.Stopped)
	assert.Equal(t, 1, run.SkippedWithdrawn)
}

func TestStopPendingRenewals_PermanentRefusal_RevertsTheSwitch(t *testing.T) {
	// Arrange: the customer switched to bitcoin; the stop did not finish
	// (a Stripe 5xx), so the job owes it.
	f := newFixture(t)
	f.cleanStops(t)
	ctx := context.Background()
	pt := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, pt, ProviderStripe, "sub_r", RenewalAuto)
	})
	r, stripe := f.switchRegistry(t, true)
	stripe.stopErrOnce = errors.New("stripe: 502")
	_, err := SwitchToPrepaid(ctx, r, f.tenantID, ProviderBTCPay, "customer")
	require.NoError(t, err)
	require.Equal(t, RenewalPrepaid, f.entitlement(t).Renewal)

	// Act: the job's call is refused for good (the key changed: a 401).
	stripe.stopErr = fmt.Errorf("%w: 401 invalid api key", ErrProviderRefused)
	_, err = r.StopPendingRenewals(ctx, f.tenantID)
	require.NoError(t, err)

	// Assert: as on the request path — the switch is put back (Stripe still
	// renews the house) and no stop is owed any more.
	e := f.entitlement(t)
	assert.Equal(t, RenewalAuto, e.Renewal)
	assert.Empty(t, f.pendingProvider(t))
	_, found := f.stopRow(t, "sub_r")
	assert.False(t, found, "a reverted switch owes no stop")
}
