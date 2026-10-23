package payments

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stage 1a Prompt 32 Part 0a (RP40-1, P4): a pending stop whose
// subscription the house renews by again — the portal un-cancel's grant
// applied, its withdraw lost — is withdrawn by the job (the un-cancel's own
// write, done late) instead of being skipped every 10 minutes for ever
// without a name in result.errors.

func TestStopPendingRenewals_RenewsAgain_WithdrawsTheStop(t *testing.T) {
	// Arrange: the customer switched to bitcoin and the stop did not finish
	// (a Stripe 5xx), so a pending row is owed …
	f := newFixture(t)
	f.cleanStops(t)
	ctx := context.Background()
	pt := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, pt, ProviderStripe, "sub_u", RenewalAuto)
	})
	r, stripe := f.switchRegistry(t, true)
	stripe.stopErrOnce = errors.New("stripe: 502")
	_, err := SwitchToPrepaid(ctx, r, f.tenantID, ProviderBTCPay, "customer")
	require.NoError(t, err)
	_, found := f.stopRow(t, "sub_u")
	require.True(t, found)
	// … then the portal un-cancel's grant put the house back on auto by the
	// same subscription, but the withdraw of the stop never happened (the
	// webhook that carried it was lost).
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, pt, ProviderStripe, "sub_u", RenewalAuto)
	})
	require.Equal(t, RenewalAuto, f.entitlement(t).Renewal)
	calls := stripe.stops

	// Act
	run, err := r.StopPendingRenewals(ctx, f.tenantID)

	// Assert: the row is gone, the withdraw audited, Stripe never asked.
	require.NoError(t, err)
	_, found = f.stopRow(t, "sub_u")
	assert.False(t, found, "a stop the house no longer owes is withdrawn, not left pending for ever")
	assert.Equal(t, 1, run.SkippedWithdrawn)
	assert.Equal(t, calls, stripe.stops, "no provider call: it would cancel a house that renews again")
	assert.Equal(t, 1, f.auditCount(t, "billing.renewal_stop_withdrawn"))
	assert.Equal(t, RenewalAuto, f.entitlement(t).Renewal)

	// A second pass finds nothing pending.
	run, err = r.StopPendingRenewals(ctx, f.tenantID)
	require.NoError(t, err)
	assert.Zero(t, run.SkippedWithdrawn)
}
