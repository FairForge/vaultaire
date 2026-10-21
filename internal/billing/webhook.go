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

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/payments"
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
	// flags answers entitlement_lapse per tenant (nil = off).
	flags payments.FlagSource
	// receipts mails the receipt of a NEWLY settled invoice (nil = none).
	receipts ReceiptSender
}

// SetFlags wires the flag source: with entitlement_lapse ON for a tenant a
// terminal Stripe status stops the renewal and keeps the house — the date
// and the lapse job decide (WP-PAY-2, docs/PAYMENTS.md § 1.3).
// ReceiptSender mails the receipt of a newly settled ledger row
// (payments.Mailer, WP-PAY-3). Best effort, after the commit.
type ReceiptSender interface {
	SendReceipt(ctx context.Context, tenantID, paymentID string) error
}

// SetReceiptSender attaches the receipt mailer.
func (h *WebhookHandler) SetReceiptSender(r ReceiptSender) { h.receipts = r }

// webhookActor is the audit actor of what the webhook writes.
const webhookActor = "stripe-webhook"

func (h *WebhookHandler) SetFlags(fl payments.FlagSource) { h.flags = fl }

// dateDecides reports entitlement_lapse ON for the tenant.
func (h *WebhookHandler) dateDecides(tenantID string) bool {
	return h.flags != nil && h.flags.Enabled(payments.FlagEntitlementLapse, tenantID)
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
	// Every delivery is counted once by its outcome
	// (vaultaire_payment_webhook_total, the PaymentWebhookFailing series).
	outcome := payments.WebhookError
	defer func() { payments.CountWebhook(payments.ProviderStripe, outcome) }()
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
			outcome = payments.WebhookRejected
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		h.logger.Error("read webhook body", zap.Error(err))
		outcome = payments.WebhookRejected
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Scheme v1 (HMAC-SHA256 over "<t>.<body>"), 5-minute tolerance, and
	// the event's api_version must equal stripe.APIVersion.
	event, err := webhook.ConstructEvent(body, r.Header.Get("Stripe-Signature"), h.endpointSecret)
	if err != nil {
		h.logger.Warn("webhook signature verification failed", zap.Error(err))
		outcome = payments.WebhookRejected
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
			outcome = payments.WebhookDuplicate
			w.WriteHeader(http.StatusOK)
			return
		}
		claim = tx
		defer func() { _ = claim.Rollback() }() // a no-op after Commit
	}

	tenantID, handleErr := h.handle(ctx, event)
	if handleErr != nil {
		// The claim is rolled back: Stripe retries, and the retry is handled.
		h.logger.Error("stripe webhook: event not applied — left for Stripe to retry",
			zap.String("type", string(event.Type)), zap.String("id", event.ID), zap.Error(handleErr))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// A claim that cannot be committed is a retry too: the handlers are
	// idempotent, so applying the event again is the safe side. The claim
	// row names the tenant the event was applied to (RP11-2: the support
	// page lists a tenant's events by it), NULL when it was not ours.
	if claim != nil {
		if tenantID != "" {
			if _, err := claim.ExecContext(ctx,
				`UPDATE stripe_events SET tenant_id = $1 WHERE event_id = $2`, tenantID, event.ID); err != nil {
				h.logger.Error("stripe webhook: record event tenant failed — left for Stripe to retry",
					zap.String("id", event.ID), zap.Error(err))
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		if err := claim.Commit(); err != nil {
			h.logger.Error("stripe webhook: record event failed", zap.String("id", event.ID), zap.Error(err))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	outcome = payments.WebhookOK
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

// handle applies one verified event and returns the tenant it resolved ("" =
// not ours, or nothing to resolve); an error means it must be retried.
func (h *WebhookHandler) handle(ctx context.Context, event stripe.Event) (string, error) {
	switch event.Type {
	case "checkout.session.completed":
		return h.handleCheckoutCompleted(ctx, event)
	case "invoice.payment_succeeded":
		return h.handlePaymentSucceeded(ctx, event)
	case "invoice.payment_failed":
		return h.handlePaymentFailed(ctx, event)
	case "customer.subscription.created", "customer.subscription.updated":
		return h.handleSubscriptionUpdated(ctx, event)
	case "customer.subscription.deleted":
		return h.handleSubscriptionDeleted(ctx, event)
	default:
		h.logger.Debug("unhandled webhook event", zap.String("type", string(event.Type)))
	}
	return "", nil
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
// persist in the pre-WP-PAY-2 CreateCustomer was logged-and-continued, so a checkout can
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

func (h *WebhookHandler) handleCheckoutCompleted(ctx context.Context, event stripe.Event) (string, error) {
	var session stripe.CheckoutSession
	if !h.unmarshal(event, "checkout session", &session) {
		return "", nil
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
		return "", nil
	}

	tenantID, ours, err := h.resolveTenant(ctx, event, customerID,
		session.ClientReferenceID, session.Metadata["tenant_id"])
	if err != nil {
		return tenantID, fmt.Errorf("lookup tenant for checkout customer %s: %w", customerID, err)
	}
	if !ours {
		return tenantID, nil
	}

	// A house checkout: the subscription's items are the floor quotas. The
	// session payload carries no items, so fetch the subscription; the
	// customer.subscription.created/updated events reach the same code.
	sub, err := h.fetchSubscription(ctx, subscriptionID)
	if stripeMissing(err) {
		h.logger.Warn("stripe webhook: checkout names a subscription Stripe does not have — nothing applied",
			zap.String("id", event.ID), zap.String("subscription", subscriptionID))
		return tenantID, nil
	}
	if err != nil {
		return tenantID, fmt.Errorf("fetch subscription %s after checkout: %w", subscriptionID, err)
	}
	if handled, err := h.applyHouse(ctx, tenantID, sub); handled || err != nil {
		return tenantID, err
	}

	// Determine plan from checkout metadata or default to "standard".
	plan := "standard"
	if session.Metadata != nil {
		if p, ok := session.Metadata["plan"]; ok {
			plan = p
		}
	}

	if err := h.stripe.SaveSubscription(ctx, tenantID, subscriptionID, "active", plan); err != nil {
		return tenantID, fmt.Errorf("save subscription after checkout: %w", err)
	}

	h.logger.Info("subscription activated",
		zap.String("tenant", tenantID),
		zap.String("subscription", subscriptionID),
		zap.String("plan", plan))
	return tenantID, nil
}

func (h *WebhookHandler) handlePaymentSucceeded(ctx context.Context, event stripe.Event) (string, error) {
	var inv stripe.Invoice
	if !h.unmarshal(event, "invoice", &inv) {
		return "", nil
	}

	customerID := ""
	if inv.Customer != nil {
		customerID = inv.Customer.ID
	}
	if customerID == "" {
		return "", nil
	}

	tenantID, ours, err := h.resolveTenant(ctx, event, customerID, invoiceTenantHint(&inv))
	if err != nil {
		return tenantID, fmt.Errorf("lookup tenant for payment: %w", err)
	}
	if !ours {
		return tenantID, nil
	}

	// The subscription is active again after a successful payment — when it
	// is the tenant's current one (RP11-1).
	if err := h.writeInvoiceStatus(ctx, event, tenantID, &inv, "active"); err != nil {
		return tenantID, err
	}
	// The ledger row, and the house extended to the subscription's period
	// end (WP-PAY-1).
	if err := h.recordInvoicePaid(ctx, event, tenantID, &inv); err != nil {
		return tenantID, err
	}

	h.logger.Info("payment succeeded",
		zap.String("tenant", tenantID),
		zap.String("invoice", inv.ID))
	return tenantID, nil
}

func (h *WebhookHandler) handlePaymentFailed(ctx context.Context, event stripe.Event) (string, error) {
	var inv stripe.Invoice
	if !h.unmarshal(event, "invoice", &inv) {
		return "", nil
	}

	customerID := ""
	if inv.Customer != nil {
		customerID = inv.Customer.ID
	}
	if customerID == "" {
		return "", nil
	}

	tenantID, ours, err := h.resolveTenant(ctx, event, customerID, invoiceTenantHint(&inv))
	if err != nil {
		return tenantID, fmt.Errorf("lookup tenant for failed payment: %w", err)
	}
	if !ours {
		return tenantID, nil
	}

	// past_due is the grace period: the house is kept (applyHouse grants
	// for past_due) while Stripe's Smart Retries run; the dashboard's
	// "after retries" setting then delivers canceled/unpaid, which clears it.
	if err := h.writeInvoiceStatus(ctx, event, tenantID, &inv, "past_due"); err != nil {
		return tenantID, err
	}
	if err := h.recordInvoiceFailed(ctx, tenantID, &inv); err != nil {
		return tenantID, err
	}

	h.logger.Warn("payment failed",
		zap.String("tenant", tenantID),
		zap.String("invoice", inv.ID))
	return tenantID, nil
}

// writeInvoiceStatus writes an invoice event's status onto the tenant when
// the invoice's subscription is the tenant's current one (RP11-1): an
// invoice of a deleted or replaced subscription, delivered late, must not
// mark a starter tenant active or the current subscription past_due. An
// invoice never adopts a subscription (checkout and the subscription events
// do), and one with no subscription (a one-off invoice) writes nothing.
func (h *WebhookHandler) writeInvoiceStatus(ctx context.Context, event stripe.Event, tenantID string, inv *stripe.Invoice, status string) error {
	if h.stripe.db == nil {
		return nil
	}
	subID := ""
	if inv.Subscription != nil {
		subID = inv.Subscription.ID
	}
	if subID == "" {
		return nil
	}
	res, err := h.stripe.db.ExecContext(ctx,
		`UPDATE tenants SET subscription_status = $1 WHERE id = $2 AND stripe_subscription_id = $3`,
		status, tenantID, subID)
	if err != nil {
		return fmt.Errorf("mark tenant %s %s: %w", tenantID, status, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.logger.Info("invoice status not written: not the tenant's current subscription",
			zap.String("id", event.ID), zap.String("tenant", tenantID),
			zap.String("subscription", subID), zap.String("status", status))
	}
	return nil
}

// handleSubscriptionUpdated applies customer.subscription.created/updated
// from the subscription as Stripe has it now — fetched, never the payload's
// status or items (R10-15): Stripe does not guarantee delivery order, and an
// older updated(active) delivered after updated(canceled) re-granted a
// cancelled house (the reverse took a paid one away). The payload names the
// subscription and its customer only.
func (h *WebhookHandler) handleSubscriptionUpdated(ctx context.Context, event stripe.Event) (string, error) {
	var payload stripe.Subscription
	if !h.unmarshal(event, "subscription", &payload) {
		return "", nil
	}

	customerID := ""
	if payload.Customer != nil {
		customerID = payload.Customer.ID
	}
	if customerID == "" || payload.ID == "" {
		return "", nil
	}

	tenantID, ours, err := h.resolveTenant(ctx, event, customerID, payload.Metadata["tenant_id"])
	if err != nil {
		return tenantID, fmt.Errorf("lookup tenant for subscription update: %w", err)
	}
	if !ours {
		return tenantID, nil
	}

	sub, err := h.fetchSubscription(ctx, payload.ID)
	if stripeMissing(err) {
		h.logger.Warn("stripe webhook: subscription event for a subscription Stripe does not have — nothing applied",
			zap.String("id", event.ID), zap.String("subscription", payload.ID), zap.String("tenant", tenantID))
		return tenantID, nil
	}
	if err != nil {
		return tenantID, fmt.Errorf("fetch subscription %s for %s: %w", payload.ID, event.Type, err)
	}

	status := string(sub.Status)

	// Determine plan from subscription metadata if available.
	plan := ""
	if sub.Metadata != nil {
		plan = sub.Metadata["plan"]
	}

	// The status (and plan) are the tenant's only for its current
	// subscription (RP11-1b): a stale event of an old subscription is not
	// written over the current one's. A tenant with none recorded adopts a
	// subscription that is not over (a created delivered before
	// checkout.session.completed); a canceled or incomplete_expired one is
	// never adopted. A stale event writes neither status nor plan.
	if h.stripe.db != nil {
		adopt := sub.Status != stripe.SubscriptionStatusCanceled &&
			sub.Status != stripe.SubscriptionStatusIncompleteExpired
		res, execErr := h.stripe.db.ExecContext(ctx,
			`UPDATE tenants
			    SET subscription_status = $1,
			        plan = CASE WHEN $2 = '' THEN plan ELSE $2 END,
			        stripe_subscription_id = $3
			  WHERE id = $4
			    AND (stripe_subscription_id = $3
			         OR ($5 AND (stripe_subscription_id IS NULL OR stripe_subscription_id = '')))`,
			status, plan, sub.ID, tenantID, adopt)
		if execErr != nil {
			return tenantID, fmt.Errorf("save subscription status for tenant %s: %w", tenantID, execErr)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			h.logger.Info("subscription status not written: not the tenant's current subscription",
				zap.String("id", event.ID), zap.String("tenant", tenantID),
				zap.String("subscription", sub.ID), zap.String("status", status))
		} else {
			h.logger.Info("subscription updated",
				zap.String("tenant", tenantID),
				zap.String("status", status))
		}
	}

	_, err = h.applyHouse(ctx, tenantID, sub)
	return tenantID, err
}

// applyHouse turns a house subscription into floor quotas and, in the same
// transaction, the entitlement record (WP-PAY-1, docs/PAYMENTS.md § 1.4):
// what the tenant bought, paid through which date. handled is false when the
// subscription is not a house (legacy pack) or nothing is wired; a non-nil
// error means the event must be retried.
//
//	active / trialing                → granted, paid_through = current_period_end
//	                                   (a trial is paid through its end)
//	past_due                         → granted (grace), paid_through = current_period_START:
//	                                   Stripe moved the period on when it raised the
//	                                   renewal invoice, and nothing was paid for it
//	canceled / unpaid / expired /
//	paused                           → entitlement_lapse ON for the tenant: renewal
//	                                   'prepaid', the house KEPT — the date and the
//	                                   lapse job decide (WP-PAY-2; the job is then
//	                                   the only writer of lapsed_at). OFF: a granted
//	                                   row paid into the future is kept the same way
//	                                   (RP26-1, payments.EndByStatus — paid time is
//	                                   never taken by a status); otherwise the house
//	                                   is cleared, data untouched, the entitlement
//	                                   marked lapsed and its renewal 'prepaid'.
//	incomplete                       → no change (first payment pending)
//
// A grant never shortens a date (payments.Grant), so a stale event cannot
// take paid time away. paused (WP-R10-2): a trial that ended without a
// payment method — Stripe creates no invoice while it lasts, so nothing is
// being paid; it is resumable, like unpaid, which is why HasLiveSubscription
// counts both.
//
// A clearing status is honoured only for the tenant's current subscription
// so a stale, older subscription's events cannot take a paid house away.
//
// The writes run in a transaction of their own beside the held claim (a
// second pooled connection, as the handlers' other queries): the claim's
// per-customer lock already serialises the customer's events, and the
// entitlement lock (payments.Lock) the tenant's grants.
func (h *WebhookHandler) applyHouse(ctx context.Context, tenantID string, sub *stripe.Subscription) (bool, error) {
	order, ok := h.stripe.HouseFromSubscription(sub)
	if !ok || h.quotas == nil || h.stripe.db == nil {
		return false, nil
	}
	switch sub.Status {
	case stripe.SubscriptionStatusActive, stripe.SubscriptionStatusTrialing, stripe.SubscriptionStatusPastDue:
		paidThrough := h.paidThrough(sub)
		renewal := payments.RenewalAuto
		if sub.CancelAtPeriodEnd {
			renewal = payments.RenewalPrepaid // nobody will extend it
		}
		var granted payments.Granted
		if err := payments.InTx(ctx, h.stripe.db, func(tx *sql.Tx) error {
			g, err := payments.GrantRecorded(ctx, tx, tenantID, order, paidThrough, payments.ProviderStripe, sub.ID, renewal)
			if err != nil {
				return err
			}
			granted = g
			_, err = tx.ExecContext(ctx,
				`UPDATE tenants
				    SET stripe_subscription_id = $1, subscription_status = $2, plan = 'house', house_period = $3
				  WHERE id = $4`,
				sub.ID, string(sub.Status), string(order.Period), tenantID)
			return err
		}); errors.Is(err, payments.ErrInvalidOrder) {
			h.houseInvalid(ctx, tenantID, sub, order, err)
			return true, nil
		} else if err != nil {
			return true, fmt.Errorf("grant house for tenant %s: %w", tenantID, err)
		}
		h.auditGrant(ctx, tenantID, granted, sub.ID)
		h.logger.Info("house granted",
			zap.String("tenant", tenantID), zap.String("subscription", sub.ID),
			zap.Int("std_tb", order.Std), zap.Int("vault_tb", order.Vault), zap.Int("pin_hot_tb", order.PinHot),
			zap.String("period", string(order.Period)), zap.String("status", string(sub.Status)),
			zap.Time("paid_through", paidThrough), zap.String("renewal", string(renewal)))
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
		if h.dateDecides(tenantID) {
			var stopped bool
			if err := payments.InTx(ctx, h.stripe.db, func(tx *sql.Tx) error {
				var err error
				if stopped, err = payments.StopRenewalChanged(ctx, tx, tenantID); err != nil {
					return err
				}
				_, err = tx.ExecContext(ctx,
					`UPDATE tenants SET subscription_status = $1 WHERE id = $2`, string(sub.Status), tenantID)
				return err
			}); err != nil {
				return true, fmt.Errorf("stop renewal for tenant %s: %w", tenantID, err)
			}
			if stopped {
				payments.AuditRenewal(ctx, h.stripe.db, tenantID, payments.RenewalPrepaid, webhookActor, payments.ProviderStripe, sub.ID)
			}
			h.logger.Warn("renewal stopped — the house runs to paid_through + grace (entitlement_lapse)",
				zap.String("tenant", tenantID), zap.String("subscription", sub.ID), zap.String("status", string(sub.Status)))
			return true, nil
		}
		var ended payments.Ended
		if err := payments.InTx(ctx, h.stripe.db, func(tx *sql.Tx) error {
			var err error
			if ended, err = payments.EndByStatus(ctx, tx, tenantID, time.Now()); err != nil {
				return err
			}
			if ended.Kept {
				_, err = tx.ExecContext(ctx,
					`UPDATE tenants SET subscription_status = $1 WHERE id = $2`, string(sub.Status), tenantID)
				return err
			}
			_, err = tx.ExecContext(ctx,
				`UPDATE tenants SET subscription_status = $1, house_period = '' WHERE id = $2`,
				string(sub.Status), tenantID)
			return err
		}); err != nil {
			return true, fmt.Errorf("clear house for tenant %s: %w", tenantID, err)
		}
		if ended.Kept {
			h.keptByDate(ctx, tenantID, sub, string(sub.Status), ended)
			return true, nil
		}
		h.logger.Warn("house cleared — back to the free tier, data untouched",
			zap.String("tenant", tenantID), zap.String("subscription", sub.ID), zap.String("status", string(sub.Status)))
	}
	return true, nil
}

// auditGrant writes billing.house_granted for a grant that changed the
// row and billing.renewal_stopped / _resumed when the grant flipped the
// renewal of an existing row (cancel_at_period_end seen by the webhook).
func (h *WebhookHandler) auditGrant(ctx context.Context, tenantID string, g payments.Granted, subID string) {
	payments.AuditGranted(ctx, h.stripe.db, tenantID, g, webhookActor)
	if g.Changed && g.Previous != nil && g.Previous.Renewal != g.Renewal {
		payments.AuditRenewal(ctx, h.stripe.db, tenantID, g.Renewal, webhookActor, payments.ProviderStripe, subID)
	}
}

// houseInvalid acknowledges a subscription whose items are no valid house
// (payments.ErrInvalidOrder: an operator deleted the Standard item at Stripe
// and kept pin-hot, say). Retrying cannot change the items, and a 500 would
// be retried for three days until Stripe disables the endpoint (RP20-4), so
// the event is handled: the house and the entitlement are left exactly as
// they were, and the operator is told — one Error line, an audit row and
// vaultaire_stripe_house_invalid_total.
func (h *WebhookHandler) houseInvalid(ctx context.Context, tenantID string, sub *stripe.Subscription, o HouseOrder, err error) {
	houseInvalidTotal.Inc()
	h.logger.Error("stripe webhook: the subscription is no valid house — acknowledged, the house left as it was",
		zap.String("tenant", tenantID), zap.String("subscription", sub.ID),
		zap.Int("std_tb", o.Std), zap.Int("vault_tb", o.Vault), zap.Int("pin_hot_tb", o.PinHot),
		zap.String("period", string(o.Period)), zap.Error(err))
	audit.Record(ctx, h.stripe.db, audit.Entry{TenantID: tenantID, Action: "billing.house_invalid",
		Resource: sub.ID, Severity: "error", Metadata: map[string]any{
			"tenant": tenantID, "subscription": sub.ID, "std": o.Std, "vault": o.Vault, "pin_hot": o.PinHot,
			"period": string(o.Period)}})
	// The subscription is Stripe's and still bills: record it — outside the
	// rolled-back grant — so the deletion runner cancels it (RP22-3; a
	// payment_events row since WP-PAY-7, the tenants columns are dropped).
	if h.stripe.db != nil {
		if err := payments.RecordInvalidHouse(ctx, h.stripe.db, tenantID, sub.ID, string(sub.Status)); err != nil {
			h.logger.Error("stripe webhook: could not record the invalid house's subscription",
				zap.String("tenant", tenantID), zap.String("subscription", sub.ID), zap.Error(err))
		}
	}
}

// keptByDate reports a terminal status that ended the renewal of a house
// still paid into the future (payments.EndByStatus, RP26-1): nothing was
// revoked, the house runs to the kept date + grace.
func (h *WebhookHandler) keptByDate(ctx context.Context, tenantID string, sub *stripe.Subscription, status string, ended payments.Ended) {
	if ended.Stopped {
		payments.AuditRenewal(ctx, h.stripe.db, tenantID, payments.RenewalPrepaid, webhookActor, payments.ProviderStripe, sub.ID)
	}
	h.logger.Warn("terminal status on a house paid into the future — renewal stopped, the house kept to paid_through + grace",
		zap.String("tenant", tenantID), zap.String("subscription", sub.ID), zap.String("status", status),
		zap.Time("paid_through", ended.PaidThrough))
}

// paidThrough is the date a granting status pays the house through: the end
// of Stripe's current period, or its start while past_due (the period
// Stripe moved on to is unpaid). A subscription with no period on record
// (never seen from Stripe) is paid through now — grace starts at once.
func (h *WebhookHandler) paidThrough(sub *stripe.Subscription) time.Time {
	at := sub.CurrentPeriodEnd
	if sub.Status == stripe.SubscriptionStatusPastDue {
		at = sub.CurrentPeriodStart
	}
	if at <= 0 {
		h.logger.Warn("stripe subscription carries no period — paid through now",
			zap.String("subscription", sub.ID), zap.String("status", string(sub.Status)))
		return time.Now().UTC()
	}
	return time.Unix(at, 0).UTC()
}

// isCurrentSubscription reports whether subID is the tenant's current
// subscription: the entitlement row's provider_ref (a Stripe row), else
// tenants.stripe_subscription_id for a tenant with no row (RP20-A3), or
// the tenant has none recorded. A lookup error is returned, not treated as
// "current" — clearing a house on a DB hiccup is the wrong default.
func (h *WebhookHandler) isCurrentSubscription(ctx context.Context, tenantID, subID string) (bool, error) {
	current, err := h.stripe.currentSubscription(ctx, tenantID)
	if err != nil {
		return false, fmt.Errorf("read current subscription for tenant %s: %w", tenantID, err)
	}
	return current == "" || current == subID, nil
}

// handleSubscriptionDeleted applies customer.subscription.deleted from the
// payload: canceled is terminal at Stripe (a deleted subscription is never
// reactivated), so a fetch cannot tell anything newer; an older updated
// delivered after it fetches the canceled state and clears again.
func (h *WebhookHandler) handleSubscriptionDeleted(ctx context.Context, event stripe.Event) (string, error) {
	var sub stripe.Subscription
	if !h.unmarshal(event, "subscription", &sub) {
		return "", nil
	}

	customerID := ""
	if sub.Customer != nil {
		customerID = sub.Customer.ID
	}
	if customerID == "" {
		return "", nil
	}

	tenantID, ours, err := h.resolveTenant(ctx, event, customerID, sub.Metadata["tenant_id"])
	if err != nil {
		return tenantID, fmt.Errorf("lookup tenant for subscription delete: %w", err)
	}
	if !ours {
		return tenantID, nil
	}

	// A deleted subscription that is not the tenant's current one (an old
	// pack, a replaced house) must not downgrade anything.
	if h.stripe.db != nil {
		current, err := h.isCurrentSubscription(ctx, tenantID, sub.ID)
		if err != nil {
			return tenantID, err
		}
		if !current {
			h.logger.Info("ignoring deletion of a stale subscription",
				zap.String("tenant", tenantID), zap.String("subscription", sub.ID))
			return tenantID, nil
		}
	}

	// entitlement_lapse ON, for a HOUSE: nobody renews it any more, the
	// house runs to paid_through + grace and the lapse job clears it
	// (WP-PAY-2). A non-house subscription (a legacy pack) keeps today's
	// downgrade below, flag or no flag (RP22-2).
	_, isHouse := h.stripe.HouseFromSubscription(&sub)
	if isHouse && h.dateDecides(tenantID) && h.stripe.db != nil {
		var stopped bool
		if err := payments.InTx(ctx, h.stripe.db, func(tx *sql.Tx) error {
			var err error
			if stopped, err = payments.StopRenewalChanged(ctx, tx, tenantID); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx,
				`UPDATE tenants SET subscription_status = 'canceled', stripe_subscription_id = NULL WHERE id = $1`, tenantID)
			return err
		}); err != nil {
			return tenantID, fmt.Errorf("stop renewal on delete for tenant %s: %w", tenantID, err)
		}
		if stopped {
			payments.AuditRenewal(ctx, h.stripe.db, tenantID, payments.RenewalPrepaid, webhookActor, payments.ProviderStripe, sub.ID)
		}
		h.logger.Info("subscription deleted — renewal stopped, the house runs to paid_through + grace",
			zap.String("tenant", tenantID))
		return tenantID, nil
	}

	// A house: floor quotas go, data stays; the entitlement is marked lapsed
	// and nobody renews it (WP-PAY-1) — unless it is paid into the future,
	// which only stops the renewal (RP26-1).
	if isHouse && h.quotas != nil && h.stripe.db != nil {
		var ended payments.Ended
		if err := payments.InTx(ctx, h.stripe.db, func(tx *sql.Tx) error {
			var err error
			if ended, err = payments.EndByStatus(ctx, tx, tenantID, time.Now()); err != nil || !ended.Kept {
				return err
			}
			_, err = tx.ExecContext(ctx,
				`UPDATE tenants SET subscription_status = 'canceled', stripe_subscription_id = NULL WHERE id = $1`, tenantID)
			return err
		}); err != nil {
			return tenantID, fmt.Errorf("clear house quotas on delete for tenant %s: %w", tenantID, err)
		}
		if ended.Kept {
			h.keptByDate(ctx, tenantID, &sub, "deleted", ended)
			return tenantID, nil
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
			return tenantID, fmt.Errorf("downgrade tenant %s after subscription delete: %w", tenantID, err)
		}
	}

	h.logger.Info("subscription deleted — downgraded to starter",
		zap.String("tenant", tenantID))
	return tenantID, nil
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
