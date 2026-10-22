package api

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RP31-7 (the WP-P25 review): with entitlement_lapse OFF for the tenant, a
// card house the customer switched to bitcoin ends when Stripe deletes the
// subscription at the paid date — no grace (the webhook's flag-OFF clear).
// The T−3 reminder says so instead of promising a grace period.

func (f *lapseFixture) stripeSwitchingToBitcoin(tenantID string, paidThrough time.Time) {
	f.t.Helper()
	require.NoError(f.t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		if err := payments.Grant(context.Background(), tx, tenantID, payments.Order{Std: 1, Period: payments.PeriodMonthly},
			paidThrough, payments.ProviderStripe, "sub_switch_"+tenantID, payments.RenewalPrepaid); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE entitlements SET pending_provider = 'btcpay' WHERE tenant_id = $1`, tenantID)
		return err
	}))
}

func TestReminders_PendingBitcoinSwitch_FlagOff_SaysEndsUnlessPaid(t *testing.T) {
	// Arrange: the card renewal is off, bitcoin chosen; T−3.
	f := setupLapseFixture(t)
	id := f.tenant()
	pt := time.Now().Add(60 * time.Hour).Truncate(time.Second)
	f.stripeSwitchingToBitcoin(id, pt)
	sender := &recordingSender{}

	// Act
	_, err := f.reminders(sender, nil).RunOnce(context.Background())

	// Assert
	require.NoError(t, err)
	require.Equal(t, 1, sender.count())
	html := sender.sent[0].html
	assert.Contains(t, html, "ends on <strong>"+pt.UTC().Format("2 Jan 2006")+"</strong> unless the bitcoin payment arrives")
	assert.NotContains(t, html, "grace period until")
}

func TestReminders_PendingBitcoinSwitch_FlagOn_PromisesGrace(t *testing.T) {
	f := setupLapseFixture(t)
	id := f.tenant()
	f.stripeSwitchingToBitcoin(id, time.Now().Add(60*time.Hour).Truncate(time.Second))
	require.NoError(t, f.fl.Set(context.Background(), flagEntitlementLapse, id, true, "test"))
	sender := &recordingSender{}

	_, err := f.reminders(sender, nil).RunOnce(context.Background())

	require.NoError(t, err)
	require.Equal(t, 1, sender.count())
	assert.Contains(t, sender.sent[0].html, "grace period until")
	assert.NotContains(t, sender.sent[0].html, "unless the bitcoin payment arrives")
}
