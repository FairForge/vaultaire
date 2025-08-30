package billing

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStripeWebhook_HandlePaymentSuccess(t *testing.T) {
	handler := NewWebhookHandler("whsec_test")

	payload := `{
        "type": "checkout.session.completed",
        "data": {
            "object": {
                "id": "cs_test_123",
                "customer": "cus_123",
                "amount_total": 1299
            }
        }
    }`

	req := httptest.NewRequest("POST", "/webhook/stripe", bytes.NewBufferString(payload))
	req.Header.Set("Stripe-Signature", "test_sig")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}
}

func TestOverageHandler_48HourGrace(t *testing.T) {
	service := &OverageService{}

	// 1TB in bytes
	oneTB := int64(1024 * 1024 * 1024 * 1024)
	// 1.1TB (10% over limit)
	overageBytes := oneTB + (oneTB / 10)

	action := service.CheckOverage("tenant-123", overageBytes, oneTB)

	if action != "GRACE_PERIOD" {
		t.Errorf("Expected GRACE_PERIOD, got %s", action)
	}
}

func TestOverageHandler_AutoUpgrade(t *testing.T) {
	service := &OverageService{
		graceStartTimes: map[string]time.Time{
			"tenant-123": time.Now().Add(-49 * time.Hour),
		},
	}

	shouldUpgrade := service.ShouldAutoUpgrade("tenant-123", 48*time.Hour)

	if !shouldUpgrade {
		t.Error("Should auto-upgrade after 48 hour grace period")
	}
}
