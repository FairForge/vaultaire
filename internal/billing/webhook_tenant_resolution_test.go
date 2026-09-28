package billing

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
)

// Post-merge review of R10 (R10-45): an event whose customer is not in
// `tenants` used to answer 500 forever — Stripe retries for three days, and
// a paid house whose stripe_customer_id persist failed at checkout could
// never be applied. Now: the tenant is resolved from the signed payload's
// own hints (client_reference_id, metadata.tenant_id) and the customer id
// healed; a customer that is nobody's is acknowledged and recorded.

func (f *houseFixture) postCode(t *testing.T, suffix, typ string, obj any) int {
	t.Helper()
	body := eventBody(t, "evt_"+f.tenantID+"_"+suffix, typ, obj)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, signedRequest(t, testWebhookSecret, time.Now(), body))
	return w.Code
}

func (f *houseFixture) eventRecorded(t *testing.T, suffix string) bool {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM stripe_events WHERE event_id = $1`,
		"evt_"+f.tenantID+"_"+suffix).Scan(&n))
	return n == 1
}

func TestWebhook_UnknownCustomerIsAcknowledgedNotRetried(t *testing.T) {
	f := setupHouseFixture(t)
	cus := "cus_nobody_" + f.tenantID
	planBefore, statusBefore, _, _ := f.tenantRow(t)

	for _, tc := range []struct {
		suffix, typ string
		obj         any
	}{
		{"unknown_checkout", "checkout.session.completed", stripe.CheckoutSession{
			ID: "cs_u", Customer: &stripe.Customer{ID: cus}, Subscription: &stripe.Subscription{ID: "sub_u"}}},
		{"unknown_paid", "invoice.payment_succeeded", stripe.Invoice{ID: "in_u", Customer: &stripe.Customer{ID: cus}}},
		{"unknown_failed", "invoice.payment_failed", stripe.Invoice{ID: "in_u2", Customer: &stripe.Customer{ID: cus}}},
		{"unknown_updated", "customer.subscription.updated", stripe.Subscription{
			ID: "sub_u", Status: stripe.SubscriptionStatusActive, Customer: &stripe.Customer{ID: cus}}},
		{"unknown_deleted", "customer.subscription.deleted", stripe.Subscription{
			ID: "sub_u", Status: stripe.SubscriptionStatusCanceled, Customer: &stripe.Customer{ID: cus}}},
	} {
		assert.Equal(t, http.StatusOK, f.postCode(t, tc.suffix, tc.typ, tc.obj), tc.typ)
		assert.True(t, f.eventRecorded(t, tc.suffix), "%s recorded so Stripe stops retrying", tc.typ)
	}
	plan, status, _, _ := f.tenantRow(t)
	assert.Equal(t, planBefore+"/"+statusBefore, plan+"/"+status, "the fixture tenant was not touched")
}

func TestWebhook_CheckoutResolvesTenantFromClientReferenceID(t *testing.T) {
	f := setupHouseFixture(t)
	// The persist of stripe_customer_id failed at checkout (CreateCustomer
	// logs and continues), so the tenant row does not know its customer.
	_, err := f.db.Exec(`UPDATE tenants SET stripe_customer_id = NULL WHERE id = $1`, f.tenantID)
	require.NoError(t, err)
	cus := "cus_unpersisted_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_std_y", 2))
	sub.Customer.ID = cus
	f.subs[sub.ID] = sub

	code := f.postCode(t, "ref_checkout", "checkout.session.completed", stripe.CheckoutSession{
		ID: "cs_ref", Customer: &stripe.Customer{ID: cus}, Subscription: &stripe.Subscription{ID: sub.ID},
		ClientReferenceID: f.tenantID})
	require.Equal(t, http.StatusOK, code)

	house, ok, err := f.qm.GetHouse(context.Background(), f.tenantID)
	require.NoError(t, err)
	require.True(t, ok, "the paid house is applied")
	assert.Equal(t, 2, house.StdTB())

	var stored sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT stripe_customer_id FROM tenants WHERE id = $1`, f.tenantID).Scan(&stored))
	assert.Equal(t, cus, stored.String, "the customer id is healed so later events resolve directly")
}

func TestWebhook_SubscriptionEventsResolveTenantFromMetadata(t *testing.T) {
	f := setupHouseFixture(t)
	_, err := f.db.Exec(`UPDATE tenants SET stripe_customer_id = NULL WHERE id = $1`, f.tenantID)
	require.NoError(t, err)
	cus := "cus_meta_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_std_y", 3))
	sub.Customer.ID = cus
	sub.Metadata = map[string]string{"tenant_id": f.tenantID, "house": "1"}

	require.Equal(t, http.StatusOK, f.postCode(t, "meta_updated", "customer.subscription.updated", sub))
	house, ok, err := f.qm.GetHouse(context.Background(), f.tenantID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 3, house.StdTB())

	// A metadata hint that names a tenant that does not exist is not ours.
	other := subWith("sub_other", "active", item("si_1", "price_std_y", 1))
	other.Customer.ID = "cus_ghost_" + f.tenantID
	other.Metadata = map[string]string{"tenant_id": fmt.Sprintf("no-such-tenant-%d", time.Now().UnixNano())}
	assert.Equal(t, http.StatusOK, f.postCode(t, "meta_ghost", "customer.subscription.updated", other))
	assert.True(t, f.eventRecorded(t, "meta_ghost"))
}
