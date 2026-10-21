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
	_, state, _, period := f.record(t)
	assert.Equal(t, "granted", state, "the house is still in force")
	assert.Equal(t, "monthly", period)
}

// RP20-A3 / WP-PAY-7: the current subscription is the entitlement's
// provider_ref and nothing else.
func TestIsCurrentSubscription_ReadsTheEntitlementFirst(t *testing.T) {
	f := setupHouseFixture(t)
	start, end := f.pastHouse(t)

	f.post(t, "canceled", "customer.subscription.updated", f.stripeHas(f.houseSub("canceled", start, end)))

	assert.False(t, f.hasHouse(t), "the entitlement's subscription was cancelled: cleared")
}

// RP22-2 / WP-PAY-7 (D-P23-3): the deletion of a NON-house subscription (a
// legacy pack, no entitlement row) is ignored, flag or no flag: the record
// holds houses only and the webhook writes no tenants column — the plan
// label is the operator's (support moves a pack to a house).
func TestSubscriptionDeleted_PackWithNoRow_IsIgnored(t *testing.T) {
	// Arrange: a tenant on a pack label, flag on.
	f := setupHouseFixture(t)
	f.handler.SetFlags(lapseFlag{f.tenantID: true})
	pack := subWith("sub_"+f.tenantID, "canceled", item("si_pack", "price_vault3_legacy", 1))
	pack.Customer.ID = "cus_" + f.tenantID
	_, err := f.db.Exec(`UPDATE tenants SET plan = 'vault3' WHERE id = $1`, f.tenantID)
	require.NoError(t, err)

	// Act
	f.post(t, "pack-deleted", "customer.subscription.deleted", pack)

	// Assert
	plan, state, _, _ := f.record(t)
	assert.Equal(t, "vault3", plan, "the label is not the webhook's to change")
	assert.Equal(t, "none", state, "no entitlement was ever written for a pack")
}
