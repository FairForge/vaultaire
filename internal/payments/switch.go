package payments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"go.uber.org/zap"
)

// Switching a customer between providers (WP-PAY-5, docs/PAYMENTS.md § 4).
// No gap, no overlap: a switch never shortens paid_through, a prepaid
// payment appends from it, and a renewing provider is told to stop
// (cancel_at_period_end) — never cancelled now. The one overlap left is a
// renewal that lands before the stop does; Settle detects it (the
// double-period detector, ledger.go) and the refund is by hand.

var (
	// ErrCryptoInvoiceOpen: a checkout with a provider that renews by itself
	// while a prepaid invoice of the tenant can still be paid (409
	// crypto_invoice_open — WP-PAY-5 item 0 c). errors.As an
	// *OpenInvoiceError for the invoice and its date.
	ErrCryptoInvoiceOpen = errors.New("crypto_invoice_open")
	// ErrNothingToSwitch: the house cannot be switched as asked (no granted
	// house, or the provider already bills it).
	ErrNothingToSwitch = errors.New("nothing_to_switch")
	// ErrRefGone: the provider no longer has the reference (or it is
	// cancelled) — a renewal stop of it is done.
	ErrRefGone = errors.New("provider reference gone")
)

// PrepaidInvoiceWindow is how long after it was opened a prepaid invoice
// may still settle: BTCPay's monitoringMinutes (1440 — a payment
// broadcast before the 30-minute expiry is watched for a day).
const PrepaidInvoiceWindow = 24 * time.Hour

// OpenInvoiceError names the open invoice that refused a checkout.
type OpenInvoiceError struct {
	Provider, Invoice string
	// Until: after it the invoice can no longer settle.
	Until time.Time
	// Paid: the provider says the invoice holds money, so the customer
	// cannot cancel it (RP31-5) — errors.Is ErrInvoicePaid too.
	Paid bool
}

func (e *OpenInvoiceError) Error() string {
	return fmt.Sprintf("%s invoice %s can be paid until %s: %v", e.Provider, e.Invoice, e.Until.UTC().Format(time.RFC3339), ErrCryptoInvoiceOpen)
}

// Is makes errors.Is(err, ErrCryptoInvoiceOpen) true.
func (e *OpenInvoiceError) Is(target error) bool {
	return target == ErrCryptoInvoiceOpen || e.Paid && target == ErrInvoicePaid
}

// OpenInvoiceOf is the tenant's open charge of provider by its id as the
// guard sees it (its Until), ErrNoSuchPayment when the tenant has none.
func OpenInvoiceOf(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, tenantID, provider, invoiceID string) (*OpenInvoiceError, error) {
	var created time.Time
	err := q.QueryRowContext(ctx, `
		SELECT created_at FROM payments
		 WHERE tenant_id = $1 AND provider = $2 AND provider_id = $3 AND kind = 'charge'
		   AND status IN ('pending', 'processing', 'expired', 'expired_partial')`, tenantID, provider, invoiceID).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("invoice %s of tenant %s: %w", invoiceID, tenantID, ErrNoSuchPayment)
	}
	if err != nil {
		return nil, fmt.Errorf("read invoice %s of tenant %s: %w", invoiceID, tenantID, err)
	}
	return &OpenInvoiceError{Provider: provider, Invoice: invoiceID, Until: created.Add(PrepaidInvoiceWindow).UTC()}, nil
}

// openPrepaidInvoice is the tenant's newest charge of another provider than
// `except` that can still settle (pending, processing, or expired inside
// PrepaidInvoiceWindow — a late payment is still accepted), nil when none.
func openPrepaidInvoice(ctx context.Context, tx *sql.Tx, tenantID, except string) (*OpenInvoiceError, error) {
	var e OpenInvoiceError
	var created time.Time
	err := tx.QueryRowContext(ctx, `
		SELECT provider, provider_id, created_at FROM payments
		 WHERE tenant_id = $1 AND kind = 'charge' AND provider <> $2
		   AND status IN ('pending', 'processing', 'expired', 'expired_partial')
		   AND created_at > NOW() - $3 * INTERVAL '1 second'
		 ORDER BY created_at DESC LIMIT 1`, tenantID, except, PrepaidInvoiceWindow.Seconds()).Scan(&e.Provider, &e.Invoice, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read open invoices of tenant %s: %w", tenantID, err)
	}
	e.Until = created.Add(PrepaidInvoiceWindow).UTC()
	return &e, nil
}

// SwitchToPrepaid is "Switch to <to> at renewal" (docs/PAYMENTS.md § 4.1
// step 1): under the tenant's entitlement lock the provider that renews the
// house is told to stop (Stripe: cancel_at_period_end — the house runs to
// its paid date), the row becomes renewal 'prepaid' with pending_provider =
// to, and paid_through is not touched. `to` must be a prepaid provider the
// tenant may use now (flag on, ready). A house already prepaid only gets
// its pending provider (no provider call). Audited after the commit:
// billing.provider_switch_requested (+ billing.renewal_stopped when the
// renewal was stopped here).
func SwitchToPrepaid(ctx context.Context, r *Registry, tenantID, to, actor string) (Entitlement, error) {
	target, err := r.For(ctx, tenantID, to)
	if err != nil {
		return Entitlement{}, err
	}
	if target.Renewal() != RenewalPrepaid {
		return Entitlement{}, fmt.Errorf("%s renews by itself: %w", to, ErrNothingToSwitch)
	}
	if r.db == nil {
		return Entitlement{}, errors.New("switch: no database")
	}
	var e Entitlement
	var stopped bool
	err = InTx(ctx, r.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", checkoutLockTimeout.Milliseconds())); err != nil {
			return fmt.Errorf("set lock_timeout: %w", err)
		}
		if err := Lock(ctx, tx, tenantID); err != nil {
			if lockTimedOut(err) {
				return fmt.Errorf("tenant %s: the entitlement lock was not free within %s: %w", tenantID, checkoutLockTimeout, ErrProviderUnavailable)
			}
			return err
		}
		var err error
		if e, err = Get(ctx, tx, tenantID); errors.Is(err, ErrNoEntitlement) {
			return fmt.Errorf("tenant %s has no house: %w", tenantID, ErrNothingToSwitch)
		} else if err != nil {
			return err
		}
		if e.LapsedAt != nil || e.Provider == to {
			return fmt.Errorf("tenant %s: house lapsed or already paid with %s: %w", tenantID, to, ErrNothingToSwitch)
		}
		if e.Renewal == RenewalAuto {
			cur := r.Provider(e.Provider)
			if cur == nil {
				return fmt.Errorf("%w: %q renews the house", ErrUnknownProvider, e.Provider)
			}
			if err := cur.StopRenewal(ctx, tenantID); err != nil {
				return fmt.Errorf("stop the renewal with %s: %w", e.Provider, err)
			}
			stopped = true
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE entitlements SET renewal = 'prepaid', pending_provider = $2, updated_at = NOW() WHERE tenant_id = $1`,
			tenantID, to); err != nil {
			return fmt.Errorf("switch tenant %s to %s: %w", tenantID, to, err)
		}
		e.Renewal = RenewalPrepaid
		return nil
	})
	if err != nil {
		return Entitlement{}, err
	}
	audit.Record(ctx, r.db, audit.Entry{Actor: actor, TenantID: tenantID, EventType: "billing", Action: "billing.provider_switch_requested",
		Resource: "tenant:" + tenantID, Metadata: map[string]any{"tenant": tenantID, "actor": actor, "from": e.Provider, "to": to,
			"paid_through": e.PaidThrough.UTC().Format(time.RFC3339), "renewal_stopped": stopped}})
	if stopped {
		AuditRenewal(ctx, r.db, tenantID, RenewalPrepaid, actor, e.Provider, e.ProviderRef)
	}
	return e, nil
}

// SwitchToPrepaid is the package function on the registry (the dashboard's
// door).
func (r *Registry) SwitchToPrepaid(ctx context.Context, tenantID, to, actor string) (Entitlement, error) {
	return SwitchToPrepaid(ctx, r, tenantID, to, actor)
}

// renewalStopBatch bounds one pass of StopPendingRenewals.
const renewalStopBatch = 100

// StopPendingRenewals asks each provider to stop the renewal of every
// pending renewal_stop row (the tenant's, or every tenant's with "") —
// after the commit of the BTCPay delivery that wrote one, and every 10
// minutes from the renewal_stops job. A provider that confirms (or no
// longer has the reference) closes the row (stopped / gone, audited
// billing.renewal_stopped); one that fails or is not configured leaves it
// pending with the error for the next pass. Never cancels a period.
func (r *Registry) StopPendingRenewals(ctx context.Context, tenantID string) (StopRun, error) {
	var run StopRun
	if r == nil || r.db == nil {
		return run, nil
	}
	rows, err := PendingRenewalStops(ctx, r.db, tenantID, renewalStopBatch)
	if err != nil {
		return run, err
	}
	for _, s := range rows {
		var cerr error
		p := r.Provider(s.Provider)
		rs, ok := p.(RefRenewalStopper)
		switch {
		case p == nil || !ok:
			cerr = ErrProviderUnconfigured
		default:
			cerr = rs.StopRenewalRef(ctx, s.TenantID, s.Ref)
		}
		status, last := RenewalStopStopped, ""
		switch {
		case cerr == nil:
			run.Stopped++
		case errors.Is(cerr, ErrRefGone):
			status = RenewalStopGone
			run.Stopped++
		case errors.Is(cerr, ErrProviderUnconfigured):
			status, last = RenewalStopPending, cerr.Error()
			run.Unconfigured++
			run.Errors = append(run.Errors, s.Provider+":"+s.Ref+": not configured")
		default:
			status, last = RenewalStopPending, cerr.Error()
			run.Failed++
			run.Errors = append(run.Errors, s.Provider+":"+s.Ref+": "+cerr.Error())
		}
		if err := MarkRenewalStop(ctx, r.db, s.Provider, s.Ref, status, last); err != nil {
			return run, err
		}
		if status != RenewalStopPending {
			AuditRenewal(ctx, r.db, s.TenantID, RenewalPrepaid, "renewal-stops", s.Provider, s.Ref)
			r.logger.Info("payments: renewal stopped for a subscription the house no longer names",
				zap.String("tenant", s.TenantID), zap.String("provider", s.Provider), zap.String("ref", s.Ref), zap.String("status", status))
		}
	}
	return run, nil
}
