package billing

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

func TestWebhookHandler_InvalidBody(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	handler := NewWebhookHandler("", svc, zap.NewNop())

	req := httptest.NewRequest("POST", "/webhook/stripe", bytes.NewBufferString("not json"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestWebhookHandler_UnhandledEvent(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	handler := NewWebhookHandler("", svc, zap.NewNop())

	event := stripe.Event{
		ID:   "evt_test",
		Type: "balance.available",
		Data: &stripe.EventData{Raw: json.RawMessage(`{}`)},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest("POST", "/webhook/stripe", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestWebhookHandler_CheckoutCompleted_NoDB(t *testing.T) {
	// Without a DB, the handler logs an error but still returns 200.
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	handler := NewWebhookHandler("", svc, zap.NewNop())

	session := stripe.CheckoutSession{
		Customer:     &stripe.Customer{ID: "cus_test"},
		Subscription: &stripe.Subscription{ID: "sub_test"},
	}
	sessionJSON, _ := json.Marshal(session)

	event := stripe.Event{
		ID:   "evt_checkout",
		Type: "checkout.session.completed",
		Data: &stripe.EventData{Raw: sessionJSON},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest("POST", "/webhook/stripe", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should not crash — returns 200 even though DB lookup fails.
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestWebhookHandler_PaymentFailed_NoDB(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	handler := NewWebhookHandler("", svc, zap.NewNop())

	inv := stripe.Invoice{
		Customer: &stripe.Customer{ID: "cus_test"},
	}
	inv.ID = "in_test"
	invJSON, _ := json.Marshal(inv)

	event := stripe.Event{
		ID:   "evt_fail",
		Type: "invoice.payment_failed",
		Data: &stripe.EventData{Raw: invJSON},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest("POST", "/webhook/stripe", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestOverageHandler_GracePeriod(t *testing.T) {
	service := &OverageService{}

	oneTB := int64(1024 * 1024 * 1024 * 1024)
	overageBytes := oneTB + (oneTB / 10)

	action := service.CheckOverage("tenant-123", overageBytes, oneTB)
	assert.Equal(t, "GRACE_PERIOD", action)

	action = service.CheckOverage("tenant-123", oneTB/2, oneTB)
	assert.Equal(t, "OK", action)
}

func TestOverageHandler_AutoUpgrade(t *testing.T) {
	service := &OverageService{
		graceStartTimes: map[string]time.Time{
			"tenant-123": time.Now().Add(-49 * time.Hour),
		},
	}

	assert.True(t, service.ShouldAutoUpgrade("tenant-123", 48*time.Hour))
	assert.False(t, service.ShouldAutoUpgrade("tenant-456", 48*time.Hour))
}
