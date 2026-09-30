package billing

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

// WP-R10-3: the account-deletion runner cancels the Stripe subscription
// (stage a) and erases the tenant row minutes later (stage c). Stripe's
// customer.subscription.deleted delivery can land on either side of the
// erase. Before: the house is cleared and the row downgraded; the runner's
// erase then removes the row without conflict. After: the customer id and
// the metadata hint resolve to nobody — the R10-45 "not ours" path answers
// 200 and records the event so Stripe stops retrying (it used to 500 for
// three days, R10-45 source (c)).

func (f *houseFixture) scheduleUser(t *testing.T) string {
	t.Helper()
	userID := "00000000-0000-4000-8000-" + f.tenantID[len(f.tenantID)-12:]
	_, err := f.db.Exec(`INSERT INTO users (id, email, password_hash, company, status, deletion_scheduled_at, created_at, updated_at)
		VALUES ($1, $2, 'x', 'House', 'pending_deletion', NOW() - INTERVAL '1 hour', NOW(), NOW())`, userID, f.tenantID+"@test.local")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM users WHERE id::text = $1`, userID) })
	return userID
}

func TestWebhook_SubscriptionDeletedBeforeTheEraseClearsTheHouseThenEraseSucceeds(t *testing.T) {
	f := setupHouseFixture(t)
	userID := f.scheduleUser(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "active", item("si_1", "price_std_y", 4))
	sub.Customer.ID = cus
	f.subs[sub.ID] = sub
	f.post(t, "checkout", "checkout.session.completed", stripe.CheckoutSession{
		ID: "cs_1", Customer: &stripe.Customer{ID: cus}, Subscription: &stripe.Subscription{ID: sub.ID}})
	_, ok, err := f.qm.GetHouse(context.Background(), f.tenantID)
	require.NoError(t, err)
	require.True(t, ok, "fixture: the house is applied")

	// The runner cancelled at Stripe (stage a); the delivery arrives before
	// the erase (stage c).
	sub.Status = stripe.SubscriptionStatusCanceled
	f.post(t, "deleted_before", "customer.subscription.deleted", sub)
	_, ok, err = f.qm.GetHouse(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.False(t, ok, "ClearHouse ran on the delivery")
	plan, status, subID, _ := f.tenantRow(t)
	assert.Equal(t, "starter/canceled/", plan+"/"+status+"/"+subID)

	// Stage c does not conflict with what the webhook wrote.
	res, err := account.NewService(f.db, zap.NewNop()).EraseRows(context.Background(), userID, f.tenantID, f.tenantID+"@test.local", time.Now())
	require.NoError(t, err)
	assert.Greater(t, res.Total, int64(0))
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenants WHERE id = $1`, f.tenantID).Scan(&n))
	assert.Zero(t, n)
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenant_floor_quotas WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	assert.Zero(t, n)
}

func TestWebhook_SubscriptionDeletedAfterTheEraseIsAcknowledged(t *testing.T) {
	f := setupHouseFixture(t)
	userID := f.scheduleUser(t)
	cus := "cus_" + f.tenantID
	sub := subWith("sub_"+f.tenantID, "canceled", item("si_1", "price_std_y", 4))
	sub.Customer.ID = cus
	sub.Metadata = map[string]string{"tenant_id": f.tenantID, "house": "1"} // what houseCheckoutParams stamps

	// The erase ran first: the tenant row is gone.
	_, err := account.NewService(f.db, zap.NewNop()).EraseRows(context.Background(), userID, f.tenantID, f.tenantID+"@test.local", time.Now())
	require.NoError(t, err)

	code := f.postCode(t, "deleted_after", "customer.subscription.deleted", sub)
	assert.Equal(t, http.StatusOK, code, "an erased tenant's delivery is not ours: 200, never a three-day retry")
	assert.True(t, f.eventRecorded(t, "deleted_after"), "recorded so a redelivery is a dedup hit")
	var tid sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT tenant_id FROM stripe_events WHERE event_id = $1`, "evt_"+f.tenantID+"_deleted_after").Scan(&tid))
	assert.False(t, tid.Valid, "no tenant reference is written for an event that resolved to nobody")
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenants WHERE id = $1`, f.tenantID).Scan(&n))
	assert.Zero(t, n, "the not-ours path never re-creates or heals a row")
}

// A run that crashed after Stripe cancelled but before the runner's stamp
// repeats the cancel the next day. Stripe refuses to cancel a cancelled
// subscription, so CancelSubscription reads the status first and reports an
// already-cancelled one as success.
func TestCancelSubscription_AlreadyCancelledIsSuccess(t *testing.T) {
	f := setupHouseFixture(t)
	subID := "sub_cancelled_" + f.tenantID
	_, err := f.db.Exec(`UPDATE tenants SET stripe_subscription_id = $1, subscription_status = 'active' WHERE id = $2`, subID, f.tenantID)
	require.NoError(t, err)
	svc := NewStripeService("sk_test_fake", f.db, zap.NewNop())
	svc.subFetcher = fakeSubs{subID: subWith(subID, "canceled")}

	require.NoError(t, svc.CancelSubscription(context.Background(), f.tenantID), "no Cancel call is made; the status is the verdict")
	_, status, _, _ := f.tenantRow(t)
	assert.Equal(t, "canceled", status)

	// The fetch failing is an error (retry tomorrow), never a silent skip.
	svc.subFetcher = fakeSubs{}
	assert.Error(t, svc.CancelSubscription(context.Background(), f.tenantID))
}
