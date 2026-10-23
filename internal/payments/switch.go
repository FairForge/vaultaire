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
// step 1): the row becomes renewal 'prepaid' with pending_provider = to
// (paid_through not touched) under the tenant's entitlement lock, and
// AFTER that commit the provider that renewed the house is told to stop
// (Stripe, by the subscription's id — StopRenewalRef: cancel_at_period_end
// on a paid period, the house runs to its paid date; cancelled now when the
// period is in dunning — past_due / unpaid / incomplete, RP33-7 = RP31-4's
// path). `to` must be a prepaid provider the tenant may use now (flag on,
// ready). A house already prepaid only gets its pending provider (no
// provider call).
//
// The stop is DURABLE (RP33-3, RP33-8a): its intent is a pending
// renewal_stop row written in the switch's own transaction, so a switch
// that committed always carries the stop it owes. The provider call is
// outside the transaction (RP31-6: a commit that failed after Stripe had
// stopped left a house that renews in our record and runs out at Stripe)
// and detached from the request (Detached: a client that leaves cannot
// cancel it; 15 s). Its outcome:
//   - stopped (or the reference gone) — the row is closed, audited
//     billing.renewal_stopped;
//   - a PERMANENT refusal (ErrProviderRefused: a Stripe 4xx) — the switch
//     is put back (revertSwitch: a second short transaction under the
//     lock, only if the row is still what the switch wrote — a webhook
//     that changed it meanwhile stands — and the pending stop with it) and
//     the error is the caller's;
//   - anything else (a 5xx, no answer, the deadline, the process killed)
//     — the switch stands and the row stays pending: the renewal_stops job
//     (10 min) finishes the stop.
//
// A provider without RefRenewalStopper, or a house with no reference, has
// no durable row: StopRenewal(tenant), and any failure reverts. Audited
// after the stop: billing.provider_switch_requested.
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
	var cur Provider
	var byRef RefRenewalStopper
	err = InTx(ctx, r.db, func(tx *sql.Tx) error {
		if err := lockWithin(ctx, tx, tenantID); err != nil {
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
			if cur = r.Provider(e.Provider); cur == nil {
				return fmt.Errorf("%w: %q renews the house", ErrUnknownProvider, e.Provider)
			}
			if rs, ok := cur.(RefRenewalStopper); ok && e.ProviderRef != "" {
				byRef = rs
				if err := RecordRenewalStop(ctx, tx, tenantID, e.Provider, e.ProviderRef, ReasonSwitch, RenewalStopPending,
					map[string]any{"switch_to": to, "paid_through_before": e.PaidThrough.UTC().Format(time.RFC3339)}); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE entitlements SET renewal = 'prepaid', pending_provider = $2, updated_at = NOW() WHERE tenant_id = $1`,
			tenantID, to); err != nil {
			return fmt.Errorf("switch tenant %s to %s: %w", tenantID, to, err)
		}
		return nil
	})
	if err != nil {
		return Entitlement{}, err
	}
	stop := "none"
	if cur != nil {
		sctx, cancel := Detached(ctx)
		var serr error
		if byRef != nil {
			serr = byRef.StopRenewalRef(sctx, tenantID, e.ProviderRef)
		} else {
			serr = cur.StopRenewal(sctx, tenantID)
		}
		switch {
		case serr == nil || byRef != nil && errors.Is(serr, ErrRefGone):
			stop = RenewalStopStopped
			if byRef != nil {
				if errors.Is(serr, ErrRefGone) {
					stop = RenewalStopGone
				}
				if err := MarkRenewalStop(sctx, r.db, e.Provider, e.ProviderRef, stop, ""); err != nil {
					// The row stays pending: the job asks again, and a stopped
					// reference answers at once.
					r.logger.Warn("payments: renewal stopped but not recorded — the renewal_stops job closes it",
						zap.String("tenant", tenantID), zap.String("ref", e.ProviderRef), zap.Error(err))
				}
			}
		case byRef != nil && !errors.Is(serr, ErrProviderRefused):
			stop = RenewalStopPending
			if err := MarkRenewalStop(sctx, r.db, e.Provider, e.ProviderRef, RenewalStopPending, serr.Error()); err != nil {
				r.logger.Warn("payments: renewal stop outcome not recorded", zap.String("ref", e.ProviderRef), zap.Error(err))
			}
			r.logger.Warn("payments: switch recorded, the provider's renewal is not stopped yet — the renewal_stops job finishes it",
				zap.String("tenant", tenantID), zap.String("provider", e.Provider), zap.String("ref", e.ProviderRef), zap.Error(serr))
		default:
			if rerr := revertSwitch(sctx, r.db, tenantID, to, e, byRef != nil); rerr != nil {
				r.logger.Error("payments: a switch whose renewal stop was refused could not be put back — the house shows prepaid while the provider still renews it; fix the row by hand",
					zap.String("tenant", tenantID), zap.String("provider", e.Provider), zap.String("to", to), zap.Error(rerr))
			}
			cancel()
			return Entitlement{}, fmt.Errorf("stop the renewal with %s: %w", e.Provider, serr)
		}
		cancel()
	}
	e.Renewal = RenewalPrepaid
	actx, cancel := Detached(ctx)
	defer cancel()
	audit.Record(actx, r.db, audit.Entry{Actor: actor, TenantID: tenantID, EventType: "billing", Action: "billing.provider_switch_requested",
		Resource: "tenant:" + tenantID, Metadata: map[string]any{"tenant": tenantID, "actor": actor, "from": e.Provider, "to": to,
			"paid_through": e.PaidThrough.UTC().Format(time.RFC3339), "renewal_stopped": stop == RenewalStopStopped || stop == RenewalStopGone,
			"renewal_stop": stop}})
	if stop == RenewalStopStopped || stop == RenewalStopGone {
		AuditRenewal(actx, r.db, tenantID, RenewalPrepaid, actor, e.Provider, e.ProviderRef)
	}
	return e, nil
}

// lockWithin takes the tenant's entitlement lock waiting at most
// checkoutLockTimeout (55P03 = ErrProviderUnavailable).
func lockWithin(ctx context.Context, tx *sql.Tx, tenantID string) error {
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", checkoutLockTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("set lock_timeout: %w", err)
	}
	if err := Lock(ctx, tx, tenantID); err != nil {
		if lockTimedOut(err) {
			return fmt.Errorf("tenant %s: the entitlement lock was not free within %s: %w", tenantID, checkoutLockTimeout, ErrProviderUnavailable)
		}
		return err
	}
	return nil
}

// revertSwitch puts back a switch whose provider stop was refused: renewal
// 'auto', no pending provider — only while the row is still what
// SwitchToPrepaid wrote over `was` (same provider and reference, prepaid,
// pending `to`) — and, with withStop, its pending renewal_stop row in the
// same transaction (the job must not stop a house that renews again). A
// row a webhook changed meanwhile keeps its stop.
func revertSwitch(ctx context.Context, db *sql.DB, tenantID, to string, was Entitlement, withStop bool) error {
	return InTx(ctx, db, func(tx *sql.Tx) error {
		if err := lockWithin(ctx, tx, tenantID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE entitlements SET renewal = $5, pending_provider = NULL, updated_at = NOW()
			 WHERE tenant_id = $1 AND renewal = 'prepaid' AND pending_provider = $2 AND provider = $3 AND COALESCE(provider_ref, '') = $4`,
			tenantID, to, was.Provider, was.ProviderRef, string(was.Renewal))
		if err != nil {
			return fmt.Errorf("revert the switch of tenant %s: %w", tenantID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 || !withStop {
			return nil
		}
		return WithdrawRenewalStop(ctx, tx, tenantID, was.Provider, was.ProviderRef)
	})
}

// SwitchToPrepaid is the package function on the registry (the dashboard's
// door).
func (r *Registry) SwitchToPrepaid(ctx context.Context, tenantID, to, actor string) (Entitlement, error) {
	return SwitchToPrepaid(ctx, r, tenantID, to, actor)
}

// renewalStopBatch bounds one pass of StopPendingRenewals.
const renewalStopBatch = 100

// stopCallTimeout bounds one provider call of StopPendingRenewals — and so
// how long it holds the tenant's entitlement lock (Detached's bound).
const stopCallTimeout = 15 * time.Second

// StopPendingRenewals asks each provider to stop the renewal of every
// pending renewal_stop row (the tenant's, or every tenant's with "") —
// after the commit of the BTCPay delivery that wrote one, and every 10
// minutes from the renewal_stops job. A provider that confirms (or no
// longer has the reference) closes the row (stopped / gone, audited
// billing.renewal_stopped); one that fails or is not configured leaves it
// pending with the error for the next pass. Never cancels a period.
//
// Each row acts on fresh truth (RP37-5): under the tenant's entitlement
// lock, held across the provider call (bounded by stopCallTimeout, 15 s), the row
// and the entitlement are read again — a row withdrawn or closed since the
// batch was listed (a portal un-cancel, a refused-stop revert, another
// pass), or one whose subscription the house renews by again, is skipped
// with no provider call (SkippedWithdrawn); every writer that withdraws a
// stop takes the same lock, so none can slip between the read and the
// call. A PERMANENT refusal (ErrProviderRefused: a 401 after a key change)
// of a customer's switch reverts it exactly as the request path does
// (revertSwitch: only while the row is still what the switch wrote — it
// withdraws the stop with it), with an Error line; any other row stays
// pending with the error.
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
		if err := r.stopOne(ctx, s, &run); err != nil {
			return run, err
		}
	}
	return run, nil
}

// stopOne is one row of StopPendingRenewals.
func (r *Registry) stopOne(ctx context.Context, s RenewalStop, run *StopRun) error {
	var (
		cerr    error
		skipped bool
		revert  string // the pending provider of a refused switch, "" none
	)
	status, last := RenewalStopStopped, ""
	err := InTx(ctx, r.db, func(tx *sql.Tx) error {
		if err := Lock(ctx, tx, s.TenantID); err != nil {
			return err
		}
		cur, found, err := RenewalStopOf(ctx, tx, s.TenantID, s.Provider, s.Ref)
		if err != nil {
			return err
		}
		e, err := Get(ctx, tx, s.TenantID)
		if err != nil && !errors.Is(err, ErrNoEntitlement) {
			return err
		}
		renewsAgain := err == nil && e.LapsedAt == nil && e.Provider == s.Provider && e.ProviderRef == s.Ref && e.Renewal == RenewalAuto
		if !found || cur.Status != RenewalStopPending || renewsAgain {
			skipped = true
			return nil
		}
		p := r.Provider(s.Provider)
		if rs, ok := p.(RefRenewalStopper); p != nil && ok {
			cctx, cancel := context.WithTimeout(ctx, stopCallTimeout)
			cerr = rs.StopRenewalRef(cctx, s.TenantID, s.Ref)
			cancel()
		} else {
			cerr = ErrProviderUnconfigured
		}
		switch {
		case cerr == nil:
		case errors.Is(cerr, ErrRefGone):
			status = RenewalStopGone
		default:
			status, last = RenewalStopPending, cerr.Error()
			if errors.Is(cerr, ErrProviderRefused) && cur.Reason == ReasonSwitch && err == nil &&
				e.Provider == s.Provider && e.ProviderRef == s.Ref && e.Renewal == RenewalPrepaid && e.PendingProvider != "" {
				revert = e.PendingProvider
			}
		}
		return MarkRenewalStop(ctx, tx, s.Provider, s.Ref, status, last)
	})
	if err != nil {
		return err
	}
	switch {
	case skipped:
		run.SkippedWithdrawn++
		r.logger.Info("payments: renewal stop no longer owed when read again — no provider call",
			zap.String("tenant", s.TenantID), zap.String("provider", s.Provider), zap.String("ref", s.Ref))
		return nil
	case cerr == nil || errors.Is(cerr, ErrRefGone):
		run.Stopped++
		AuditRenewal(ctx, r.db, s.TenantID, RenewalPrepaid, "renewal-stops", s.Provider, s.Ref)
		r.logger.Info("payments: renewal stopped for a subscription the house no longer names",
			zap.String("tenant", s.TenantID), zap.String("provider", s.Provider), zap.String("ref", s.Ref), zap.String("status", status))
		return nil
	case errors.Is(cerr, ErrProviderUnconfigured):
		run.Unconfigured++
		run.Errors = append(run.Errors, s.Provider+":"+s.Ref+": not configured")
		return nil
	}
	run.Failed++
	run.Errors = append(run.Errors, s.Provider+":"+s.Ref+": "+cerr.Error())
	if revert == "" {
		return nil
	}
	// The request path's revert (SwitchToPrepaid's default case): the
	// provider refuses for good, so the switch cannot finish — put it back
	// while the row is still what the switch wrote, and its stop with it.
	was := Entitlement{TenantID: s.TenantID, Provider: s.Provider, ProviderRef: s.Ref, Renewal: RenewalAuto}
	if rerr := revertSwitch(ctx, r.db, s.TenantID, revert, was, true); rerr != nil {
		r.logger.Error("payments: a switch whose renewal stop was refused for good could not be put back — the house shows prepaid while the provider still renews it; fix the row by hand",
			zap.String("tenant", s.TenantID), zap.String("provider", s.Provider), zap.String("ref", s.Ref), zap.Error(rerr))
		return nil
	}
	run.Reverted++
	r.logger.Error("payments: the provider refused the renewal stop for good — the switch is put back, the house renews with it again",
		zap.String("tenant", s.TenantID), zap.String("provider", s.Provider), zap.String("ref", s.Ref),
		zap.String("switch_to", revert), zap.Error(cerr))
	return nil
}
