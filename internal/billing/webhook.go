package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stripe/stripe-go/v75"
	"github.com/stripe/stripe-go/v75/webhook"
	"go.uber.org/zap"
)

// maxWebhookBodyBytes caps a webhook delivery. Stripe puts every invoice
// line, discount, tax line and previous_attributes in the event, so a
// multi-line house invoice can pass the 64 KB the general request limit
// allows; oversize is refused (413), never silently truncated (Review R10).
const maxWebhookBodyBytes = 10 << 20

// HouseQuotas is the slice of usage.QuotaManager the webhook drives: a
// house subscription's items become per-floor quotas (066).
type HouseQuotas interface {
	SetHouse(ctx context.Context, tenantID string, h usage.House) error
	ClearHouse(ctx context.Context, tenantID string) error
}

// WebhookHandler processes Stripe webhook events with signature
// verification and persists subscription state to the database.
//
// Delivery contract (Review R10, WP-R10-2): the event id is CLAIMED in
// stripe_events before its handler runs — an INSERT inside a transaction
// that stays open while the handler runs and commits only after it
// succeeded. A delivery of an id already committed is a duplicate (200 at
// once); a concurrent delivery of the same id waits on the uncommitted claim
// (≤ 5 s, lock_timeout) and is then a duplicate, or takes the event over if
// the first one failed. Any failure rolls the claim back and answers 5xx, so
// Stripe retries (exponential backoff for up to three days in live mode) and
// the retry is handled as a fresh event; a process that dies mid-handler
// takes its connection — and so its claim — with it. One Stripe customer's
// events are applied one at a time (an advisory lock in the claim), each
// from the subscription as Stripe has it when it is applied.
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
// whsec_... value from the Stripe Dashboard webhook endpoint config. It is
// required: with an empty secret the handler refuses every request (503)
// rather than trusting unsigned bodies — the Stripe endpoint must also be
// created pinned to stripe.APIVersion (2023-08-16 for this SDK), or every
// delivery fails verification.
func NewWebhookHandler(secret string, stripeSvc *StripeService, logger *zap.Logger) *WebhookHandler {
	if secret == "" {
		logger.Error("stripe webhook: STRIPE_WEBHOOK_SECRET is empty — the endpoint will refuse every delivery (503) until it is set")
	}
	return &WebhookHandler{
		endpointSecret: secret,
		stripe:         stripeSvc,
		logger:         logger,
	}
}

// ServeHTTP handles POST /webhook/stripe.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.endpointSecret == "" {
		h.logger.Error("stripe webhook refused: no endpoint secret configured")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.logger.Warn("stripe webhook body over the cap", zap.Int64("limit", tooBig.Limit))
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		h.logger.Error("read webhook body", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Scheme v1 (HMAC-SHA256 over "<t>.<body>"), 5-minute tolerance, and
	// the event's api_version must equal stripe.APIVersion.
	event, err := webhook.ConstructEvent(body, r.Header.Get("Stripe-Signature"), h.endpointSecret)
	if err != nil {
		h.logger.Warn("webhook signature verification failed", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	h.logger.Info("stripe webhook received",
		zap.String("type", string(event.Type)),
		zap.String("id", event.ID))

	// Claim the event id before anything is applied (R10-16: the dedup was
	// check-then-act, two concurrent deliveries were both handled).
	var claim *sql.Tx
	if h.stripe.db != nil && event.ID != "" {
		tx, duplicate, err := h.claimEvent(ctx, event, body)
		if err != nil {
			h.logger.Error("stripe webhook: claim failed — left for Stripe to retry", zap.String("id", event.ID), zap.Error(err))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if duplicate {
			h.logger.Debug("duplicate webhook event, skipping", zap.String("id", event.ID))
			w.WriteHeader(http.StatusOK)
			return
		}
		claim = tx
		defer func() { _ = claim.Rollback() }() // a no-op after Commit
	}

	if handleErr := h.handle(ctx, event); handleErr != nil {
		// The claim is rolled back: Stripe retries, and the retry is handled.
		h.logger.Error("stripe webhook: event not applied — left for Stripe to retry",
			zap.String("type", string(event.Type)), zap.String("id", event.ID), zap.Error(handleErr))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// A claim that cannot be committed is a retry too: the handlers are
	// idempotent, so applying the event again is the safe side.
	if claim != nil {
		if err := claim.Commit(); err != nil {
			h.logger.Error("stripe webhook: record event failed", zap.String("id", event.ID), zap.Error(err))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

// claimEvent claims event.ID: a transaction holding its new stripe_events
// row (uncommitted) and the customer's advisory lock. duplicate is true when
// the id was already committed (or is committed by the delivery this one
// waited on); the transaction is then closed.
func (h *WebhookHandler) claimEvent(ctx context.Context, event stripe.Event, body []byte) (*sql.Tx, bool, error) {
	tx, err := h.stripe.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin claim of %s: %w", event.ID, err)
	}
	// How long a delivery waits on another delivery of the same event, or on
	// another event of the same customer, before it answers 500 and lets
	// Stripe retry later.
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		_ = tx.Rollback()
		return nil, false, fmt.Errorf("claim of %s: lock timeout: %w", event.ID, err)
	}
	var claimed string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO stripe_events (event_id, event_type, data)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (event_id) DO NOTHING
		 RETURNING event_id`,
		event.ID, string(event.Type), string(body)).Scan(&claimed)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return nil, true, nil
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, false, fmt.Errorf("claim %s: %w", event.ID, err)
	}
	// One customer's events one at a time: each fetches the subscription
	// after the previous one was applied, so the last applied is the latest.
	if customer := eventCustomer(event); customer != "" {
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended('stripe-customer:' || $1, 0))`, customer); err != nil {
			_ = tx.Rollback()
			return nil, false, fmt.Errorf("claim %s: lock customer %s: %w", event.ID, customer, err)
		}
	}
	return tx, false, nil
}

// eventCustomer is the Stripe customer id of an event's object (an id, or an
// expanded customer), "" when it has none.
func eventCustomer(event stripe.Event) string {
	if event.Data == nil {
		return ""
	}
	switch c := event.Data.Object["customer"].(type) {
	case string:
		return c
	case map[string]any:
		id, _ := c["id"].(string)
		return id
	}
	return ""
}

// handle applies one verified event; an error means it must be retried.
func (h *WebhookHandler) handle(ctx context.Context, event stripe.Event) error {
	var handleErr error
	switch event.Type {
	case "checkout.session.completed":
		handleErr = h.handleCheckoutCompleted(ctx, event)
	case "invoice.payment_succeeded":
		handleErr = h.handlePaymentSucceeded(ctx, event)
	case "invoice.payment_failed":
		handleErr = h.handlePaymentFailed(ctx, event)
	case "customer.subscription.created", "customer.subscription.updated":
		handleErr = h.handleSubscriptionUpdated(ctx, event)
	case "customer.subscription.deleted":
		handleErr = h.handleSubscriptionDeleted(ctx, event)
	default:
		h.logger.Debug("unhandled webhook event", zap.String("type", string(event.Type)))
	}
	return handleErr
}

// stripeMissing reports a Stripe 404 (resource_missing): the object is not
// at Stripe — nothing an event about it could apply, and nothing a retry
// would change.
func stripeMissing(err error) bool {
	var se *stripe.Error
	return errors.As(err, &se) && (se.HTTPStatusCode == http.StatusNotFound || se.Code == stripe.ErrorCodeResourceMissing)
}

// A payload the SDK cannot deserialise is not retryable (it would fail the
// same way for three days and get the endpoint disabled): log and drop.
func (h *WebhookHandler) unmarshal(event stripe.Event, what string, v any) bool {
	if err := json.Unmarshal(event.Data.Raw, v); err != nil {
		h.logger.Error("unmarshal "+what, zap.String("id", event.ID), zap.Error(err))
		return false
	}
	return true
}

// resolveTenant maps a verified event to a tenant. The customer id is the
// normal key; when `tenants` does not know it, the payload's own hints —
// Checkout's client_reference_id and the metadata.tenant_id this service
// stamps on every session and subscription — name the tenant, and the
// customer id is healed onto the row so later events resolve directly (the
// persist in CreateCustomer is logged-and-continued, so a checkout can
// complete under a customer id the row never received). A customer that
// resolves nowhere is not ours (another product on the Stripe account, a
// customer made in the dashboard, a tenant already erased): the event is
// acknowledged and recorded rather than answered 500 for three days of
// retries (post-merge review R10-45). Only a real lookup failure is an
// error, which ServeHTTP turns into a retry.
func (h *WebhookHandler) resolveTenant(ctx context.Context, event stripe.Event, customerID string, hints ...string) (string, bool, error) {
	tenantID, err := h.stripe.LookupTenantByCustomer(ctx, customerID)
	if err == nil {
		return tenantID, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	for _, hint := range hints {
		if hint == "" {
			continue
		}
		var exists bool
		if err := h.stripe.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM tenants WHERE id = $1)`, hint).Scan(&exists); err != nil {
			return "", false, fmt.Errorf("resolve tenant hint %q: %w", hint, err)
		}
		if !exists {
			continue
		}
		res, err := h.stripe.db.ExecContext(ctx,
			`UPDATE tenants SET stripe_customer_id = $1
			  WHERE id = $2 AND (stripe_customer_id IS NULL OR stripe_customer_id = '')`,
			customerID, hint)
		if err != nil {
			return "", false, fmt.Errorf("heal stripe_customer_id for tenant %s: %w", hint, err)
		}
		healed, _ := res.RowsAffected()
		h.logger.Warn("stripe webhook: customer unknown, tenant resolved from the event's own reference",
			zap.String("id", event.ID), zap.String("customer", customerID), zap.String("tenant", hint),
			zap.Bool("customer_id_healed", healed == 1))
		return hint, true, nil
	}
	h.logger.Warn("stripe webhook: customer belongs to no tenant — acknowledged, not applied",
		zap.String("id", event.ID), zap.String("type", string(event.Type)), zap.String("customer", customerID))
	return "", false, nil
}

// invoiceTenantHint is the subscription metadata Stripe copies onto every
// invoice (subscription_details.metadata, invoices since 2023-06-29).
func invoiceTenantHint(inv *stripe.Invoice) string {
	if inv.SubscriptionDetails == nil {
		return ""
	}
	return inv.SubscriptionDetails.Metadata["tenant_id"]
}

func (h *WebhookHandler) handleCheckoutCompleted(ctx context.Context, event stripe.Event) error {
	var session stripe.CheckoutSession
	if !h.unmarshal(event, "checkout session", &session) {
		return nil
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
		return nil
	}

	tenantID, ours, err := h.resolveTenant(ctx, event, customerID,
		session.ClientReferenceID, session.Metadata["tenant_id"])
	if err != nil {
		return fmt.Errorf("lookup tenant for checkout customer %s: %w", customerID, err)
	}
	if !ours {
		return nil
	}

	// A house checkout: the subscription's items are the floor quotas. The
	// session payload carries no items, so fetch the subscription; the
	// customer.subscription.created/updated events reach the same code.
	sub, err := h.fetchSubscription(ctx, subscriptionID)
	if stripeMissing(err) {
		h.logger.Warn("stripe webhook: checkout names a subscription Stripe does not have — nothing applied",
			zap.String("id", event.ID), zap.String("subscription", subscriptionID))
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetch subscription %s after checkout: %w", subscriptionID, err)
	}
	if handled, err := h.applyHouse(ctx, tenantID, sub); handled || err != nil {
		return err
	}

	// Determine plan from checkout metadata or default to "standard".
	plan := "standard"
	if session.Metadata != nil {
		if p, ok := session.Metadata["plan"]; ok {
			plan = p
		}
	}

	if err := h.stripe.SaveSubscription(ctx, tenantID, subscriptionID, "active", plan); err != nil {
		return fmt.Errorf("save subscription after checkout: %w", err)
	}

	h.logger.Info("subscription activated",
		zap.String("tenant", tenantID),
		zap.String("subscription", subscriptionID),
		zap.String("plan", plan))
	return nil
}

func (h *WebhookHandler) handlePaymentSucceeded(ctx context.Context, event stripe.Event) error {
	var inv stripe.Invoice
	if !h.unmarshal(event, "invoice", &inv) {
		return nil
	}

	customerID := ""
	if inv.Customer != nil {
		customerID = inv.Customer.ID
	}
	if customerID == "" {
		return nil
	}

	tenantID, ours, err := h.resolveTenant(ctx, event, customerID, invoiceTenantHint(&inv))
	if err != nil {
		return fmt.Errorf("lookup tenant for payment: %w", err)
	}
	if !ours {
		return nil
	}

	// Ensure subscription status is active after successful payment.
	if h.stripe.db != nil {
		if _, err := h.stripe.db.ExecContext(ctx,
			`UPDATE tenants SET subscription_status = 'active' WHERE id = $1`, tenantID); err != nil {
			return fmt.Errorf("mark tenant %s active: %w", tenantID, err)
		}
	}

	h.logger.Info("payment succeeded",
		zap.String("tenant", tenantID),
		zap.String("invoice", inv.ID))
	return nil
}

func (h *WebhookHandler) handlePaymentFailed(ctx context.Context, event stripe.Event) error {
	var inv stripe.Invoice
	if !h.unmarshal(event, "invoice", &inv) {
		return nil
	}

	customerID := ""
	if inv.Customer != nil {
		customerID = inv.Customer.ID
	}
	if customerID == "" {
		return nil
	}

	tenantID, ours, err := h.resolveTenant(ctx, event, customerID, invoiceTenantHint(&inv))
	if err != nil {
		return fmt.Errorf("lookup tenant for failed payment: %w", err)
	}
	if !ours {
		return nil
	}

	// past_due is the grace period: the house is kept (applyHouse grants
	// for past_due) while Stripe's Smart Retries run; the dashboard's
	// "after retries" setting then delivers canceled/unpaid, which clears it.
	if h.stripe.db != nil {
		if _, err := h.stripe.db.ExecContext(ctx,
			`UPDATE tenants SET subscription_status = 'past_due' WHERE id = $1`, tenantID); err != nil {
			return fmt.Errorf("mark tenant %s past_due: %w", tenantID, err)
		}
	}

	h.logger.Warn("payment failed",
		zap.String("tenant", tenantID),
		zap.String("invoice", inv.ID))
	return nil
}

// handleSubscriptionUpdated applies customer.subscription.created/updated
// from the subscription as Stripe has it now — fetched, never the payload's
// status or items (R10-15): Stripe does not guarantee delivery order, and an
// older updated(active) delivered after updated(canceled) re-granted a
// cancelled house (the reverse took a paid one away). The payload names the
// subscription and its customer only.
func (h *WebhookHandler) handleSubscriptionUpdated(ctx context.Context, event stripe.Event) error {
	var payload stripe.Subscription
	if !h.unmarshal(event, "subscription", &payload) {
		return nil
	}

	customerID := ""
	if payload.Customer != nil {
		customerID = payload.Customer.ID
	}
	if customerID == "" || payload.ID == "" {
		return nil
	}

	tenantID, ours, err := h.resolveTenant(ctx, event, customerID, payload.Metadata["tenant_id"])
	if err != nil {
		return fmt.Errorf("lookup tenant for subscription update: %w", err)
	}
	if !ours {
		return nil
	}

	sub, err := h.fetchSubscription(ctx, payload.ID)
	if stripeMissing(err) {
		h.logger.Warn("stripe webhook: subscription event for a subscription Stripe does not have — nothing applied",
			zap.String("id", event.ID), zap.String("subscription", payload.ID), zap.String("tenant", tenantID))
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetch subscription %s for %s: %w", payload.ID, event.Type, err)
	}

	status := string(sub.Status)

	// Determine plan from subscription metadata if available.
	plan := ""
	if sub.Metadata != nil {
		plan = sub.Metadata["plan"]
	}

	if h.stripe.db != nil {
		var execErr error
		if plan != "" {
			_, execErr = h.stripe.db.ExecContext(ctx,
				`UPDATE tenants SET subscription_status = $1, plan = $2 WHERE id = $3`,
				status, plan, tenantID)
		} else {
			_, execErr = h.stripe.db.ExecContext(ctx,
				`UPDATE tenants SET subscription_status = $1 WHERE id = $2`,
				status, tenantID)
		}
		if execErr != nil {
			return fmt.Errorf("save subscription status for tenant %s: %w", tenantID, execErr)
		}
	}

	h.logger.Info("subscription updated",
		zap.String("tenant", tenantID),
		zap.String("status", status))

	_, err = h.applyHouse(ctx, tenantID, sub)
	return err
}

// applyHouse turns a house subscription into floor quotas. handled is false
// when the subscription is not a house (legacy pack) or nothing is wired;
// a non-nil error means the event must be retried.
//
//	active / trialing / past_due     → the house is granted (past_due = grace)
//	canceled / unpaid / expired /
//	paused                           → the house is cleared, data untouched
//	incomplete                       → no change (first payment pending)
//
// paused (WP-R10-2): a trial that ended without a payment method — Stripe
// creates no invoice while it lasts, so nothing is being paid; it is
// resumable, like unpaid, which is why HasLiveSubscription counts both.
//
// A clearing status is honoured only for the tenant's current subscription
// so a stale, older subscription's events cannot take a paid house away.
func (h *WebhookHandler) applyHouse(ctx context.Context, tenantID string, sub *stripe.Subscription) (bool, error) {
	order, ok := h.stripe.HouseFromSubscription(sub)
	if !ok || h.quotas == nil || h.stripe.db == nil {
		return false, nil
	}
	switch sub.Status {
	case stripe.SubscriptionStatusActive, stripe.SubscriptionStatusTrialing, stripe.SubscriptionStatusPastDue:
		if err := h.quotas.SetHouse(ctx, tenantID, usage.HouseFromTB(order.Std, order.Vault, order.PinHot)); err != nil {
			return true, fmt.Errorf("set house quotas for tenant %s: %w", tenantID, err)
		}
		if _, err := h.stripe.db.ExecContext(ctx,
			`UPDATE tenants
			    SET stripe_subscription_id = $1, subscription_status = $2, plan = 'house', house_period = $3
			  WHERE id = $4`,
			sub.ID, string(sub.Status), string(order.Period), tenantID); err != nil {
			return true, fmt.Errorf("save house subscription for tenant %s: %w", tenantID, err)
		}
		h.logger.Info("house granted",
			zap.String("tenant", tenantID), zap.String("subscription", sub.ID),
			zap.Int("std_tb", order.Std), zap.Int("vault_tb", order.Vault), zap.Int("pin_hot_tb", order.PinHot),
			zap.String("period", string(order.Period)), zap.String("status", string(sub.Status)))
	case stripe.SubscriptionStatusCanceled, stripe.SubscriptionStatusUnpaid, stripe.SubscriptionStatusIncompleteExpired,
		stripe.SubscriptionStatusPaused:
		current, err := h.isCurrentSubscription(ctx, tenantID, sub.ID)
		if err != nil {
			return true, err
		}
		if !current {
			h.logger.Info("ignoring clearing event for a stale subscription",
				zap.String("tenant", tenantID), zap.String("subscription", sub.ID))
			return true, nil
		}
		if err := h.quotas.ClearHouse(ctx, tenantID); err != nil {
			return true, fmt.Errorf("clear house quotas for tenant %s: %w", tenantID, err)
		}
		if _, err := h.stripe.db.ExecContext(ctx,
			`UPDATE tenants SET subscription_status = $1, house_period = '' WHERE id = $2`,
			string(sub.Status), tenantID); err != nil {
			return true, fmt.Errorf("save cleared house for tenant %s: %w", tenantID, err)
		}
		h.logger.Warn("house cleared — back to the free tier, data untouched",
			zap.String("tenant", tenantID), zap.String("subscription", sub.ID), zap.String("status", string(sub.Status)))
	}
	return true, nil
}

// isCurrentSubscription reports whether subID is the tenant's recorded
// subscription (or the tenant has none recorded). A lookup error is
// returned, not treated as "current" — clearing a house on a DB hiccup is
// the wrong default.
func (h *WebhookHandler) isCurrentSubscription(ctx context.Context, tenantID, subID string) (bool, error) {
	var stored sql.NullString
	if err := h.stripe.db.QueryRowContext(ctx,
		`SELECT stripe_subscription_id FROM tenants WHERE id = $1`, tenantID).Scan(&stored); err != nil {
		return false, fmt.Errorf("read current subscription for tenant %s: %w", tenantID, err)
	}
	return !stored.Valid || stored.String == "" || stored.String == subID, nil
}

// handleSubscriptionDeleted applies customer.subscription.deleted from the
// payload: canceled is terminal at Stripe (a deleted subscription is never
// reactivated), so a fetch cannot tell anything newer; an older updated
// delivered after it fetches the canceled state and clears again.
func (h *WebhookHandler) handleSubscriptionDeleted(ctx context.Context, event stripe.Event) error {
	var sub stripe.Subscription
	if !h.unmarshal(event, "subscription", &sub) {
		return nil
	}

	customerID := ""
	if sub.Customer != nil {
		customerID = sub.Customer.ID
	}
	if customerID == "" {
		return nil
	}

	tenantID, ours, err := h.resolveTenant(ctx, event, customerID, sub.Metadata["tenant_id"])
	if err != nil {
		return fmt.Errorf("lookup tenant for subscription delete: %w", err)
	}
	if !ours {
		return nil
	}

	// A deleted subscription that is not the tenant's current one (an old
	// pack, a replaced house) must not downgrade anything.
	if h.stripe.db != nil {
		current, err := h.isCurrentSubscription(ctx, tenantID, sub.ID)
		if err != nil {
			return err
		}
		if !current {
			h.logger.Info("ignoring deletion of a stale subscription",
				zap.String("tenant", tenantID), zap.String("subscription", sub.ID))
			return nil
		}
	}

	// A house: floor quotas go, data stays.
	if _, isHouse := h.stripe.HouseFromSubscription(&sub); isHouse && h.quotas != nil {
		if err := h.quotas.ClearHouse(ctx, tenantID); err != nil {
			return fmt.Errorf("clear house quotas on delete for tenant %s: %w", tenantID, err)
		}
	}

	// Downgrade to starter — clear subscription ID, reset plan.
	if h.stripe.db != nil {
		if _, err := h.stripe.db.ExecContext(ctx,
			`UPDATE tenants
			 SET subscription_status = 'canceled',
			     stripe_subscription_id = NULL,
			     plan = 'starter',
			     house_period = ''
			 WHERE id = $1`, tenantID); err != nil {
			return fmt.Errorf("downgrade tenant %s after subscription delete: %w", tenantID, err)
		}
	}

	h.logger.Info("subscription deleted — downgraded to starter",
		zap.String("tenant", tenantID))
	return nil
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
