package billing

// RP11-1 / RP11-1b / RP11-2 (WP-P11 residuals): tenants.subscription_status
// is written only for the tenant's current subscription — the invoice events
// and the fetched subscription alike — and the claim row records the tenant
// the event was applied to.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
)

func invoiceFor(id, customer, subscription string) stripe.Invoice {
	inv := stripe.Invoice{ID: id, Customer: &stripe.Customer{ID: customer}}
	if subscription != "" {
		inv.Subscription = &stripe.Subscription{ID: subscription}
	}
	return inv
}

func TestWebhook_StalePaymentSucceededAfterDelete_DoesNotMarkTheTenantActive(t *testing.T) {
	// Arrange: a house, then Stripe deletes the subscription.
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_std_y", 3))
	sub.Customer.ID = cus
	require.Equal(t, http.StatusOK, f.postCode(t, "created", "customer.subscription.created", f.stripeHas(sub)))
	deleted := *sub
	deleted.Status = stripe.SubscriptionStatusCanceled
	require.Equal(t, http.StatusOK, f.postCode(t, "deleted", "customer.subscription.deleted", f.stripeHas(&deleted)))
	_, status, subID, _ := f.tenantRow(t)
	require.Equal(t, "canceled", status)
	require.Empty(t, subID)

	// Act: the subscription's last invoice.payment_succeeded arrives late.
	code := f.postCode(t, "paid_stale", "invoice.payment_succeeded", invoiceFor("in_1", cus, sub.ID))

	// Assert
	assert.Equal(t, http.StatusOK, code)
	plan, status, subID, _ := f.tenantRow(t)
	assert.Equal(t, "canceled", status, "a stale invoice for a deleted subscription marked a starter tenant active")
	assert.Equal(t, "starter", plan)
	assert.Empty(t, subID, "an invoice never adopts a subscription")
}

func TestWebhook_StalePaymentFailedForAnOldSubscription_LeavesTheCurrentStatus(t *testing.T) {
	// Arrange: the tenant is on S2.
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	s2 := subWith("sub2_"+f.tenantID, "active", item("si_2", "price_std_y", 4))
	s2.Customer.ID = cus
	require.Equal(t, http.StatusOK, f.postCode(t, "created_s2", "customer.subscription.created", f.stripeHas(s2)))

	// Act: a failed invoice of the old S1.
	code := f.postCode(t, "failed_s1", "invoice.payment_failed", invoiceFor("in_old", cus, "sub1_"+f.tenantID))

	// Assert
	assert.Equal(t, http.StatusOK, code)
	_, status, subID, _ := f.tenantRow(t)
	assert.Equal(t, "active", status, "an old subscription's failed invoice marked the current one past_due")
	assert.Equal(t, s2.ID, subID)
}

func TestWebhook_PaymentEventsOfTheCurrentSubscription_StillWriteTheStatus(t *testing.T) {
	// Arrange
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_std_y", 2))
	sub.Customer.ID = cus
	require.Equal(t, http.StatusOK, f.postCode(t, "created", "customer.subscription.created", f.stripeHas(sub)))

	// Act + Assert: failed → past_due, then paid → active.
	require.Equal(t, http.StatusOK, f.postCode(t, "failed", "invoice.payment_failed", invoiceFor("in_1", cus, sub.ID)))
	_, status, _, _ := f.tenantRow(t)
	assert.Equal(t, "past_due", status)
	require.Equal(t, http.StatusOK, f.postCode(t, "paid", "invoice.payment_succeeded", invoiceFor("in_2", cus, sub.ID)))
	_, status, _, _ = f.tenantRow(t)
	assert.Equal(t, "active", status)
}

func TestWebhook_StaleUpdatedForAnOldSubscription_DoesNotOverwriteTheCurrentStatus(t *testing.T) {
	// Arrange: the tenant is on S2 (a house, active); S1 is cancelled at Stripe.
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	s2 := subWith("sub2_"+f.tenantID, "active", item("si_2", "price_std_y", 4))
	s2.Customer.ID = cus
	require.Equal(t, http.StatusOK, f.postCode(t, "created_s2", "customer.subscription.created", f.stripeHas(s2)))
	s1 := subWith("sub1_"+f.tenantID, "canceled", item("si_1", "price_std_y", 1))
	s1.Customer.ID = cus
	s1.Metadata = map[string]string{"plan": "vault3"}
	payload := f.stripeHas(s1)

	// Act: a stale updated for S1 (fetches canceled, with a plan in its metadata).
	code := f.postCode(t, "upd_s1_stale", "customer.subscription.updated", payload)

	// Assert: the row is S2's; the house stays.
	assert.Equal(t, http.StatusOK, code)
	plan, status, subID, _ := f.tenantRow(t)
	assert.Equal(t, "active", status, "a stale subscription's status overwrote the current one")
	assert.Equal(t, "house", plan, "a stale subscription's plan is not applied")
	assert.Equal(t, s2.ID, subID)
	std, ok := f.houseTB(t)
	assert.True(t, ok)
	assert.Equal(t, 4, std)
}

func TestWebhook_CreatedBeforeCheckout_AdoptsTheSubscription(t *testing.T) {
	// Arrange: a legacy pack subscription (no house price), the tenant has
	// none recorded; customer.subscription.created arrives before
	// checkout.session.completed.
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_legacy_pack", 1))
	sub.Customer.ID = cus
	sub.Metadata = map[string]string{"plan": "vault3"}

	// Act
	code := f.postCode(t, "created", "customer.subscription.created", f.stripeHas(sub))

	// Assert
	assert.Equal(t, http.StatusOK, code)
	plan, status, subID, _ := f.tenantRow(t)
	assert.Equal(t, "active", status)
	assert.Equal(t, "vault3", plan)
	assert.Equal(t, sub.ID, subID, "the first live subscription is the tenant's current one")
}

func TestWebhook_CanceledSubscriptionIsNeverAdopted(t *testing.T) {
	// Arrange: the subscription was deleted (id cleared); a stale updated for
	// it fetches canceled.
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_legacy_pack", 1))
	sub.Customer.ID = cus
	require.Equal(t, http.StatusOK, f.postCode(t, "created", "customer.subscription.created", f.stripeHas(sub)))
	canceled := *sub
	canceled.Status = stripe.SubscriptionStatusCanceled
	require.Equal(t, http.StatusOK, f.postCode(t, "deleted", "customer.subscription.deleted", f.stripeHas(&canceled)))

	// Act
	code := f.postCode(t, "upd_stale", "customer.subscription.updated", sub)

	// Assert
	assert.Equal(t, http.StatusOK, code)
	plan, status, subID, _ := f.tenantRow(t)
	assert.Equal(t, "canceled", status)
	assert.Equal(t, "starter", plan)
	assert.Empty(t, subID, "a terminal subscription is not adopted")
}

func TestWebhook_ClaimRowRecordsTheResolvedTenant(t *testing.T) {
	// Arrange
	f := setupHouseFixture(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_std_y", 1))
	sub.Customer.ID = cus

	// Act: one event of ours, one of a customer that is not.
	require.Equal(t, http.StatusOK, f.postCode(t, "created", "customer.subscription.created", f.stripeHas(sub)))
	require.Equal(t, http.StatusOK, f.postCode(t, "foreign", "invoice.payment_succeeded",
		invoiceFor("in_x", "cus_nobody_"+f.tenantID, "")))

	// Assert: the support page's per-tenant query finds ours.
	var types []string
	rows, err := f.db.Query(`SELECT event_type FROM stripe_events WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var typ string
		require.NoError(t, rows.Scan(&typ))
		types = append(types, typ)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"customer.subscription.created"}, types)
	var foreignTenant *string
	require.NoError(t, f.db.QueryRow(`SELECT tenant_id FROM stripe_events WHERE event_id = $1`,
		"evt_"+f.tenantID+"_foreign").Scan(&foreignTenant))
	assert.Nil(t, foreignTenant, "an event that is not ours names no tenant")
}
