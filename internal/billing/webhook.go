package billing

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/stripe/stripe-go/v75"
)

type WebhookHandler struct {
	endpointSecret string
}

func NewWebhookHandler(secret string) *WebhookHandler {
	return &WebhookHandler{endpointSecret: secret}
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var event stripe.Event
	if err := json.Unmarshal(payload, &event); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	switch event.Type {
	case "checkout.session.completed":
		// Activate subscription
	case "invoice.payment_failed":
		// Handle failed payment
	}

	w.WriteHeader(http.StatusOK)
}

type OverageService struct {
	graceStartTimes map[string]time.Time
}

func (s *OverageService) CheckOverage(tenantID string, usedBytes, limitBytes int64) string {
	if usedBytes <= limitBytes {
		return "OK"
	}

	if _, exists := s.graceStartTimes[tenantID]; !exists {
		if s.graceStartTimes == nil {
			s.graceStartTimes = make(map[string]time.Time)
		}
		s.graceStartTimes[tenantID] = time.Now()
	}

	return "GRACE_PERIOD"
}

func (s *OverageService) ShouldAutoUpgrade(tenantID string, graceDuration time.Duration) bool {
	startTime, exists := s.graceStartTimes[tenantID]
	if !exists {
		return false
	}

	return time.Since(startTime) >= graceDuration
}
