package btcpay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/payments"
	"go.uber.org/zap"
)

// The webhook (docs/PAYMENTS.md § 2.4) and the invoice state table (§ 3.3)
// as code. A delivery is verified (BTCPay-Sig), claimed in payment_events
// under its dedup key, resolved to OUR pending ledger row for the invoice
// (never to the payload's tenant), and the FETCHED invoice — never the
// payload — is applied in the claim's transaction, under the tenant's
// entitlement lock: the claim, the ledger and the entitlement commit
// together, and any failure rolls all three back for BTCPay to redeliver.
// Every transition is idempotent on (invoice id, fetched status).
//
// The row that decides a write is read again UNDER the lock, and the
// invoice is fetched after it (RP30-1): two overlapping deliveries of one
// invoice — two webhooks on the store, or one outliving BTCPay's 100 s —
// apply one after the other, the second seeing what the first wrote.
//
// BTCPay sends one delivery at a time per webhook, and its automatic
// redelivery waits (10 s, 1 min, 6 × 10 min) INSIDE that queue: a 5xx
// stalls every other invoice's events for up to ~71 min (RP30-4). So a 5xx
// is only for what a retry can fix — a database error, a Greenfield 5xx or
// no answer, the lock not free in time. A permanent Greenfield refusal (a
// 4xx), and every verdict that applies nothing (foreign, settled unpaid),
// is 200 with its own outcome, and its claim is NOT kept (RP30-3): BTCPay's
// redelivery of it — automatic or from the UI after the fix — carries the
// same originalDeliveryId and is handled fresh.

// webhookActor is the audit actor of what the webhook writes.
const webhookActor = "btcpay-webhook"

// ServeHTTP handles POST /webhook/btcpay.
func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	outcome := payments.WebhookError
	defer func() { payments.CountWebhook(payments.ProviderBTCPay, outcome) }()
	if p.cfg.WebhookSecret == "" || p.db == nil {
		p.logger.Error("btcpay webhook refused: no secret or database")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	body, err := ReadBody(w, r)
	if errors.Is(err, ErrBodyTooLarge) {
		outcome = payments.WebhookRejected
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		outcome = payments.WebhookRejected
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := VerifySig(p.cfg.WebhookSecret, r.Header.Get(SigHeader), body); err != nil {
		p.logger.Warn("btcpay webhook: signature refused", zap.Error(err))
		outcome = payments.WebhookRejected
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ev, err := ParseEvent(body)
	if err != nil {
		p.logger.Warn("btcpay webhook: undecodable delivery", zap.Error(err))
		outcome = payments.WebhookRejected
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if ev.StoreID != "" && ev.StoreID != p.cfg.StoreID {
		p.logger.Warn("btcpay webhook: delivery for another store — acknowledged, not applied",
			zap.String("delivery", ev.DeliveryID), zap.String("type", ev.Type))
		outcome = payments.WebhookForeign
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx := r.Context()
	claim, dup, err := payments.ClaimEvent(ctx, p.db, payments.ProviderBTCPay, ev.DedupKey(), ev.Type, body)
	if err != nil {
		p.logger.Error("btcpay webhook: claim failed", zap.String("delivery", ev.DeliveryID), zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if dup {
		outcome = payments.WebhookDuplicate
		w.WriteHeader(http.StatusOK)
		return
	}
	res, err := p.handle(ctx, claim.Tx(), ev)
	if err != nil {
		claim.Rollback()
		p.logger.Error("btcpay webhook: not applied — BTCPay will redeliver",
			zap.String("delivery", ev.DeliveryID), zap.String("type", ev.Type), zap.String("invoice", ev.InvoiceID), zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if res.verdict != "" {
		// Nothing applied: acknowledged, the claim released (RP30-3).
		claim.Rollback()
		outcome = res.verdict
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := claim.Commit(ctx, res.tenantID); err != nil {
		p.logger.Error("btcpay webhook: commit failed", zap.String("delivery", ev.DeliveryID), zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	res.after(ctx, p)
	outcome = payments.WebhookOK
	w.WriteHeader(http.StatusOK)
}

// applied is what a delivery did, for the work after the commit.
type applied struct {
	tenantID string
	// verdict is set when the delivery applied nothing (WebhookForeign,
	// WebhookSettledUnpaid, WebhookMarkRefused, WebhookFetchRefused): the
	// claim is rolled back and the delivery answered 200 under it.
	verdict string
	granted *payments.Granted
	settled *payments.Settled
	marked  string // the invoice marked Settled by the paid-late automation
	// switched: the grant took the house from another provider (WP-PAY-5).
	switched *switched
	credit   int64 // the change in the tenant's credit
	purpose  string
	invoice  string
}

// after counts, audits and mails what the committed delivery did.
func (a applied) after(ctx context.Context, p *Provider) {
	if a.settled != nil {
		outcome := payments.OutcomeSettled
		if !a.settled.Newly {
			outcome = payments.OutcomeReplayed
		}
		payments.CountPayment(payments.ProviderBTCPay, payments.KindCharge, outcome)
	}
	if a.credit > 0 {
		payments.CountPayment(payments.ProviderBTCPay, payments.KindCredit, payments.OutcomeSettled)
	}
	if a.granted != nil {
		payments.AuditGranted(ctx, p.db, a.tenantID, *a.granted, webhookActor)
	}
	if sw := a.switched; sw != nil {
		audit.Record(ctx, p.db, audit.Entry{TenantID: a.tenantID, EventType: "billing", Action: "billing.provider_switched",
			Resource: "tenant:" + a.tenantID, Metadata: map[string]any{"tenant": a.tenantID, "actor": webhookActor,
				"from": sw.from, "to": payments.ProviderBTCPay, "previous_ref": sw.ref, "invoice": a.invoice, "implicit": sw.implicit}})
		if sw.implicit {
			p.logger.Warn("btcpay: a payment settled over a house another provider renews by itself — implicit switch, its renewal is being stopped",
				zap.String("tenant", a.tenantID), zap.String("from", sw.from), zap.String("subscription", sw.ref), zap.String("invoice", a.invoice))
			if p.stopper != nil {
				// After the commit, bounded, best effort: the renewal_stops job
				// retries what this does not finish.
				sctx, cancel := payments.Detached(ctx)
				run, err := p.stopper.StopPendingRenewals(sctx, a.tenantID)
				cancel()
				if err != nil || run.Failed > 0 || run.Unconfigured > 0 {
					p.logger.Warn("btcpay: the other provider's renewal is not stopped yet — the renewal_stops job retries",
						zap.String("tenant", a.tenantID), zap.String("subscription", sw.ref), zap.Error(err), zap.Strings("errors", run.Errors))
				}
			}
		}
	}
	if a.marked != "" {
		audit.Record(ctx, p.db, audit.Entry{TenantID: a.tenantID, EventType: "billing", Action: "billing.invoice_marked",
			Resource: "btcpay-invoice:" + a.marked, Metadata: map[string]any{"tenant": a.tenantID, "actor": webhookActor,
				"invoice": a.marked, "status": StatusSettled, "reason": "paid_late_in_full"}})
	}
	if a.settled != nil && a.settled.DoublePeriod != nil {
		if err := payments.ReportDoublePeriod(ctx, p.db, p.receipts, a.tenantID, *a.settled); err != nil {
			p.logger.Warn("btcpay: double period not mailed to the operator", zap.String("tenant", a.tenantID), zap.Error(err))
		}
		p.logger.Error("btcpay: the customer paid twice for overlapping dates — granted; refund one by hand",
			zap.String("tenant", a.tenantID), zap.String("invoice", a.invoice),
			zap.String("other", a.settled.DoublePeriod.OtherProvider+":"+a.settled.DoublePeriod.OtherProviderID))
	}
	if a.settled != nil && a.settled.Newly {
		p.logger.Info("btcpay: payment recorded", zap.String("tenant", a.tenantID), zap.String("invoice", a.invoice),
			zap.String("invoice_number", a.settled.InvoiceNumber), zap.String("purpose", a.purpose))
		if a.settled.InvoiceNumber != "" && p.receipts != nil {
			mctx, cancel := payments.Detached(ctx)
			if err := p.receipts.SendReceipt(mctx, a.tenantID, a.settled.ID); err != nil {
				p.logger.Warn("btcpay: receipt not sent", zap.String("tenant", a.tenantID), zap.Error(err))
			}
			cancel()
		}
	}
}

// webhookLockWait bounds the wait for the tenant's entitlement lock (the
// claim's own lock_timeout is 5 s): a checkout holds it across
// CreateInvoice (≤ 10 s per Greenfield call), and a lock timeout is a 5xx
// that stalls the store's queue (RP30-4). Well inside BTCPay's 100 s.
const webhookLockWait = 15 * time.Second

// handle resolves and applies one claimed delivery in tx.
func (p *Provider) handle(ctx context.Context, tx *sql.Tx, ev Event) (applied, error) {
	var a applied
	switch ev.Type {
	case EventInvoiceReceivedPayment, EventInvoicePaymentSettled, EventInvoiceProcessing,
		EventInvoiceExpired, EventInvoiceSettled, EventInvoiceInvalid,
		EventInvoiceExpiredPaidPartial, EventInvoicePaidAfterExpiration:
	default:
		// InvoiceCreated (the row is already pending), InvoiceRefund (an
		// operator's refund, recorded by hand) and anything else.
		return a, nil
	}
	a.invoice = ev.InvoiceID
	// The tenant first, from our row — a plain read: a row's tenant never
	// changes; everything else is read again under the lock.
	row, err := payments.PendingOf(ctx, tx, payments.ProviderBTCPay, ev.InvoiceID)
	if errors.Is(err, payments.ErrNoSuchPayment) {
		// The invoice id may not be on its row yet (the crash window of
		// startCheckout): adopt the row the invoice's metadata names.
		inv, ferr := p.client.GetInvoice(ctx, ev.InvoiceID)
		if ferr != nil {
			return a, p.fetchFailed(&a, ev, "", ferr, "invoice unknown here and at BTCPay")
		}
		row, err = p.adopt(ctx, tx, inv)
		if err != nil && !errors.Is(err, payments.ErrNoSuchPayment) {
			return a, err
		}
	}
	if errors.Is(err, payments.ErrNoSuchPayment) || row.Kind != payments.KindCharge || row.TenantID == "" {
		p.logger.Warn("btcpay webhook: invoice our ledger does not know — acknowledged, not applied",
			zap.String("invoice", ev.InvoiceID), zap.String("type", ev.Type))
		a.verdict = payments.WebhookForeign
		return a, nil
	}
	if err != nil {
		return a, err
	}
	tenantID := row.TenantID

	// RP30-1: the lock, then the row as it is now, then BTCPay's invoice.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", webhookLockWait.Milliseconds())); err != nil {
		return a, fmt.Errorf("set lock_timeout: %w", err)
	}
	if err := payments.Lock(ctx, tx, tenantID); err != nil {
		return a, err
	}
	if row, err = payments.LockedPending(ctx, tx, payments.ProviderBTCPay, ev.InvoiceID); err != nil {
		return a, err
	}
	if row.TenantID != tenantID {
		return a, fmt.Errorf("invoice %s: the ledger row changed tenant under the lock", ev.InvoiceID)
	}
	inv, err := p.client.GetInvoice(ctx, ev.InvoiceID)
	if err != nil {
		return a, p.fetchFailed(&a, ev, tenantID, err, "our invoice is gone at BTCPay")
	}
	if mt := metadataTenant(inv.Metadata); mt != row.TenantID || inv.StoreID != "" && inv.StoreID != p.cfg.StoreID {
		p.logger.Warn("btcpay webhook: invoice metadata names another tenant than our row — acknowledged, not applied",
			zap.String("invoice", ev.InvoiceID), zap.String("row_tenant", row.TenantID), zap.String("metadata_tenant", mt))
		a.verdict = payments.WebhookForeign
		return a, nil
	}
	if cents, err := usdToCents(inv.Amount); err != nil || !strings.EqualFold(inv.Currency, "USD") || cents != row.AmountCents {
		p.logger.Error("btcpay webhook: invoice amount differs from our row — acknowledged, not applied",
			zap.String("invoice", ev.InvoiceID), zap.String("tenant", row.TenantID), zap.String("currency", inv.Currency),
			zap.Int64("row_cents", row.AmountCents))
		a.verdict = payments.WebhookForeign
		return a, nil
	}
	a.tenantID, a.purpose = row.TenantID, row.Purpose
	pms, err := p.client.GetPaymentMethods(ctx, ev.InvoiceID)
	if err != nil {
		return a, p.fetchFailed(&a, ev, tenantID, fmt.Errorf("fetch payment methods of %s: %w", ev.InvoiceID, err), "")
	}
	return a, p.apply(ctx, tx, &a, row, inv, pms)
}

// fetchFailed classifies a Greenfield read that failed: BTCPay's "no such
// invoice" (403 missing-permission — unknownWhat names the case) is a
// foreign verdict with a Warn; any other 4xx is permanent until an operator
// acts (a wrong key, a permission removed) — the fetch_refused verdict with
// an Error, so the store's queue is not stalled for it (RP30-4); a 5xx or no
// answer is returned, a retry can fix it.
func (p *Provider) fetchFailed(a *applied, ev Event, tenantID string, err error, unknownWhat string) error {
	if unknownWhat != "" && IsUnknownInvoice(err) {
		p.logger.Warn("btcpay webhook: "+unknownWhat+" — acknowledged, not applied (a redelivery is handled fresh)",
			zap.String("invoice", ev.InvoiceID), zap.String("type", ev.Type), zap.String("tenant", tenantID))
		a.verdict = payments.WebhookForeign
		return nil
	}
	if IsPermanent(err) {
		p.logger.Error("btcpay webhook: BTCPay refused a read — acknowledged, not applied; fix the store key and redeliver from the BTCPay UI",
			zap.String("invoice", ev.InvoiceID), zap.String("type", ev.Type), zap.String("tenant", tenantID), zap.Error(err))
		a.verdict = payments.WebhookFetchRefused
		return nil
	}
	return fmt.Errorf("fetch invoice %s: %w", ev.InvoiceID, err)
}

// adopt finds the pending row the invoice's metadata names (payment_id,
// the same tenant, still under its provisional id) and gives it the
// invoice id.
func (p *Provider) adopt(ctx context.Context, tx *sql.Tx, inv Invoice) (payments.Opened, error) {
	var m struct {
		PaymentID string `json:"payment_id"`
		TenantID  string `json:"tenant_id"`
	}
	if len(inv.Metadata) == 0 || json.Unmarshal(inv.Metadata, &m) != nil || m.PaymentID == "" {
		return payments.Opened{}, payments.ErrNoSuchPayment
	}
	row, err := payments.PendingOf(ctx, tx, payments.ProviderBTCPay, "pending:"+m.PaymentID)
	if err != nil {
		return row, err
	}
	if row.TenantID != m.TenantID {
		return payments.Opened{}, payments.ErrNoSuchPayment
	}
	if err := payments.AdoptPending(ctx, tx, payments.ProviderBTCPay, "pending:"+m.PaymentID, inv.ID); err != nil {
		return row, err
	}
	p.logger.Warn("btcpay webhook: adopted a pending row from the invoice metadata", zap.String("invoice", inv.ID),
		zap.String("tenant", row.TenantID))
	return row, nil
}

// btcMethod is the on-chain BTC payment method of an invoice (the launch
// set is BTC on-chain only — § 3.6), nil when absent.
func btcMethod(pms []PaymentMethod) *PaymentMethod {
	for i := range pms {
		id := pms[i].PaymentMethodID
		if id == "" {
			id = pms[i].PaymentMethod
		}
		if id == "BTC-CHAIN" || id == "BTC" || id == "BTC-OnChain" {
			return &pms[i]
		}
	}
	if len(pms) == 1 {
		return &pms[0]
	}
	return nil
}

// cryptoDetail is the ledger's view of the method's payments.
func cryptoDetail(pm *PaymentMethod) payments.CryptoDetail {
	if pm == nil {
		return payments.CryptoDetail{}
	}
	d := payments.CryptoDetail{Amount: pm.TotalPaid, Currency: pm.Currency, Rate: pm.Rate}
	if d.Currency == "" {
		d.Currency = "BTC"
	}
	seen := map[string]bool{}
	if pm.Destination != "" {
		d.Addresses = append(d.Addresses, pm.Destination)
		seen[pm.Destination] = true
	}
	for _, pay := range pm.Payments {
		// A payment id is "<txid>-<output index>" (BTCPay 2.4, the lab).
		if txid := txidOf(pay.ID); txid != "" && !seen["tx:"+txid] {
			d.Txids = append(d.Txids, txid)
			seen["tx:"+txid] = true
		}
		if pay.Destination != "" && !seen[pay.Destination] {
			d.Addresses = append(d.Addresses, pay.Destination)
			seen[pay.Destination] = true
		}
	}
	return d
}

// txidOf is the transaction id of a payment id ("<txid>-<vout>").
func txidOf(paymentID string) string {
	if i := strings.LastIndexByte(paymentID, '-'); i > 0 {
		return paymentID[:i]
	}
	return paymentID
}

// Credit row ids per invoice: what arrived on an invoice that did not
// settle (tracks the total; 0 once it settles), and what was paid over.
func partialCreditID(invoice string) string  { return "partial:" + invoice }
func overpaidCreditID(invoice string) string { return "overpaid:" + invoice }

// apply is § 3.3 on the fetched invoice. Money is read from the invoice's
// paidAmount — USD at the invoice rate (the lab: 15.00 on a 1.5× payment,
// 5.00 on a half) — never from the crypto due, which grows by a network
// fee once a first partial payment arrived (networkFeeMode
// MultiplePaymentsOnly), so a customer paying in two transactions is not
// held to more than the invoice.
func (p *Provider) apply(ctx context.Context, tx *sql.Tx, a *applied, row payments.Opened, inv Invoice, pms []PaymentMethod) error {
	now := p.now().UTC()
	pm := btcMethod(pms)
	paid, err := usdToCents(inv.PaidAmount)
	if err != nil {
		return fmt.Errorf("invoice %s: paidAmount: %w", inv.ID, err)
	}
	switch inv.Status {
	case StatusSettled:
		if row.Order == nil {
			return fmt.Errorf("invoice %s: the ledger row records no house", inv.ID)
		}
		if paid < row.AmountCents {
			// The lab: marking an UNPAID invoice Settled by hand sends
			// InvoiceSettled{manuallyMarked} with paidAmount 0. Nobody paid:
			// no grant, no receipt — an operator who means it records a
			// manual payment.
			p.logger.Error("btcpay webhook: invoice Settled without its amount paid (marked by hand?) — not granted",
				zap.String("invoice", inv.ID), zap.String("tenant", row.TenantID), zap.String("additional_status", inv.AdditionalStatus),
				zap.Int64("paid_cents", paid), zap.Int64("amount_cents", row.AmountCents))
			a.verdict = payments.WebhookSettledUnpaid
			return nil
		}
		// Granted only on a NEW settle: a replay (redelivery, a later
		// event of a settled invoice) changes nothing.
		var start, end time.Time
		var granted *payments.Granted
		if row.Status != payments.StatusSettled && row.Status != payments.StatusRefunded {
			s, e, g, err := p.grantSettled(ctx, tx, a, row, inv.ID)
			if err != nil {
				return err
			}
			start, end, granted = s, e, g
		}
		settled, err := payments.Settle(ctx, tx, payments.Payment{TenantID: row.TenantID, Provider: payments.ProviderBTCPay,
			ProviderID: inv.ID, Kind: payments.KindCharge, Purpose: row.Purpose, AmountCents: row.AmountCents,
			CreditCents: row.CreditCents, Order: row.Order, PeriodStart: start, PeriodEnd: end,
			// A $0 invoice never takes a receipt number (RP24-1's rule).
			Unnumbered: row.AmountCents == 0}, now)
		if err != nil {
			return err
		}
		a.settled, a.granted = &settled, granted
		if !settled.Newly {
			return nil
		}
		if err := payments.RecordCrypto(ctx, tx, payments.ProviderBTCPay, inv.ID, cryptoDetail(pm)); err != nil {
			return err
		}
		// Settled after all: what arrived is the payment, no longer a credit;
		// anything over the amount is (§ 3.4).
		if _, err := payments.SetCredit(ctx, tx, row.TenantID, payments.ProviderBTCPay, partialCreditID(inv.ID), 0, "", now); err != nil {
			return err
		}
		d, err := payments.SetCredit(ctx, tx, row.TenantID, payments.ProviderBTCPay, overpaidCreditID(inv.ID), paid-row.AmountCents,
			"overpaid invoice "+inv.ID, now)
		a.credit = d
		return err

	case StatusExpired:
		if !row.Open() {
			// A concurrent delivery settled (or an operator refunded) the row
			// before this one took the lock: an Expired fetch is not a reason
			// to mark anything or to credit money that paid it (RP30-1).
			p.logger.Info("btcpay webhook: invoice reads Expired but our row is no longer open — nothing to do",
				zap.String("invoice", inv.ID), zap.String("tenant", row.TenantID), zap.String("row_status", row.Status))
			return nil
		}
		// An expired invoice whose payments reached its amount was paid late
		// (§ 3.3; the lab: Expired/PaidLate — a full payment inside the
		// window makes it Processing): mark it Settled; BTCPay's
		// InvoiceSettled{manuallyMarked} that follows grants it.
		if paid > 0 && paid >= row.AmountCents {
			if _, err := p.client.MarkStatus(ctx, inv.ID, StatusSettled); err != nil {
				if IsPermanent(err) {
					// 422 (not in availableStatusesForManualMarking), 403 (the
					// key lacks canmodifyinvoices): no retry changes it.
					p.logger.Error("btcpay webhook: BTCPay refused to mark a late-paid invoice Settled — acknowledged, not granted; mark it by hand",
						zap.String("invoice", inv.ID), zap.String("tenant", row.TenantID), zap.Error(err))
					a.verdict = payments.WebhookMarkRefused
					return nil
				}
				return fmt.Errorf("mark late-paid invoice %s settled: %w", inv.ID, err)
			}
			a.marked = inv.ID
			return nil
		}
		if _, err := payments.Expire(ctx, tx, payments.ProviderBTCPay, inv.ID, paid > 0); err != nil {
			return err
		}
		// Never a partial grant (§ 3.4): what arrived is a credit (it tracks
		// a late partial payment too).
		d, err := payments.SetCredit(ctx, tx, row.TenantID, payments.ProviderBTCPay, partialCreditID(inv.ID), paid,
			"partial payment of expired invoice "+inv.ID, now)
		a.credit = d
		return err

	case StatusInvalid:
		ok, err := payments.Invalidate(ctx, tx, payments.ProviderBTCPay, inv.ID)
		if err == nil && !ok && (row.Status == payments.StatusSettled || row.Status == payments.StatusRefunded) {
			// The lab: a hand-marked Settled invoice can later be marked
			// Invalid. The grant stands — an operator adjusts it.
			p.logger.Warn("btcpay webhook: a settled invoice was marked Invalid — the grant stands; adjust the entitlement by hand if meant",
				zap.String("invoice", inv.ID), zap.String("tenant", row.TenantID))
		}
		return err

	case StatusProcessing:
		// Informational only: Processing is not monotonic (the lab: a double
		// spend before confirmation sends the invoice back to New, with no
		// event). Nothing is granted on it.
		_, err := payments.MarkProcessing(ctx, tx, payments.ProviderBTCPay, inv.ID)
		return err
	}
	// New (nothing paid, or a payment double-spent away): nothing.
	return nil
}

// grantSettled applies a newly settled invoice's order.
func (p *Provider) grantSettled(ctx context.Context, tx *sql.Tx, a *applied, row payments.Opened, invoiceID string) (time.Time, time.Time, *payments.Granted, error) {
	before, _ := payments.Get(ctx, tx, row.TenantID)
	start, end, sw, err := p.applyOrder(ctx, tx, row.TenantID, row.Purpose, *row.Order, invoiceID)
	if err != nil {
		return start, end, nil, err
	}
	a.switched = sw
	if row.Purpose == payments.PurposeResize {
		return start, end, nil, nil
	}
	after, err := payments.Get(ctx, tx, row.TenantID)
	if err != nil {
		return start, end, nil, err
	}
	g := &payments.Granted{Changed: true, PaidThrough: after.PaidThrough, Order: after.Order, Provider: after.Provider,
		ProviderRef: after.ProviderRef, Renewal: after.Renewal}
	if before.TenantID != "" {
		prev := before
		g.Previous = &prev
	}
	return start, end, g, nil
}
