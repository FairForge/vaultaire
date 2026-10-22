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

// Stage 1a Prompt 27 Part 0 — RP33-3 (P3, proven) + RP33-8a (P4): the
// Stripe stop of "Switch to bitcoin at renewal" is durable. The intent is a
// pending renewal_stop row written in the switch's own transaction; the
// call after the commit runs detached from the request (a client that
// leaves cannot cancel it) and a stop it could not finish — a client gone,
// a Stripe 5xx, the process killed — is finished by the renewal_stops job.

// stopRow is the tenant's renewal_stop row of ref (found false = none).
func (f *fixture) stopRow(t *testing.T, ref string) (RenewalStop, bool) {
	t.Helper()
	s, found, err := RenewalStopOf(context.Background(), f.db, f.tenantID, ProviderStripe, ref)
	require.NoError(t, err)
	return s, found
}

func (f *fixture) cleanStops(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM payment_events WHERE tenant_id = $1`, f.tenantID) })
}

func TestSwitchToPrepaid_ClientGoneDuringStop_JobStopsStripe(t *testing.T) {
	// Arrange: Stripe renews the house; the customer's browser goes away
	// while Stripe is being told to stop, and that call does not finish.
	f := newFixture(t)
	f.cleanStops(t)
	pt := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(context.Background(), tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, pt, ProviderStripe, "sub_gone", RenewalAuto)
	})
	r, stripe := f.switchRegistry(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stripe.onStop = cancel
	stripe.stopErrOnce = errors.New("stripe: connection reset")

	// Act
	_, _ = SwitchToPrepaid(ctx, r, f.tenantID, ProviderBTCPay, "customer")

	// Assert: the switch stands (the customer asked for it) and the stop is
	// owed — a pending renewal_stop row names the subscription.
	assert.NoError(t, stripe.ctxErrAtStop, "the provider call must not run under the request's context")
	e := f.entitlement(t)
	assert.Equal(t, RenewalPrepaid, e.Renewal)
	assert.Equal(t, ProviderBTCPay, f.pendingProvider(t))
	s, found := f.stopRow(t, "sub_gone")
	require.True(t, found, "the stop's intent is durable")
	assert.Equal(t, RenewalStopPending, s.Status)

	// The renewal_stops job finishes it.
	run, err := r.StopPendingRenewals(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.Equal(t, 1, run.Stopped)
	assert.Contains(t, stripe.refs, "sub_gone")
	s, _ = f.stopRow(t, "sub_gone")
	assert.Equal(t, RenewalStopStopped, s.Status)
}

func TestSwitchToPrepaid_CrashAfterCommit_JobStopsStripe(t *testing.T) {
	// Arrange: the process dies inside the Stripe call, after the switch
	// committed.
	f := newFixture(t)
	f.cleanStops(t)
	pt := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(context.Background(), tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, pt, ProviderStripe, "sub_crash", RenewalAuto)
	})
	r, stripe := f.switchRegistry(t, true)
	stripe.panicOnStop = true

	// Act
	func() {
		defer func() { _ = recover() }()
		_, _ = SwitchToPrepaid(context.Background(), r, f.tenantID, ProviderBTCPay, "customer")
	}()

	// Assert: the committed switch carries its pending stop; after the
	// restart the job stops Stripe.
	assert.Equal(t, RenewalPrepaid, f.entitlement(t).Renewal)
	s, found := f.stopRow(t, "sub_crash")
	require.True(t, found, "the stop's intent committed with the switch")
	assert.Equal(t, RenewalStopPending, s.Status)
	stripe.panicOnStop = false
	run, err := r.StopPendingRenewals(context.Background(), "")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, run.Stopped, 1)
	assert.Contains(t, stripe.refs, "sub_crash")
	s, _ = f.stopRow(t, "sub_crash")
	assert.Equal(t, RenewalStopStopped, s.Status)
}

func TestRecordRenewalStop_StoppedNeverClosesAPendingStop(t *testing.T) {
	// A bitcoin settle after a switch whose stop did not finish records the
	// switch's stop as "stopped" (the customer asked for it) — it must not
	// close the pending row: Stripe was never told, and only its answer
	// closes the row.
	f := newFixture(t)
	f.cleanStops(t)
	f.tx(t, func(tx *sql.Tx) error {
		return RecordRenewalStop(context.Background(), tx, f.tenantID, ProviderStripe, "sub_owed", ReasonSwitch, RenewalStopPending, nil)
	})

	f.tx(t, func(tx *sql.Tx) error {
		return RecordRenewalStop(context.Background(), tx, f.tenantID, ProviderStripe, "sub_owed", ReasonSwitch, RenewalStopStopped, nil)
	})

	s, found := f.stopRow(t, "sub_owed")
	require.True(t, found)
	assert.Equal(t, RenewalStopPending, s.Status)
	// A pending record still re-opens a stopped row.
	require.NoError(t, MarkRenewalStop(context.Background(), f.db, ProviderStripe, "sub_owed", RenewalStopStopped, ""))
	f.tx(t, func(tx *sql.Tx) error {
		return RecordRenewalStop(context.Background(), tx, f.tenantID, ProviderStripe, "sub_owed", ReasonSwitch, RenewalStopPending, nil)
	})
	s, _ = f.stopRow(t, "sub_owed")
	assert.Equal(t, RenewalStopPending, s.Status)
}

func TestSwitchToPrepaid_StopFinishes_RowClosedAndAudited(t *testing.T) {
	f := newFixture(t)
	f.cleanStops(t)
	pt := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(context.Background(), tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, pt, ProviderStripe, "sub_ok", RenewalAuto)
	})
	r, stripe := f.switchRegistry(t, true)

	_, err := SwitchToPrepaid(context.Background(), r, f.tenantID, ProviderBTCPay, "customer")

	require.NoError(t, err)
	assert.Equal(t, []string{"sub_ok"}, stripe.refs, "stopped by the subscription's id")
	s, found := f.stopRow(t, "sub_ok")
	require.True(t, found)
	assert.Equal(t, RenewalStopStopped, s.Status)
	assert.Equal(t, 1, f.auditCount(t, "billing.renewal_stopped"))
}
