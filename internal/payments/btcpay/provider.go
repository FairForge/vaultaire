package btcpay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Provider is the BTCPay payments.Provider (docs/PAYMENTS.md § 3): prepaid
// — a settled invoice extends paid_through, nothing renews by itself.
type Provider struct {
	cfg    Config
	client *Client
	db     *sql.DB
	logger *zap.Logger
	now    func() time.Time

	// receipts mails the receipt of a newly settled, numbered row (nil =
	// none; payments.Mailer in production).
	receipts ReceiptSender
	// stopper runs pending renewal stops after a commit (nil = the
	// renewal_stops job does, within its interval).
	stopper RenewalStopper

	mu       sync.Mutex
	state    payments.Readiness
	reason   string
	failures int
}

var (
	_ payments.Provider           = (*Provider)(nil)
	_ payments.LockedCheckout     = (*Provider)(nil)
	_ payments.InvoiceInvalidator = (*Provider)(nil)
	_ payments.InvoiceCanceller   = (*Provider)(nil)
)

// New builds the provider over a complete Config. Readiness is Pending
// until the first check (Start).
func New(cfg Config, db *sql.DB, logger *zap.Logger) *Provider {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Provider{cfg: cfg, client: NewClient(cfg.URL, cfg.StoreID, cfg.APIKey), db: db, logger: logger,
		now: time.Now, state: payments.Pending, reason: "no check completed yet"}
}

// ReceiptSender mails a receipt (payments.Mailer). Best effort, after the
// commit.
type ReceiptSender interface {
	SendReceipt(ctx context.Context, tenantID, paymentID string) error
}

// RenewalStopper runs the tenant's pending renewal stops
// (payments.Registry.StopPendingRenewals) — after a delivery whose settle
// took over a house another provider renewed (WP-PAY-5).
type RenewalStopper interface {
	StopPendingRenewals(ctx context.Context, tenantID string) (payments.StopRun, error)
}

// SetRenewalStopper attaches it (the registry, in production).
func (p *Provider) SetRenewalStopper(s RenewalStopper) { p.stopper = s }

// SetReceiptSender attaches the receipt mailer.
func (p *Provider) SetReceiptSender(r ReceiptSender) { p.receipts = r }

// Name is "btcpay".
func (p *Provider) Name() string { return payments.ProviderBTCPay }

// Renewal: a crypto payment is prepaid — nobody extends the date but the
// customer paying again.
func (p *Provider) Renewal() payments.Renewal { return payments.RenewalPrepaid }

// ── readiness (docs/PAYMENTS.md § 3.7 "When BTCPay is down") ────────────

// unreadyAfter consecutive failed checks make the provider Unready; one
// success makes it Ready again.
const unreadyAfter = 2

// Ready answers from the last checks (never a network call: the registry
// asks on every checkout and every minute): Pending before the first check
// completed, Unready after two consecutive failures, else Ready.
func (p *Provider) Ready(context.Context) (payments.Readiness, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state, p.reason
}

// Check runs one readiness check: GET /api/v1/health — its body, since
// BTCPay answers 200 {"synchronized":false} while its node is down — and a
// read of the store with the scoped key (GET …/invoices?take=1: the key
// may not read the store's settings — the lab).
func (p *Provider) Check(ctx context.Context) {
	err := p.probe(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		if p.state != payments.Ready {
			p.logger.Info("btcpay: ready", zap.String("store", p.cfg.StoreID))
		}
		p.state, p.reason, p.failures = payments.Ready, "", 0
		return
	}
	p.failures++
	if p.failures >= unreadyAfter {
		if p.state != payments.Unready {
			p.logger.Warn("btcpay: unready", zap.Int("failures", p.failures), zap.Error(err))
		}
		p.state, p.reason = payments.Unready, err.Error()
	}
}

func (p *Provider) probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*RequestTimeout)
	defer cancel()
	synced, err := p.client.Health(ctx)
	if err != nil {
		return fmt.Errorf("health: %w", err)
	}
	if !synced {
		return errors.New("health: BTCPay is not synchronized with its node")
	}
	if err := p.client.ProbeInvoices(ctx); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// Start checks at once, then every interval until ctx ends.
func (p *Provider) Start(ctx context.Context, every time.Duration) {
	go func() {
		p.Check(ctx)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.Check(ctx)
			}
		}
	}()
}

// ── checkout ─────────────────────────────────────────────────────────────

// Checkout policy (docs/PAYMENTS.md § 3.2–3.3).
const (
	expirationMinutes = 30
	monitoringMinutes = 1440
	// lowMediumFromCents: an invoice of $200 or more waits for two
	// confirmations (LowMediumSpeed), under it one (MediumSpeed).
	lowMediumFromCents = 20000
)

// speedPolicy is the invoice's confirmation policy by its amount.
func speedPolicy(cents int64) string {
	if cents >= lowMediumFromCents {
		return SpeedLowMedium
	}
	return SpeedMedium
}

// StartCheckout without the guard's transaction (a caller outside
// payments.StartCheckout): an order the credit covers in full cannot be
// granted here (it needs the entitlement lock) and is refused.
func (p *Provider) StartCheckout(ctx context.Context, c payments.Checkout) (string, error) {
	return p.startCheckout(ctx, nil, c, nil)
}

// StartCheckoutTx is the checkout under the double-billing guard
// (payments.StartCheckout holds the tenant's entitlement lock in tx).
func (p *Provider) StartCheckoutTx(ctx context.Context, tx *sql.Tx, c payments.Checkout) (string, error) {
	return p.startCheckout(ctx, tx, c, nil)
}

// ErrNeedsLock: the order is covered by the tenant's credit, which only the
// guarded checkout can grant.
var ErrNeedsLock = errors.New("btcpay: an order the credit covers must go through payments.StartCheckout")

// startCheckout: the ledger row is written PENDING and committed BEFORE the
// invoice exists (a crash between the two leaves a pending row and no
// invoice — never an invoice the ledger does not know), the invoice is
// created for the order's charge less the tenant's ledger credit, and its
// id replaces the row's provisional one. topUp, when set, prices a resize
// instead of a period.
func (p *Provider) startCheckout(ctx context.Context, tx *sql.Tx, c payments.Checkout, topUp *TopUp) (string, error) {
	if p.db == nil {
		return "", errors.New("btcpay: no database")
	}
	if err := payments.ValidateHouseOrder(c.Order); err != nil {
		return "", fmt.Errorf("house order: %w", err)
	}
	purpose := c.Purpose
	if purpose == "" {
		purpose = payments.PurposeNew
	}
	charge := payments.QuoteHouse(c.Order.Std, c.Order.Vault, c.Order.PinHot, c.Order.Period).ChargeCents
	if topUp != nil {
		charge, purpose = topUp.Cents, payments.PurposeResize
	}
	// The ledger is the credit's truth (Checkout.CreditCents is not
	// trusted): what overpayments and expired partial payments left.
	var q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	} = p.db
	if tx != nil {
		q = tx
	}
	credit, err := payments.CreditBalance(ctx, q, c.TenantID)
	if err != nil {
		return "", err
	}
	if credit > charge {
		credit = charge
	}
	due := charge - credit
	order := c.Order
	if due <= 0 {
		// Nothing to pay in crypto: the credit buys it. A $0 payment takes no
		// receipt number (RP24-1's rule) and needs no invoice.
		if tx == nil {
			return "", ErrNeedsLock
		}
		if err := p.grantFromCredit(ctx, tx, c, purpose, charge); err != nil {
			return "", err
		}
		return c.ReturnURL, nil
	}

	ledgerID := uuid.New().String()
	provisional := "pending:" + ledgerID
	meta := map[string]any{"tenant_id": c.TenantID, "payment_id": ledgerID, "purpose": purpose,
		"order":  map[string]any{"std_tb": order.Std, "vault_tb": order.Vault, "pin_hot_tb": order.PinHot, "period": string(order.Period)},
		"period": string(order.Period), "charge_cents": charge, "credit_cents": credit, "orderId": ledgerID}
	if topUp != nil {
		meta["top_up"] = topUp.Metadata()
	}
	if err := payments.InTx(ctx, p.db, func(ptx *sql.Tx) error {
		_, err := payments.RecordPending(ctx, ptx, payments.Payment{TenantID: c.TenantID, Provider: payments.ProviderBTCPay,
			ProviderID: provisional, Kind: payments.KindCharge, Purpose: purpose, AmountCents: due, CreditCents: credit,
			Order: &order, Actor: "customer", Metadata: map[string]any{payments.MetaIDs: map[string]any{"ledger": ledgerID}}})
		return err
	}); err != nil {
		return "", err
	}
	yes := true
	inv, err := p.client.CreateInvoice(ctx, CreateInvoiceRequest{
		Amount: usdString(due), Currency: "USD", Metadata: meta,
		Checkout: CheckoutOptions{SpeedPolicy: speedPolicy(due), ExpirationMinutes: expirationMinutes,
			MonitoringMinutes: monitoringMinutes, RedirectURL: c.ReturnURL, RedirectAutomatically: c.ReturnURL != ""},
		Receipt: &ReceiptOptions{Enabled: &yes, ShowPayments: &yes},
	})
	if err != nil {
		// No invoice: the row will never be paid.
		if ferr := payments.InTx(ctx, p.db, func(ptx *sql.Tx) error {
			_, err := payments.Fail(ctx, ptx, payments.Payment{Provider: payments.ProviderBTCPay, ProviderID: provisional,
				Kind: payments.KindCharge, Purpose: purpose})
			return err
		}); ferr != nil {
			p.logger.Warn("btcpay: pending row of a failed invoice not marked failed", zap.String("tenant", c.TenantID), zap.Error(ferr))
		}
		return "", fmt.Errorf("btcpay: create invoice: %w: %w", payments.ErrProviderUnavailable, err)
	}
	if err := payments.AdoptPending(ctx, p.db, payments.ProviderBTCPay, provisional, inv.ID); err != nil {
		// The webhook adopts it from the invoice's metadata (payment_id).
		p.logger.Error("btcpay: invoice id not written onto its pending row — the webhook adopts it from the metadata",
			zap.String("tenant", c.TenantID), zap.String("invoice", inv.ID), zap.Error(err))
	}
	p.logger.Info("btcpay: invoice created", zap.String("tenant", c.TenantID), zap.String("invoice", inv.ID),
		zap.String("purpose", purpose), zap.Int64("due_cents", due), zap.Int64("credit_cents", credit))
	if inv.CheckoutLink == "" {
		return "", fmt.Errorf("btcpay: invoice %s has no checkout link", inv.ID)
	}
	return inv.CheckoutLink, nil
}

// grantFromCredit settles a $0 charge (unnumbered, the credit consumed in
// credit_cents) and applies it, in the guard's transaction.
func (p *Provider) grantFromCredit(ctx context.Context, tx *sql.Tx, c payments.Checkout, purpose string, charge int64) error {
	order := c.Order
	id := "credit:" + uuid.New().String()
	now := p.now().UTC()
	start, end, sw, err := p.applyOrder(ctx, tx, c.TenantID, purpose, order, id)
	if err != nil {
		return err
	}
	if sw != nil {
		// The guard refused this checkout while another provider renewed by
		// itself, so this is a switch the customer asked for (its renewal
		// already stopped): the row only keeps the subscription findable.
		p.logger.Info("btcpay: house switched to bitcoin from the ledger credit", zap.String("tenant", c.TenantID),
			zap.String("from", sw.from), zap.Bool("implicit", sw.implicit))
	}
	if _, err := payments.Settle(ctx, tx, payments.Payment{TenantID: c.TenantID, Provider: payments.ProviderBTCPay, ProviderID: id,
		Kind: payments.KindCharge, Purpose: purpose, AmountCents: 0, CreditCents: charge, Order: &order,
		PeriodStart: start, PeriodEnd: end, Actor: "customer", Unnumbered: true, Note: "paid from the ledger credit"}, now); err != nil {
		return err
	}
	p.logger.Info("btcpay: order paid from the ledger credit", zap.String("tenant", c.TenantID), zap.Int64("credit_cents", charge))
	return nil
}

// switched is a grant that took the house from another provider (WP-PAY-5):
// its subscription is remembered in a renewal_stop row written in the same
// transaction.
type switched struct {
	from, ref string
	// implicit: the other provider still renewed by itself (RP30-2) — its
	// renewal must be stopped (the row is pending); else the customer had
	// asked for the switch and the renewal was stopped then.
	implicit bool
}

// applyOrder grants (new / renew / switch: one more period on the later of
// paid_through and now, provider btcpay, renewal prepaid, the pending
// provider cleared) or resizes (the house changes, the date stays). It
// returns the period the payment bought, and — when the grant took the
// house from another provider's subscription — that subscription.
//
// A house another provider renews by itself is never taken over silently
// (RP30-2, an implicit switch): the BTC period is appended (the start is
// paid_through), and the other provider's subscription is written into a
// renewal_stop row (pending) in this transaction — the stop itself is a
// provider call made after the commit (and retried by the renewal_stops
// job), never one that could turn this delivery into a 5xx that stalls the
// store's webhook queue (RP30-4). Until the stop is confirmed the row keeps
// the subscription findable: the deletion runner cancels it, the Stripe
// webhook never lets it take the house back.
func (p *Provider) applyOrder(ctx context.Context, tx *sql.Tx, tenantID, purpose string, o payments.Order, ref string) (start, end time.Time, sw *switched, err error) {
	if err := payments.Lock(ctx, tx, tenantID); err != nil {
		return start, end, nil, err
	}
	if purpose == payments.PurposeResize {
		e, err := payments.Get(ctx, tx, tenantID)
		if err != nil {
			return start, end, nil, err
		}
		if err := payments.Resize(ctx, tx, tenantID, o.House()); err != nil {
			return start, end, nil, err
		}
		return p.now().UTC(), e.PaidThrough, nil, nil
	}
	start = p.now().UTC()
	prev, err := payments.Get(ctx, tx, tenantID)
	switch {
	case errors.Is(err, payments.ErrNoEntitlement):
	case err != nil:
		return start, end, nil, err
	default:
		if prev.LapsedAt == nil && prev.PaidThrough.After(start) {
			start = prev.PaidThrough.UTC()
		}
		if prev.Provider != payments.ProviderBTCPay && prev.ProviderRef != "" &&
			(prev.Renewal == payments.RenewalAuto || prev.Provider == payments.ProviderStripe) {
			sw = &switched{from: prev.Provider, ref: prev.ProviderRef, implicit: prev.Renewal == payments.RenewalAuto}
			status, reason := payments.RenewalStopStopped, payments.ReasonSwitch
			if sw.implicit {
				status, reason = payments.RenewalStopPending, payments.ReasonImplicitSwitch
			}
			if err := payments.RecordRenewalStop(ctx, tx, tenantID, prev.Provider, prev.ProviderRef, reason, status,
				map[string]any{"replaced_by": payments.ProviderBTCPay + ":" + ref,
					"paid_through_before": prev.PaidThrough.UTC().Format(time.RFC3339)}); err != nil {
				return start, end, nil, err
			}
		}
	}
	if end, err = payments.PaidThroughAfter(ctx, tx, tenantID, o.Period); err != nil {
		return start, end, nil, err
	}
	if _, err := payments.GrantRecorded(ctx, tx, tenantID, o, end, payments.ProviderBTCPay, ref, payments.RenewalPrepaid); err != nil {
		return start, end, nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE entitlements SET pending_provider = NULL WHERE tenant_id = $1 AND pending_provider IS NOT NULL`, tenantID); err != nil {
		return start, end, nil, fmt.Errorf("clear pending provider of tenant %s: %w", tenantID, err)
	}
	return start, end, sw, nil
}

// Resize: up = a top-up invoice for the rest of the period (§ 3.5's
// formula, its inputs in the invoice metadata) — the house changes when it
// settles; down (or nothing left to pay) = in place, no money moves (the
// Registry records it).
func (p *Provider) Resize(ctx context.Context, tenantID string, o payments.Order) (payments.ResizeOutcome, error) {
	if p.db == nil {
		return payments.ResizeOutcome{}, errors.New("btcpay: no database")
	}
	e, err := payments.Get(ctx, p.db, tenantID)
	if err != nil {
		return payments.ResizeOutcome{}, err
	}
	if e.LapsedAt != nil {
		return payments.ResizeOutcome{}, fmt.Errorf("tenant %s: the house has lapsed — buy it again", tenantID)
	}
	if o.Period == "" {
		o.Period = e.Order.Period
	}
	t, err := PriceTopUp(e.Order, o, e.PaidThrough, p.now())
	if err != nil {
		return payments.ResizeOutcome{}, err
	}
	if t.Cents <= 0 {
		applied := o
		return payments.ResizeOutcome{Applied: &applied}, nil
	}
	u, err := p.startCheckout(ctx, nil, payments.Checkout{TenantID: tenantID, Order: o, Purpose: payments.PurposeResize}, &t)
	if errors.Is(err, ErrNeedsLock) {
		// The credit covers the top-up: in place, the credit consumed.
		err = payments.InTx(ctx, p.db, func(tx *sql.Tx) error {
			return p.grantFromCredit(ctx, tx, payments.Checkout{TenantID: tenantID, Order: o}, payments.PurposeResize, t.Cents)
		})
		if err != nil {
			return payments.ResizeOutcome{}, err
		}
		applied := o
		return payments.ResizeOutcome{Applied: &applied}, nil
	}
	if err != nil {
		return payments.ResizeOutcome{}, err
	}
	return payments.ResizeOutcome{RedirectURL: u}, nil
}

// unpaidForInvalid reports whether the fetched invoice may be marked
// Invalid: provably unpaid — New or Expired with a paidAmount of 0
// (RP31-1). Processing is paid (a payment seen, not confirmed yet), a
// partly-paid New or Expired invoice holds money, Settled and Invalid are
// final: none of them is ours to invalidate. BTCPay 2.4.5 would let us
// (InvoiceEntity.CanMarkInvalid = New|Processing|Expired) — the rule is
// ours.
func unpaidForInvalid(inv Invoice) (bool, error) {
	paid, err := usdToCents(inv.PaidAmount)
	if err != nil {
		return false, fmt.Errorf("invoice %s: paidAmount: %w", inv.ID, err)
	}
	return (inv.Status == StatusNew || inv.Status == StatusExpired) && paid == 0, nil
}

// InvalidateOpenInvoices marks the tenant's open (pending / processing)
// invoices Invalid at BTCPay (payments.InvoiceInvalidator — the portal
// race withdraws a pending switch, docs/PAYMENTS.md § 4.1 step 5) — only
// the ones BTCPay says are provably unpaid (unpaidForInvalid, each fetched
// first): one that holds money is left to settle (its settle is an
// implicit switch, which asserts the other provider's stop again) and is
// counted vaultaire_payment_webhook_total{provider="btcpay",outcome=
// "invalidate_skipped_paid"} with a Warn (RP31-1). Each mark is audited
// billing.invoice_marked with the reason; BTCPay's InvoiceInvalid delivery
// that follows moves our row (the fetched state, which credits anything
// paid after the fetch). A fetch that fails marks nothing; a refused mark
// (a 4xx) is an Error and outcome mark_refused (pages — RP30-4), another
// failure an Error; none fails the caller.
func (p *Provider) InvalidateOpenInvoices(ctx context.Context, tenantID, reason string) (int, error) {
	if p.db == nil {
		return 0, nil
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT provider_id FROM payments
		 WHERE tenant_id = $1 AND provider = 'btcpay' AND kind = 'charge' AND status IN ('pending', 'processing')
		   AND provider_id NOT LIKE 'pending:%'
		 ORDER BY created_at`, tenantID)
	if err != nil {
		return 0, fmt.Errorf("open invoices of tenant %s: %w", tenantID, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("open invoices of tenant %s: %w", tenantID, err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("open invoices of tenant %s: %w", tenantID, err)
	}
	marked := 0
	for _, id := range ids {
		inv, err := p.client.GetInvoice(ctx, id)
		if err != nil {
			p.logger.Error("btcpay: an open invoice could not be read, so it is not marked Invalid — check it in BTCPay",
				zap.String("tenant", tenantID), zap.String("invoice", id), zap.String("reason", reason), zap.Error(err))
			continue
		}
		unpaid, err := unpaidForInvalid(inv)
		if err != nil || !unpaid {
			payments.CountWebhook(payments.ProviderBTCPay, payments.WebhookInvalidateSkippedPaid)
			p.logger.Warn("btcpay: an open invoice holds a payment — left to settle, not marked Invalid",
				zap.String("tenant", tenantID), zap.String("invoice", id), zap.String("status", inv.Status),
				zap.String("paid", inv.PaidAmount), zap.String("reason", reason), zap.Error(err))
			continue
		}
		if !p.markInvalid(ctx, tenantID, id, reason, "payments") {
			continue
		}
		marked++
	}
	return marked, nil
}

// markInvalid marks one invoice Invalid at BTCPay and audits it; false
// (logged, a refusal counted mark_refused) when BTCPay did not.
func (p *Provider) markInvalid(ctx context.Context, tenantID, id, reason, actor string) bool {
	if _, err := p.client.MarkStatus(ctx, id, StatusInvalid); err != nil {
		if IsPermanent(err) {
			payments.CountWebhook(payments.ProviderBTCPay, payments.WebhookMarkRefused)
		}
		p.logger.Error("btcpay: an open invoice could not be marked Invalid — mark it by hand in BTCPay",
			zap.String("tenant", tenantID), zap.String("invoice", id), zap.String("reason", reason), zap.Error(err))
		return false
	}
	audit.Record(ctx, p.db, audit.Entry{TenantID: tenantID, EventType: "billing", Action: "billing.invoice_marked",
		Resource: "btcpay-invoice:" + id, Metadata: map[string]any{"tenant": tenantID, "actor": actor,
			"invoice": id, "status": StatusInvalid, "reason": reason}})
	return true
}

// ReasonCustomerCancel is the reason of an invoice the customer withdrew
// from the crypto_invoice_open page (RP31-5).
const ReasonCustomerCancel = "customer_cancel"

// CancelUnpaidInvoice withdraws an open invoice of the tenant nobody paid
// (payments.InvoiceCanceller, RP31-5): our open row must be the tenant's
// (else ErrNoSuchPayment), BTCPay's invoice provably unpaid
// (unpaidForInvalid; else ErrInvoicePaid, an *OpenInvoiceError with its
// date and Paid set — it is left to settle), then it
// is marked Invalid at BTCPay and — under the tenant's entitlement lock —
// our row is closed (invalid) at once, so the card checkout's guard lets
// the customer through without waiting for BTCPay's delivery. Audited
// billing.invoice_marked, reason customer_cancel. A payment that reaches
// the invoice after the fetch is credited by the InvoiceInvalid delivery
// (apply's StatusInvalid branch).
func (p *Provider) CancelUnpaidInvoice(ctx context.Context, tenantID, invoiceID, actor string) error {
	if p.db == nil {
		return errors.New("btcpay: no database")
	}
	open, err := payments.OpenInvoiceOf(ctx, p.db, tenantID, payments.ProviderBTCPay, invoiceID)
	if err != nil {
		return err
	}
	inv, err := p.client.GetInvoice(ctx, invoiceID)
	if err != nil {
		return fmt.Errorf("read invoice %s: %w: %w", invoiceID, payments.ErrProviderUnavailable, err)
	}
	unpaid, err := unpaidForInvalid(inv)
	if err != nil {
		return err
	}
	if !unpaid {
		payments.CountWebhook(payments.ProviderBTCPay, payments.WebhookInvalidateSkippedPaid)
		open.Paid = true
		return fmt.Errorf("invoice %s is %s with %s paid: %w", invoiceID, inv.Status, inv.PaidAmount, open)
	}
	if !p.markInvalid(ctx, tenantID, invoiceID, ReasonCustomerCancel, actor) {
		return fmt.Errorf("mark invoice %s invalid: %w", invoiceID, payments.ErrProviderUnavailable)
	}
	// BTCPay has it Invalid now: a failure from here on is not "nothing
	// changed" (RP33-8b) — the InvoiceInvalid delivery closes our row.
	err = payments.InTx(ctx, p.db, func(tx *sql.Tx) error {
		if err := payments.Lock(ctx, tx, tenantID); err != nil {
			return err
		}
		if _, err := payments.Invalidate(ctx, tx, payments.ProviderBTCPay, invoiceID); err != nil {
			return err
		}
		// An expired row is closed for the guard too.
		if _, err := tx.ExecContext(ctx, `UPDATE payments SET status = 'invalid'
			 WHERE provider = 'btcpay' AND provider_id = $1 AND status = 'expired'`, invoiceID); err != nil {
			return fmt.Errorf("close expired invoice %s: %w", invoiceID, err)
		}
		return nil
	})
	if err != nil {
		p.logger.Warn("btcpay: invoice marked Invalid at BTCPay, our row not closed yet — its InvoiceInvalid delivery closes it",
			zap.String("tenant", tenantID), zap.String("invoice", invoiceID), zap.Error(err))
		return fmt.Errorf("invoice %s cancelled at BTCPay, row not closed: %w: %w", invoiceID, payments.ErrInvoiceCancelPending, err)
	}
	return nil
}

// StopRenewal: nothing renews a prepaid house by itself.
func (p *Provider) StopRenewal(context.Context, string) error { return nil }

// ResumeRenewal: nothing to resume — the customer pays again.
func (p *Provider) ResumeRenewal(context.Context, string) error { return nil }

// Cancel: nothing to cancel (no subscription; an open invoice expires by
// itself). The deletion runner treats a btcpay row as nothing to stop.
func (p *Provider) Cancel(context.Context, string) error { return nil }

// ManageURL: our own billing page is the place.
func (p *Provider) ManageURL(context.Context, string, string) (string, error) { return "", nil }

// Webhook is POST /webhook/btcpay.
func (p *Provider) Webhook() http.Handler { return http.HandlerFunc(p.ServeHTTP) }
