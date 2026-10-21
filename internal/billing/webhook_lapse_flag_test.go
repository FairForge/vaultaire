package billing

import (
	"context"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-PAY-2 item 6: with entitlement_lapse ON for the tenant the date
// decides — a terminal Stripe status stops the renewal and keeps the house
// (the lapse job clears it after paid_through + grace); OFF, today's clear.

type lapseFlag map[string]bool

func (f lapseFlag) Enabled(key, tenantID string) bool {
	return key == payments.FlagEntitlementLapse && f[tenantID]
}

func (f *houseFixture) grantedHouse(t *testing.T) (start, end time.Time) {
	t.Helper()
	start = time.Now().Add(-time.Hour).Truncate(time.Second)
	end = start.Add(30 * 24 * time.Hour)
	f.post(t, "created", "customer.subscription.created", f.stripeHas(f.houseSub("active", start, end)))
	return start, end
}

// pastHouse is a granted house whose paid_through passed an hour ago (a
// period that ended unpaid): a terminal status revokes it while
// entitlement_lapse is off. A date still ahead is kept (RP26-1).
func (f *houseFixture) pastHouse(t *testing.T) (start, end time.Time) {
	t.Helper()
	start = time.Now().Add(-31 * 24 * time.Hour).Truncate(time.Second)
	end = time.Now().Add(-time.Hour).Truncate(time.Second)
	f.post(t, "created", "customer.subscription.created", f.stripeHas(f.houseSub("active", start, end)))
	return start, end
}

func (f *houseFixture) hasHouse(t *testing.T) bool {
	t.Helper()
	_, ok, err := f.qm.GetHouse(context.Background(), f.tenantID)
	require.NoError(t, err)
	return ok
}

func TestApplyHouse_TerminalStatus_FlagOn_SetsPrepaidKeepsHouse(t *testing.T) {
	for _, status := range []string{"canceled", "unpaid", "incomplete_expired", "paused"} {
		t.Run(status, func(t *testing.T) {
			// Arrange
			f := setupHouseFixture(t)
			f.handler.SetFlags(lapseFlag{f.tenantID: true})
			start, end := f.grantedHouse(t)

			// Act
			f.post(t, status, "customer.subscription.updated", f.stripeHas(f.houseSub(status, start, end)))

			// Assert
			assert.True(t, f.hasHouse(t), "the floors stay: the date and the lapse job decide")
			e := f.entitlement(t)
			assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
			assert.Nil(t, e.LapsedAt, "only the lapse job writes lapsed_at while the flag is on")
			assert.True(t, end.Equal(e.PaidThrough))
		})
	}
}

func TestApplyHouse_TerminalStatus_FlagOff_Clears(t *testing.T) {
	f := setupHouseFixture(t)
	f.handler.SetFlags(lapseFlag{}) // off for this tenant
	start, end := f.pastHouse(t)

	f.post(t, "canceled", "customer.subscription.updated", f.stripeHas(f.houseSub("canceled", start, end)))

	assert.False(t, f.hasHouse(t))
	e := f.entitlement(t)
	assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
	assert.NotNil(t, e.LapsedAt)
}

func TestSubscriptionDeleted_FlagOn_SetsPrepaidKeepsHouse(t *testing.T) {
	f := setupHouseFixture(t)
	f.handler.SetFlags(lapseFlag{f.tenantID: true})
	start, end := f.grantedHouse(t)

	f.post(t, "deleted", "customer.subscription.deleted", f.houseSub("canceled", start, end))

	assert.True(t, f.hasHouse(t))
	e := f.entitlement(t)
	assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
	assert.Nil(t, e.LapsedAt)
	_, status, _, period := f.tenantRow(t)
	assert.Equal(t, "canceled", status)
	assert.Equal(t, "monthly", period, "the house is still in force")
}

// RP20-A3: the current subscription is the entitlement's provider_ref; the
// tenants column is the fallback for a tenant with no row. A clearing event
// of a subscription that is not the row's is stale, whatever tenants says.
func TestIsCurrentSubscription_ReadsTheEntitlementFirst(t *testing.T) {
	f := setupHouseFixture(t)
	start, end := f.pastHouse(t)
	// tenants points elsewhere (an out-of-band edit); the entitlement row
	// names sub_<tenant>.
	_, err := f.db.Exec(`UPDATE tenants SET stripe_subscription_id = 'sub_other' WHERE id = $1`, f.tenantID)
	require.NoError(t, err)

	f.post(t, "canceled", "customer.subscription.updated", f.stripeHas(f.houseSub("canceled", start, end)))

	assert.False(t, f.hasHouse(t), "the entitlement's subscription was cancelled: cleared")
}

// RP22-2: with the flag ON, the deletion of a NON-house subscription (a
// legacy pack) keeps today's downgrade — plan 'starter', nothing to stop
// renewing; the flag branch is for a house only.
func TestSubscriptionDeleted_PackUnderFlag_DowngradesToStarter(t *testing.T) {
	// Arrange: a tenant on a pack, flag on.
	f := setupHouseFixture(t)
	f.handler.SetFlags(lapseFlag{f.tenantID: true})
	pack := subWith("sub_"+f.tenantID, "canceled", item("si_pack", "price_vault3_legacy", 1))
	pack.Customer.ID = "cus_" + f.tenantID
	_, err := f.db.Exec(`UPDATE tenants SET plan = 'vault3', stripe_subscription_id = $2, subscription_status = 'active' WHERE id = $1`,
		f.tenantID, pack.ID)
	require.NoError(t, err)

	// Act
	f.post(t, "pack-deleted", "customer.subscription.deleted", pack)

	// Assert
	plan, status, subID, period := f.tenantRow(t)
	assert.Equal(t, "starter", plan, "a pack is downgraded, flag or no flag")
	assert.Equal(t, "canceled", status)
	assert.Empty(t, subID)
	assert.Empty(t, period)
	_, err = payments.Get(context.Background(), f.db, f.tenantID)
	assert.ErrorIs(t, err, payments.ErrNoEntitlement, "no entitlement was ever written for a pack")
}
