package payments

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stage 1a Prompt 26 Part 0 — RP31-6 (P4): SwitchToPrepaid writes the row
// first and tells the renewing provider to stop after the commit; a stop
// that fails puts the row back.

// failCommitOf makes every COMMIT that updated the tenant's entitlement row
// fail (a deferred constraint trigger scoped to the tenant), until cleanup.
func (f *fixture) failCommitOf(t *testing.T) {
	t.Helper()
	fn := fmt.Sprintf("p26_fail_commit_%d", time.Now().UnixNano())
	_, err := f.db.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.tenant_id = %s THEN RAISE EXCEPTION 'p26: commit refused'; END IF; RETURN NULL; END $$`,
		fn, pq.QuoteLiteral(f.tenantID)))
	require.NoError(t, err)
	_, err = f.db.Exec(fmt.Sprintf(`CREATE CONSTRAINT TRIGGER %s AFTER UPDATE ON entitlements
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON entitlements`, fn))
		_, _ = f.db.Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
}

func TestSwitchToPrepaid_CommitFails_StripeNeverStopped(t *testing.T) {
	// Arrange: Stripe renews the house; the switch's commit will fail.
	f := newFixture(t)
	pt := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(context.Background(), tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, pt, ProviderStripe, "sub_c", RenewalAuto)
	})
	r, stripe := f.switchRegistry(t, true)
	f.failCommitOf(t)

	// Act
	_, err := SwitchToPrepaid(context.Background(), r, f.tenantID, ProviderBTCPay, "customer")

	// Assert: nothing recorded, so Stripe was never told to stop — a house
	// whose page says "renews" must not run out at period end.
	require.Error(t, err)
	assert.Zero(t, stripe.stops, "the provider is called only after the row committed")
	e := f.entitlement(t)
	assert.Equal(t, RenewalAuto, e.Renewal)
	assert.Empty(t, f.pendingProvider(t))
	assert.Zero(t, f.auditCount(t, "billing.provider_switch_requested"))
}

func TestSwitchToPrepaid_StripeRefuses_RowReverted(t *testing.T) {
	// Arrange
	f := newFixture(t)
	pt := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(context.Background(), tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, pt, ProviderStripe, "sub_r", RenewalAuto)
	})
	r, stripe := f.switchRegistry(t, true)
	stripe.stopErr = fmt.Errorf("stripe: 400 resource_invalid: %w", ErrProviderRefused)

	// Act
	_, err := SwitchToPrepaid(context.Background(), r, f.tenantID, ProviderBTCPay, "customer")

	// Assert: the refusal is the caller's; the row is back to auto, no
	// pending switch, nothing audited as requested.
	require.Error(t, err)
	assert.Equal(t, 1, stripe.stops)
	e := f.entitlement(t)
	assert.Equal(t, RenewalAuto, e.Renewal)
	assert.True(t, pt.Equal(e.PaidThrough))
	assert.Empty(t, f.pendingProvider(t))
	assert.Zero(t, f.auditCount(t, "billing.provider_switch_requested"))
	_, found := f.stopRow(t, "sub_r")
	assert.False(t, found, "the reverted switch owes no stop (RP33-3)")
}

func TestSwitchToPrepaid_StripeRefuses_ConcurrentChangeKept(t *testing.T) {
	// The revert puts back only what the switch wrote: a webhook that
	// changed the row meanwhile (the portal's un-cancel withdrew the
	// switch) is not overwritten.
	f := newFixture(t)
	pt := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(context.Background(), tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, pt, ProviderStripe, "sub_k", RenewalAuto)
	})
	r, stripe := f.switchRegistry(t, true)
	stripe.stopErr = fmt.Errorf("stripe: 400 resource_invalid: %w", ErrProviderRefused)
	stripe.onStop = func() {
		_, err := f.db.Exec(`UPDATE entitlements SET pending_provider = NULL, renewal = 'prepaid' WHERE tenant_id = $1`, f.tenantID)
		require.NoError(t, err)
	}

	_, err := SwitchToPrepaid(context.Background(), r, f.tenantID, ProviderBTCPay, "customer")

	require.Error(t, err)
	assert.Equal(t, RenewalPrepaid, f.entitlement(t).Renewal, "the concurrent writer's row stands")
}
