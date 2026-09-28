package billing

import (
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
	handler := NewWebhookHandler(testWebhookSecret, svc, zap.NewNop())

	req := signedRequest(t, testWebhookSecret, time.Now(), []byte("not json"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestWebhookHandler_UnhandledEvent(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	handler := NewWebhookHandler(testWebhookSecret, svc, zap.NewNop())

	body := eventBody(t, "evt_test", "balance.available", map[string]any{})

	req := signedRequest(t, testWebhookSecret, time.Now(), body)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestWebhookHandler_CheckoutCompleted_NoDB(t *testing.T) {
	// Without a DB the tenant lookup fails; the event must be left for
	// Stripe to retry (non-2xx), never swallowed as 200 (R10-02).
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	handler := NewWebhookHandler(testWebhookSecret, svc, zap.NewNop())

	body := eventBody(t, "evt_checkout", "checkout.session.completed", stripe.CheckoutSession{
		Customer:     &stripe.Customer{ID: "cus_test"},
		Subscription: &stripe.Subscription{ID: "sub_test"},
	})

	req := signedRequest(t, testWebhookSecret, time.Now(), body)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code, "Stripe retries non-2xx")
}

func TestWebhookHandler_PaymentFailed_NoDB(t *testing.T) {
	svc := NewStripeService("sk_test_fake", nil, zap.NewNop())
	handler := NewWebhookHandler(testWebhookSecret, svc, zap.NewNop())

	inv := stripe.Invoice{
		Customer: &stripe.Customer{ID: "cus_test"},
	}
	inv.ID = "in_test"
	body := eventBody(t, "evt_fail", "invoice.payment_failed", inv)

	req := signedRequest(t, testWebhookSecret, time.Now(), body)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code, "no DB = tenant lookup failed = retry")
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
