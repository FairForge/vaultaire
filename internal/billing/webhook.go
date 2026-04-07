package billing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/stripe/stripe-go/v75"
	"github.com/stripe/stripe-go/v75/webhook"
	"go.uber.org/zap"
)

// WebhookHandler processes Stripe webhook events with signature
// verification and persists subscription state to the database.
type WebhookHandler struct {
	endpointSecret string
	stripe         *StripeService
	logger         *zap.Logger
}

// NewWebhookHandler creates a webhook handler. The endpointSecret is the
// whsec_... value from the Stripe Dashboard webhook endpoint config.
func NewWebhookHandler(secret string, stripeSvc *StripeService, logger *zap.Logger) *WebhookHandler {
	return &WebhookHandler{
		endpointSecret: secret,
		stripe:         stripeSvc,
		logger:         logger,
	}
}

// ServeHTTP handles POST /webhook/stripe.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const maxBodyBytes = 65536
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		h.logger.Error("read webhook body", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Verify signature when endpoint secret is configured.
	var event stripe.Event
	if h.endpointSecret != "" {
		event, err = webhook.ConstructEvent(body, r.Header.Get("Stripe-Signature"), h.endpointSecret)
		if err != nil {
			h.logger.Warn("webhook signature verification failed", zap.Error(err))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	} else {
		if err := json.Unmarshal(body, &event); err != nil {
			h.logger.Error("unmarshal webhook event", zap.Error(err))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}

	ctx := r.Context()

	h.logger.Info("stripe webhook received",
		zap.String("type", string(event.Type)),
		zap.String("id", event.ID))

	switch event.Type {
	case "checkout.session.completed":
		h.handleCheckoutCompleted(ctx, event)
	case "invoice.payment_succeeded":
		h.handlePaymentSucceeded(ctx, event)
	case "invoice.payment_failed":
		h.handlePaymentFailed(ctx, event)
	case "customer.subscription.updated":
		h.handleSubscriptionUpdated(ctx, event)
	case "customer.subscription.deleted":
		h.handleSubscriptionDeleted(ctx, event)
	default:
		h.logger.Debug("unhandled webhook event", zap.String("type", string(event.Type)))
	}

	w.WriteHeader(http.StatusOK)
}

func (h *WebhookHandler) handleCheckoutCompleted(ctx context.Context, event stripe.Event) {
	var session stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
		h.logger.Error("unmarshal checkout session", zap.Error(err))
		return
	}

	customerID := ""
	if session.Customer != nil {
		customerID = session.Customer.ID
	}
	subscriptionID := ""
	if session.Subscription != nil {
		subscriptionID = session.Subscription.ID
	}

	if customerID == "" || subscriptionID == "" {
		h.logger.Warn("checkout session missing customer or subscription",
			zap.String("session", session.ID))
		return
	}

	tenantID, err := h.stripe.LookupTenantByCustomer(ctx, customerID)
	if err != nil {
		h.logger.Error("lookup tenant for checkout", zap.String("customer", customerID), zap.Error(err))
		return
	}

	// Determine plan from checkout metadata or default to "standard".
	plan := "standard"
	if session.Metadata != nil {
		if p, ok := session.Metadata["plan"]; ok {
			plan = p
		}
	}

	if err := h.stripe.SaveSubscription(ctx, tenantID, subscriptionID, "active", plan); err != nil {
		h.logger.Error("save subscription after checkout", zap.Error(err))
		return
	}

	h.logger.Info("subscription activated",
		zap.String("tenant", tenantID),
		zap.String("subscription", subscriptionID),
		zap.String("plan", plan))
}

func (h *WebhookHandler) handlePaymentSucceeded(ctx context.Context, event stripe.Event) {
	var inv stripe.Invoice
	if err := json.Unmarshal(event.Data.Raw, &inv); err != nil {
		h.logger.Error("unmarshal invoice", zap.Error(err))
		return
	}

	customerID := ""
	if inv.Customer != nil {
		customerID = inv.Customer.ID
	}
	if customerID == "" {
		return
	}

	tenantID, err := h.stripe.LookupTenantByCustomer(ctx, customerID)
	if err != nil {
		h.logger.Error("lookup tenant for payment", zap.String("customer", customerID), zap.Error(err))
		return
	}

	// Ensure subscription status is active after successful payment.
	if h.stripe.db != nil {
		_, _ = h.stripe.db.ExecContext(ctx,
			`UPDATE tenants SET subscription_status = 'active' WHERE id = $1`, tenantID)
	}

	h.logger.Info("payment succeeded",
		zap.String("tenant", tenantID),
		zap.String("invoice", inv.ID))
}

func (h *WebhookHandler) handlePaymentFailed(ctx context.Context, event stripe.Event) {
	var inv stripe.Invoice
	if err := json.Unmarshal(event.Data.Raw, &inv); err != nil {
		h.logger.Error("unmarshal invoice", zap.Error(err))
		return
	}

	customerID := ""
	if inv.Customer != nil {
		customerID = inv.Customer.ID
	}
	if customerID == "" {
		return
	}

	tenantID, err := h.stripe.LookupTenantByCustomer(ctx, customerID)
	if err != nil {
		h.logger.Error("lookup tenant for failed payment", zap.String("customer", customerID), zap.Error(err))
		return
	}

	if h.stripe.db != nil {
		_, _ = h.stripe.db.ExecContext(ctx,
			`UPDATE tenants SET subscription_status = 'past_due' WHERE id = $1`, tenantID)
	}

	h.logger.Warn("payment failed",
		zap.String("tenant", tenantID),
		zap.String("invoice", inv.ID))
}

func (h *WebhookHandler) handleSubscriptionUpdated(ctx context.Context, event stripe.Event) {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		h.logger.Error("unmarshal subscription", zap.Error(err))
		return
	}

	customerID := ""
	if sub.Customer != nil {
		customerID = sub.Customer.ID
	}
	if customerID == "" {
		return
	}

	tenantID, err := h.stripe.LookupTenantByCustomer(ctx, customerID)
	if err != nil {
		h.logger.Error("lookup tenant for subscription update", zap.String("customer", customerID), zap.Error(err))
		return
	}

	status := string(sub.Status)

	// Determine plan from subscription metadata if available.
	plan := ""
	if sub.Metadata != nil {
		plan = sub.Metadata["plan"]
	}

	if h.stripe.db != nil {
		if plan != "" {
			_, _ = h.stripe.db.ExecContext(ctx,
				`UPDATE tenants SET subscription_status = $1, plan = $2 WHERE id = $3`,
				status, plan, tenantID)
		} else {
			_, _ = h.stripe.db.ExecContext(ctx,
				`UPDATE tenants SET subscription_status = $1 WHERE id = $2`,
				status, tenantID)
		}
	}

	h.logger.Info("subscription updated",
		zap.String("tenant", tenantID),
		zap.String("status", status))
}

func (h *WebhookHandler) handleSubscriptionDeleted(ctx context.Context, event stripe.Event) {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		h.logger.Error("unmarshal subscription", zap.Error(err))
		return
	}

	customerID := ""
	if sub.Customer != nil {
		customerID = sub.Customer.ID
	}
	if customerID == "" {
		return
	}

	tenantID, err := h.stripe.LookupTenantByCustomer(ctx, customerID)
	if err != nil {
		h.logger.Error("lookup tenant for subscription delete", zap.String("customer", customerID), zap.Error(err))
		return
	}

	// Downgrade to starter — clear subscription ID, reset plan.
	if h.stripe.db != nil {
		_, _ = h.stripe.db.ExecContext(ctx,
			`UPDATE tenants
			 SET subscription_status = 'canceled',
			     stripe_subscription_id = NULL,
			     plan = 'starter'
			 WHERE id = $1`, tenantID)
	}

	h.logger.Info("subscription deleted — downgraded to starter",
		zap.String("tenant", tenantID))
}

// OverageService tracks grace periods for tenants exceeding quotas.
type OverageService struct {
	graceStartTimes map[string]time.Time
}

// CheckOverage returns "OK" if within limits, "GRACE_PERIOD" if over.
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

// ShouldAutoUpgrade checks if a tenant has been in grace period long enough.
func (s *OverageService) ShouldAutoUpgrade(tenantID string, graceDuration time.Duration) bool {
	startTime, exists := s.graceStartTimes[tenantID]
	if !exists {
		return false
	}

	return time.Since(startTime) >= graceDuration
}

// InvoiceDateFmt formats a Unix timestamp as a readable date for invoice display.
func InvoiceDateFmt(unix int64) string {
	return time.Unix(unix, 0).Format("Jan 2, 2006")
}
