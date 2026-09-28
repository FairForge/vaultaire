package billing

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
	"github.com/stripe/stripe-go/v75/webhook"
	"go.uber.org/zap"
)

// Review R10: every webhook test signs its payload the way Stripe does
// (scheme v1: HMAC-SHA256 over "<t>.<body>", header "t=<t>,v1=<hex>"), with
// the event pinned to the API version this SDK deserialises. Before this
// file the handler was only ever exercised with an empty secret, which
// skipped verification altogether (R10-01).

const testWebhookSecret = "whsec_r10_test_secret"

// signedRequest builds a POST /webhook/stripe carrying a v1 signature made
// at `at` with `secret` over body.
func signedRequest(t *testing.T, secret string, at time.Time, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", "/webhook/stripe", bytes.NewReader(body))
	sig := webhook.ComputeSignature(at, body, secret)
	req.Header.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(sig)))
	return req
}

// eventBody serialises a Stripe event of the given type around obj, at the
// SDK's pinned API version.
func eventBody(t *testing.T, id, typ string, obj any) []byte {
	t.Helper()
	raw, err := json.Marshal(obj)
	require.NoError(t, err)
	ev := stripe.Event{ID: id, APIVersion: stripe.APIVersion, Type: stripe.EventType(typ),
		Created: time.Now().Unix(), Data: &stripe.EventData{Raw: raw}}
	body, err := json.Marshal(ev)
	require.NoError(t, err)
	return body
}

func newSignedHandler(db *sql.DB) *WebhookHandler {
	svc := NewStripeService("sk_test_fake", db, zap.NewNop())
	return NewWebhookHandler(testWebhookSecret, svc, zap.NewNop())
}

func TestWebhook_SignatureVerification(t *testing.T) {
	h := newSignedHandler(nil)
	body := eventBody(t, "evt_sig_1", "balance.available", map[string]any{})

	// No signature at all.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/webhook/stripe", bytes.NewReader(body)))
	assert.Equal(t, http.StatusBadRequest, w.Code, "unsigned")

	// Signed with the wrong secret.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(t, "whsec_other", time.Now(), body))
	assert.Equal(t, http.StatusBadRequest, w.Code, "wrong secret")

	// Signed, but outside the 5-minute tolerance (replay).
	w = httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(t, testWebhookSecret, time.Now().Add(-10*time.Minute), body))
	assert.Equal(t, http.StatusBadRequest, w.Code, "stale timestamp")

	// Signed over a different body than the one sent.
	w = httptest.NewRecorder()
	tampered := signedRequest(t, testWebhookSecret, time.Now(), body)
	tampered.Body = http.NoBody
	tampered.Body = io.NopCloser(strings.NewReader(strings.Replace(string(body), "balance.available", "invoice.paid", 1)))
	h.ServeHTTP(w, tampered)
	assert.Equal(t, http.StatusBadRequest, w.Code, "tampered body")

	// The real thing.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(t, testWebhookSecret, time.Now(), body))
	assert.Equal(t, http.StatusOK, w.Code)
}

// stripe-go v75 refuses events whose api_version differs from the one it
// was built against (webhook/client.go: ConstructEventWithOptions). The
// endpoint in the Stripe dashboard must therefore be pinned to
// stripe.APIVersion, or every delivery is a 400 (R10-03).
func TestWebhook_RejectsForeignAPIVersion(t *testing.T) {
	h := newSignedHandler(nil)
	ev := stripe.Event{ID: "evt_ver_1", APIVersion: "2025-08-27.basil", Type: "balance.available",
		Data: &stripe.EventData{Raw: json.RawMessage(`{}`)}}
	body, err := json.Marshal(ev)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(t, testWebhookSecret, time.Now(), body))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "2023-08-16", stripe.APIVersion, "the pin the Stripe endpoint must be created with")
}

// An empty endpoint secret must never mean "trust everyone" (R10-01).
func TestWebhook_EmptySecretRefusesToServe(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	h := NewWebhookHandler("", svc, zap.NewNop())
	body := eventBody(t, "evt_nosecret", "invoice.payment_succeeded", stripe.Invoice{Customer: &stripe.Customer{ID: "cus_x"}})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/webhook/stripe", bytes.NewReader(body)))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// Stripe puts every invoice line, discount and previous_attributes in the
// event; the old 64 KB LimitReader silently truncated and then failed the
// signature (R10-04).
func TestWebhook_LargeSignedBodyIsAccepted(t *testing.T) {
	h := newSignedHandler(nil)
	pad := strings.Repeat("x", 70<<10)
	body := eventBody(t, "evt_big", "invoice.upcoming", map[string]any{"object": "invoice", "description": pad})
	require.Greater(t, len(body), 65536)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(t, testWebhookSecret, time.Now(), body))
	assert.Equal(t, http.StatusOK, w.Code)

	// Past the new cap the request is refused outright, never truncated.
	huge := eventBody(t, "evt_huge", "invoice.upcoming", map[string]any{"object": "invoice", "description": strings.Repeat("y", maxWebhookBodyBytes)})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(t, testWebhookSecret, time.Now(), huge))
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

// A handler that could not apply the event must answer non-2xx and must NOT
// record the event id, so Stripe retries it (R10-02). Before: 200 + recorded
// = a paid house that never arrives.
func TestWebhook_HandlerFailureIsLeftForStripeToRetry(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tenantID := fmt.Sprintf("house-retry-%d", time.Now().UnixNano())
	_, err = db.Exec(`INSERT INTO tenants (id, name, email, stripe_customer_id) VALUES ($1, 'Retry', $2, $3)`,
		tenantID, tenantID+"@test.local", "cus_"+tenantID)
	require.NoError(t, err)
	qm := usage.NewQuotaManager(db)
	require.NoError(t, qm.CreateTenant(context.Background(), tenantID, "free", usage.FreeTierLimits.StorageBytes))
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM stripe_events WHERE event_id LIKE $1`, "evt_"+tenantID+"%")
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
	})

	svc := NewStripeService("sk_test_fake", db, zap.NewNop())
	svc.ConfigureHouse(testIDs, goodPrices())
	subs := fakeSubs{}
	h := NewWebhookHandler(testWebhookSecret, svc, zap.NewNop())
	h.SetHouseQuotas(qm)
	h.subs = subs

	recorded := func(id string) bool {
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM stripe_events WHERE event_id = $1`, id).Scan(&n))
		return n == 1
	}

	// 1. A checkout for a customer we do not know: the tenant lookup fails.
	evID := "evt_" + tenantID + "_unknown"
	body := eventBody(t, evID, "checkout.session.completed", stripe.CheckoutSession{
		ID: "cs_u", Customer: &stripe.Customer{ID: "cus_nobody"}, Subscription: &stripe.Subscription{ID: "sub_u"}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(t, testWebhookSecret, time.Now(), body))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.False(t, recorded(evID), "a failed event is not recorded, so the retry is handled")

	// 2. The subscription cannot be fetched (Stripe blip): same.
	evID = "evt_" + tenantID + "_nosub"
	body = eventBody(t, evID, "checkout.session.completed", stripe.CheckoutSession{
		ID: "cs_n", Customer: &stripe.Customer{ID: "cus_" + tenantID}, Subscription: &stripe.Subscription{ID: "sub_missing"}})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(t, testWebhookSecret, time.Now(), body))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.False(t, recorded(evID))
	_, ok, _ := qm.GetHouse(context.Background(), tenantID)
	assert.False(t, ok, "nothing granted")

	// 3. The retry, once the subscription is readable, grants the house and is recorded.
	sub := subWith("sub_missing", "active", item("si_1", "price_std_y", 2))
	sub.Customer.ID = "cus_" + tenantID
	subs[sub.ID] = sub
	w = httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(t, testWebhookSecret, time.Now(), body))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, recorded(evID))
	house, ok, err := qm.GetHouse(context.Background(), tenantID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, usage.HouseFromTB(2, 0, 0), house)

	// 4. A replay of the recorded event is a no-op 200.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(t, testWebhookSecret, time.Now(), body))
	assert.Equal(t, http.StatusOK, w.Code)
}
