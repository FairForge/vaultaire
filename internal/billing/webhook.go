package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stripe/stripe-go/v75"
	"github.com/stripe/stripe-go/v75/webhook"
	"go.uber.org/zap"
)

// HouseQuotas is the slice of usage.QuotaManager the webhook drives: a
// house subscription's items become per-floor quotas (066).
type HouseQuotas interface {
	SetHouse(ctx context.Context, tenantID string, h usage.House) error
	ClearHouse(ctx context.Context, tenantID string) error
}

// WebhookHandler processes Stripe webhook events with signature
// verification and persists subscription state to the database.
type WebhookHandler struct {
	endpointSecret string
	stripe         *StripeService
	logger         *zap.Logger

	// quotas, when set, receives house subscriptions as floor quotas.
	// subs overrides the subscription source (tests); nil = the Stripe API.
	quotas HouseQuotas
	subs   subscriptionFetcher
}

// SetHouseQuotas wires the quota manager the house webhooks write to.
func (h *WebhookHandler) SetHouseQuotas(q HouseQuotas) { h.quotas = q }

func (h *WebhookHandler) fetchSubscription(ctx context.Context, id string) (*stripe.Subscription, error) {
	if h.subs != nil {
		return h.subs.Get(ctx, id)
	}
	return h.stripe.subs().Get(ctx, id)
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

	// Idempotency: skip if we already processed this event.
	if h.stripe.db != nil && event.ID != "" {
		var exists bool
		_ = h.stripe.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM stripe_events WHERE event_id = $1)`,
			event.ID).Scan(&exists)
		if exists {
			h.logger.Debug("duplicate webhook event, skipping", zap.String("id", event.ID))
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	switch event.Type {
	case "checkout.session.completed":
		h.handleCheckoutCompleted(ctx, event)
	case "invoice.payment_succeeded":
		h.handlePaymentSucceeded(ctx, event)
	case "invoice.payment_failed":
		h.handlePaymentFailed(ctx, event)
	case "customer.subscription.created", "customer.subscription.updated":
		h.handleSubscriptionUpdated(ctx, event)
	case "customer.subscription.deleted":
		h.handleSubscriptionDeleted(ctx, event)
	default:
		h.logger.Debug("unhandled webhook event", zap.String("type", string(event.Type)))
	}

	// Record event for idempotency.
	if h.stripe.db != nil && event.ID != "" {
		_, _ = h.stripe.db.ExecContext(ctx,
			`INSERT INTO stripe_events (event_id, event_type, data)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (event_id) DO NOTHING`,
			event.ID, string(event.Type), string(body))
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

	// A house checkout: the subscription's items are the floor quotas. The
	// session payload carries no items, so fetch the subscription; the
	// customer.subscription.created/updated events reach the same code.
	if sub, err := h.fetchSubscription(ctx, subscriptionID); err != nil {
		h.logger.Error("fetch subscription after checkout", zap.String("subscription", subscriptionID), zap.Error(err))
	} else if h.applyHouse(ctx, tenantID, sub) {
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

	h.applyHouse(ctx, tenantID, &sub)
}

// applyHouse turns a house subscription into floor quotas. Returns false
// when the subscription is not a house (legacy pack) or nothing is wired.
//
//	active / trialing / past_due  → the house is granted (past_due = grace)
//	canceled / unpaid / expired   → the house is cleared, data untouched
//	incomplete / paused           → no change
//
// A clearing status is honoured only for the tenant's current subscription
// so a stale, older subscription's events cannot take a paid house away.
func (h *WebhookHandler) applyHouse(ctx context.Context, tenantID string, sub *stripe.Subscription) bool {
	order, ok := h.stripe.HouseFromSubscription(sub)
	if !ok || h.quotas == nil || h.stripe.db == nil {
		return false
	}
	switch sub.Status {
	case stripe.SubscriptionStatusActive, stripe.SubscriptionStatusTrialing, stripe.SubscriptionStatusPastDue:
		if err := h.quotas.SetHouse(ctx, tenantID, usage.HouseFromTB(order.Std, order.Vault, order.PinHot)); err != nil {
			h.logger.Error("set house quotas", zap.String("tenant", tenantID), zap.Error(err))
			return true
		}
		if _, err := h.stripe.db.ExecContext(ctx,
			`UPDATE tenants
			    SET stripe_subscription_id = $1, subscription_status = $2, plan = 'house', house_period = $3
			  WHERE id = $4`,
			sub.ID, string(sub.Status), string(order.Period), tenantID); err != nil {
			h.logger.Error("save house subscription", zap.String("tenant", tenantID), zap.Error(err))
			return true
		}
		h.logger.Info("house granted",
			zap.String("tenant", tenantID), zap.String("subscription", sub.ID),
			zap.Int("std_tb", order.Std), zap.Int("vault_tb", order.Vault), zap.Int("pin_hot_tb", order.PinHot),
			zap.String("period", string(order.Period)), zap.String("status", string(sub.Status)))
	case stripe.SubscriptionStatusCanceled, stripe.SubscriptionStatusUnpaid, stripe.SubscriptionStatusIncompleteExpired:
		if !h.isCurrentSubscription(ctx, tenantID, sub.ID) {
			h.logger.Info("ignoring clearing event for a stale subscription",
				zap.String("tenant", tenantID), zap.String("subscription", sub.ID))
			return true
		}
		if err := h.quotas.ClearHouse(ctx, tenantID); err != nil {
			h.logger.Error("clear house quotas", zap.String("tenant", tenantID), zap.Error(err))
			return true
		}
		if _, err := h.stripe.db.ExecContext(ctx,
			`UPDATE tenants SET subscription_status = $1, house_period = '' WHERE id = $2`,
			string(sub.Status), tenantID); err != nil {
			h.logger.Error("save cleared house", zap.String("tenant", tenantID), zap.Error(err))
		}
		h.logger.Warn("house cleared — back to the free tier, data untouched",
			zap.String("tenant", tenantID), zap.String("subscription", sub.ID), zap.String("status", string(sub.Status)))
	}
	return true
}

// isCurrentSubscription reports whether subID is the tenant's recorded
// subscription (or the tenant has none recorded).
func (h *WebhookHandler) isCurrentSubscription(ctx context.Context, tenantID, subID string) bool {
	var stored sql.NullString
	if err := h.stripe.db.QueryRowContext(ctx,
		`SELECT stripe_subscription_id FROM tenants WHERE id = $1`, tenantID).Scan(&stored); err != nil {
		return true
	}
	return !stored.Valid || stored.String == "" || stored.String == subID
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

	// A deleted subscription that is not the tenant's current one (an old
	// pack, a replaced house) must not downgrade anything.
	if h.stripe.db != nil && !h.isCurrentSubscription(ctx, tenantID, sub.ID) {
		h.logger.Info("ignoring deletion of a stale subscription",
			zap.String("tenant", tenantID), zap.String("subscription", sub.ID))
		return
	}

	// A house: floor quotas go, data stays.
	if _, isHouse := h.stripe.HouseFromSubscription(&sub); isHouse && h.quotas != nil {
		if err := h.quotas.ClearHouse(ctx, tenantID); err != nil {
			h.logger.Error("clear house quotas on delete", zap.String("tenant", tenantID), zap.Error(err))
		}
	}

	// Downgrade to starter — clear subscription ID, reset plan.
	if h.stripe.db != nil {
		_, _ = h.stripe.db.ExecContext(ctx,
			`UPDATE tenants
			 SET subscription_status = 'canceled',
			     stripe_subscription_id = NULL,
			     plan = 'starter',
			     house_period = ''
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
