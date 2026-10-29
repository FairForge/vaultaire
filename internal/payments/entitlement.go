// Package payments is the provider-neutral core of billing (WP-PAY,
// docs/PAYMENTS.md): what a tenant has bought lives in our database with a
// date — the entitlement — and a payment from any provider extends the
// date. The quota tables (tenant_quotas, tenant_floor_quotas) stay the
// enforcement truth the write path reads; this package is the one writer
// that keeps the two together: every function that changes a house takes
// the caller's transaction and writes the entitlement and the floors in it.
//
// Lock order: the tenant's entitlement advisory lock (Lock) → tenant_quotas
// → tenant_floor_quotas (usage.SetHouseTx / ClearHouseTx take the last two
// in that order, ledger_tx.go). Nothing that holds a key, head-row or
// upload lock takes the entitlement lock, so it cannot cycle with a write.
package payments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/usage"
)

// Period is a house's billing interval.
type Period string

const (
	PeriodAnnual  Period = "annual"
	PeriodMonthly Period = "monthly"
)

// Valid reports a known period.
func (p Period) Valid() bool { return p == PeriodAnnual || p == PeriodMonthly }

// interval is the Postgres interval one period adds.
func (p Period) interval() string {
	if p == PeriodAnnual {
		return "1 year"
	}
	return "1 month"
}

// Renewal says whether a provider will try to extend the date on its own.
type Renewal string

const (
	// RenewalAuto: a provider extends it (a live Stripe subscription).
	RenewalAuto Renewal = "auto"
	// RenewalPrepaid: nobody will (crypto, manual, a cancelled subscription).
	RenewalPrepaid Renewal = "prepaid"
)

// Providers (the CHECK of migration 097).
const (
	ProviderStripe = "stripe"
	ProviderBTCPay = "btcpay"
	ProviderManual = "manual"
)

// Order is a house: whole TB per line and a period (billing.HouseOrder is
// an alias of it).
type Order struct {
	Std, Vault, PinHot int
	Period             Period
}

// House is the order in bytes, as the quota tables hold it.
func (o Order) House() usage.House { return usage.HouseFromTB(o.Std, o.Vault, o.PinHot) }

// Words says the plan the way the site does: "5 TB Standard, 2 TB Vault,
// 1 TB pinned hot" (one box is one terabyte since 2026-10-29).
func (o Order) Words() string {
	var parts []string
	if o.Std > 0 {
		parts = append(parts, fmt.Sprintf("%d TB Standard", o.Std))
	}
	if o.Vault > 0 {
		parts = append(parts, fmt.Sprintf("%d TB Vault", o.Vault))
	}
	if o.PinHot > 0 {
		parts = append(parts, fmt.Sprintf("%d TB pinned hot", o.PinHot))
	}
	return strings.Join(parts, ", ")
}

// PeriodWord is "yearly" or "monthly".
func (p Period) Word() string {
	if p == PeriodMonthly {
		return "monthly"
	}
	return "yearly"
}

// ErrInvalidOrder is a house no grant may record: an unknown period, a
// negative line, or more pin-hot than Standard (migration 097's CHECK
// pin_hot_tb <= std_tb is the same rule). It is the order's fault, never a
// transient one — a webhook answers it as handled, not as a retry (RP20-4).
var ErrInvalidOrder = errors.New("invalid house order")

// Validate reports an order no grant may record (ErrInvalidOrder).
func (o Order) Validate() error {
	if !o.Period.Valid() {
		return fmt.Errorf("%w: unknown billing period %q", ErrInvalidOrder, o.Period)
	}
	if o.Std < 0 || o.Vault < 0 || o.PinHot < 0 || o.PinHot > o.Std {
		return fmt.Errorf("%w: %d/%d/%d TB (std/vault/pin-hot)", ErrInvalidOrder, o.Std, o.Vault, o.PinHot)
	}
	return nil
}

// Entitlement is one entitlements row.
type Entitlement struct {
	TenantID    string
	Order       Order
	PaidThrough time.Time
	Renewal     Renewal
	Provider    string
	ProviderRef string
	// PendingProvider is the provider the customer asked to switch to at
	// renewal (WP-PAY-5: SwitchToPrepaid), "" when none.
	PendingProvider string
	// LapsedAt is set when the house was cleared (nil = granted).
	LapsedAt  *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ErrNoEntitlement is an operation on a tenant with no entitlement row.
var ErrNoEntitlement = errors.New("no entitlement")

// Lock takes the tenant's entitlement lock for the rest of tx: every grant,
// extension, renewal change, clear and (WP-PAY-2) checkout start of one
// tenant runs one at a time.
func Lock(ctx context.Context, tx *sql.Tx, tenantID string) error {
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('entitlement:' || $1, 0))`, tenantID); err != nil {
		return fmt.Errorf("lock entitlement of tenant %s: %w", tenantID, err)
	}
	return nil
}

// Grant records that the tenant holds house o paid through paidThrough,
// extendable by provider (providerRef: the Stripe subscription, the last
// settled invoice, a ticket), and writes the floors in the same
// transaction. Idempotent: a replay with the same values changes no row.
//
// A grant never shortens a running date: paid_through becomes the later of
// the row's and paidThrough (a stale or replayed event cannot take paid
// time away — only an operator refund shortens a date, WP-PAY-7). A lapsed
// row is granted again (lapsed_at cleared) at the given date.
func Grant(ctx context.Context, tx *sql.Tx, tenantID string, o Order, paidThrough time.Time,
	provider, providerRef string, renewal Renewal) error {
	_, err := GrantRecorded(ctx, tx, tenantID, o, paidThrough, provider, providerRef, renewal)
	return err
}

// Granted is what a grant did (WP-PAY-3 item 5): Changed is false for a
// replay that touched no row — the caller writes billing.house_granted
// only when it is true (AuditGranted). Previous is the row before the
// grant (nil when there was none), so a caller can tell a renewal change.
type Granted struct {
	Changed     bool
	PaidThrough time.Time
	Order       Order
	Provider    string
	ProviderRef string
	Renewal     Renewal
	Previous    *Entitlement
}

// GrantRecorded is Grant reporting whether the row changed (the upsert's
// RETURNING: its WHERE excludes a replay, so a replay returns no row).
func GrantRecorded(ctx context.Context, tx *sql.Tx, tenantID string, o Order, paidThrough time.Time,
	provider, providerRef string, renewal Renewal) (Granted, error) {
	g := Granted{Order: o, Provider: provider, ProviderRef: providerRef, Renewal: renewal, PaidThrough: paidThrough.UTC()}
	if err := o.Validate(); err != nil {
		return g, fmt.Errorf("grant tenant %s: %w", tenantID, err)
	}
	if err := Lock(ctx, tx, tenantID); err != nil {
		return g, err
	}
	prev, err := Get(ctx, tx, tenantID)
	switch {
	case errors.Is(err, ErrNoEntitlement):
	case err != nil:
		return g, err
	default:
		g.Previous = &prev
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO entitlements (tenant_id, std_tb, vault_tb, pin_hot_tb, period, paid_through, renewal, provider, provider_ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''))
		ON CONFLICT (tenant_id) DO UPDATE
		   SET std_tb = EXCLUDED.std_tb, vault_tb = EXCLUDED.vault_tb, pin_hot_tb = EXCLUDED.pin_hot_tb,
		       period = EXCLUDED.period,
		       paid_through = CASE WHEN entitlements.lapsed_at IS NULL
		                           THEN GREATEST(entitlements.paid_through, EXCLUDED.paid_through)
		                           ELSE EXCLUDED.paid_through END,
		       renewal = EXCLUDED.renewal, provider = EXCLUDED.provider, provider_ref = EXCLUDED.provider_ref,
		       lapsed_at = NULL, updated_at = NOW()
		 WHERE NOT (entitlements.std_tb = EXCLUDED.std_tb AND entitlements.vault_tb = EXCLUDED.vault_tb
		        AND entitlements.pin_hot_tb = EXCLUDED.pin_hot_tb AND entitlements.period = EXCLUDED.period
		        AND entitlements.renewal = EXCLUDED.renewal AND entitlements.provider = EXCLUDED.provider
		        AND entitlements.provider_ref IS NOT DISTINCT FROM EXCLUDED.provider_ref
		        AND entitlements.lapsed_at IS NULL
		        AND entitlements.paid_through >= EXCLUDED.paid_through)
		RETURNING paid_through`,
		tenantID, o.Std, o.Vault, o.PinHot, string(o.Period), paidThrough.UTC(), string(renewal), provider, providerRef).Scan(&g.PaidThrough)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		g.Changed = false
		if g.Previous != nil {
			g.PaidThrough = g.Previous.PaidThrough
		}
	case err != nil:
		return g, fmt.Errorf("grant tenant %s: %w", tenantID, err)
	default:
		g.Changed = true
	}
	if err := usage.SetHouseTx(ctx, tx, tenantID, o.House()); err != nil {
		return g, err
	}
	return g, nil
}

// AuditGranted writes billing.house_granted for a grant that changed the
// row (nothing for a replay). Called AFTER the caller's commit; actor is
// who granted ("stripe-webhook", an admin's user id).
func AuditGranted(ctx context.Context, db *sql.DB, tenantID string, g Granted, actor string) {
	if !g.Changed {
		return
	}
	// performed_by is a users.id (audit.nullUUID): a named actor such as
	// the webhook lives in the metadata, an admin's id in both.
	meta := map[string]any{"tenant": tenantID, "actor": actor, "provider": g.Provider, "renewal": string(g.Renewal),
		"paid_through": g.PaidThrough.UTC().Format(time.RFC3339), "std": g.Order.Std, "vault": g.Order.Vault,
		"pin_hot": g.Order.PinHot, "period": string(g.Order.Period)}
	if g.ProviderRef != "" {
		meta["provider_ref"] = g.ProviderRef
	}
	if g.Previous != nil {
		meta["previous_paid_through"] = g.Previous.PaidThrough.UTC().Format(time.RFC3339)
	}
	audit.Record(ctx, db, audit.Entry{Actor: actor, TenantID: tenantID, EventType: "billing", Action: "billing.house_granted",
		Resource: "tenant:" + tenantID, Metadata: meta})
}

// Extend adds one period to the tenant's date: from paid_through when the
// house is running, from now when it has passed (never two overlapping
// periods), and grants a lapsed house again. Month arithmetic is
// Postgres's, in UTC: '2026-01-31' + 1 month = '2026-02-28' (accepted — the
// next month has no 31st; the date never moves back to the 31st after).
func Extend(ctx context.Context, tx *sql.Tx, tenantID string, by Period) (time.Time, error) {
	if !by.Valid() {
		return time.Time{}, fmt.Errorf("extend tenant %s: unknown period %q", tenantID, by)
	}
	if err := Lock(ctx, tx, tenantID); err != nil {
		return time.Time{}, err
	}
	var e Entitlement
	var period, renewal string
	err := tx.QueryRowContext(ctx, `
		UPDATE entitlements
		   SET paid_through = ((GREATEST(paid_through, NOW()) AT TIME ZONE 'UTC') + $2::interval) AT TIME ZONE 'UTC',
		       lapsed_at = NULL, updated_at = NOW()
		 WHERE tenant_id = $1
		RETURNING paid_through, std_tb, vault_tb, pin_hot_tb, period, renewal`,
		tenantID, by.interval()).Scan(&e.PaidThrough, &e.Order.Std, &e.Order.Vault, &e.Order.PinHot, &period, &renewal)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("extend tenant %s: %w", tenantID, ErrNoEntitlement)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("extend tenant %s: %w", tenantID, err)
	}
	if err := usage.SetHouseTx(ctx, tx, tenantID, e.Order.House()); err != nil {
		return time.Time{}, err
	}
	return e.PaidThrough, nil
}

// StopRenewal records that nobody will extend the date any more (a
// cancelled subscription): the house runs to paid_through + grace.
func StopRenewal(ctx context.Context, tx *sql.Tx, tenantID string) error {
	_, err := StopRenewalChanged(ctx, tx, tenantID)
	return err
}

// StopRenewalChanged is StopRenewal reporting whether a row changed (the
// caller writes billing.renewal_stopped only then).
func StopRenewalChanged(ctx context.Context, tx *sql.Tx, tenantID string) (bool, error) {
	if err := Lock(ctx, tx, tenantID); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE entitlements SET renewal = 'prepaid', updated_at = NOW() WHERE tenant_id = $1 AND renewal <> 'prepaid'`,
		tenantID)
	if err != nil {
		return false, fmt.Errorf("stop renewal of tenant %s: %w", tenantID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AuditRenewal writes billing.renewal_stopped / billing.renewal_resumed
// (actor: "stripe-webhook", the customer, an admin).
func AuditRenewal(ctx context.Context, db *sql.DB, tenantID string, renewal Renewal, actor, provider, ref string) {
	action := "billing.renewal_resumed"
	if renewal == RenewalPrepaid {
		action = "billing.renewal_stopped"
	}
	audit.Record(ctx, db, audit.Entry{Actor: actor, TenantID: tenantID, EventType: "billing", Action: action,
		Resource: "tenant:" + tenantID, Metadata: map[string]any{"tenant": tenantID, "actor": actor, "provider": provider, "provider_ref": ref, "renewal": string(renewal)}})
}

// Revoke clears the house (usage.ClearHouseTx: free-tier limits, used
// bytes untouched — billing never deletes data) and marks the entitlement
// lapsed in the same transaction, so the lapse job's reconcile never grants
// back a house that was taken away. Writers of lapsed_at: the lapse job
// and, while the entitlement_lapse flag is OFF, the Stripe webhook's
// clearing branch (WP-PAY-2 moves that branch behind the flag). A tenant
// with no entitlement row (a house granted before 097) has its floors
// cleared all the same.
func Revoke(ctx context.Context, tx *sql.Tx, tenantID string) error {
	if err := Lock(ctx, tx, tenantID); err != nil {
		return err
	}
	if err := usage.ClearHouseTx(ctx, tx, tenantID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE entitlements SET lapsed_at = NOW(), updated_at = NOW() WHERE tenant_id = $1 AND lapsed_at IS NULL`,
		tenantID); err != nil {
		return fmt.Errorf("revoke tenant %s: %w", tenantID, err)
	}
	return nil
}

// Ended is what EndByStatus did.
type Ended struct {
	// Kept: the row is granted with a paid_through after now — the house
	// stays, only its renewal stopped.
	Kept        bool
	PaidThrough time.Time
	// Stopped: the renewal flipped to prepaid in this call (audit it).
	Stopped bool
	// Switched: kept because the subscription that ended is the one the
	// customer's own switch to a prepaid provider stopped (EndByStatusOf,
	// RP37-3) — whatever its date.
	Switched bool
}

// EndByStatus applies a terminal provider status (a Stripe subscription
// canceled / unpaid / incomplete_expired / paused / deleted) while the
// entitlement_lapse flag is OFF: a GRANTED row whose paid_through is after
// now keeps its house and only stops renewing — paid time is never taken by
// a status (RP26-1, D-P23-1; a $0 trialing renew of a manually-paid house
// reaches here when the trial is cancelled). Otherwise the house is revoked
// as before (Revoke + StopRenewal), which is also what a tenant with no row
// gets. The decision and the write run under the tenant's entitlement lock.
func EndByStatus(ctx context.Context, tx *sql.Tx, tenantID string, now time.Time) (Ended, error) {
	if err := Lock(ctx, tx, tenantID); err != nil {
		return Ended{}, err
	}
	e, err := Get(ctx, tx, tenantID)
	switch {
	case errors.Is(err, ErrNoEntitlement):
	case err != nil:
		return Ended{}, err
	case e.LapsedAt == nil && e.PaidThrough.After(now):
		stopped, err := StopRenewalChanged(ctx, tx, tenantID)
		return Ended{Kept: true, PaidThrough: e.PaidThrough, Stopped: stopped}, err
	}
	if err := Revoke(ctx, tx, tenantID); err != nil {
		return Ended{}, err
	}
	stopped, err := StopRenewalChanged(ctx, tx, tenantID)
	return Ended{PaidThrough: e.PaidThrough, Stopped: stopped}, err
}

// EndByStatusOf is EndByStatus for the terminal status of one provider
// reference (the Stripe webhook's two call sites: a terminal status and a
// deleted subscription). When that reference ended because the customer
// switched the house to a prepaid provider — the entitlement still names
// it, renewal 'prepaid' with a pending provider, and a renewal_stop row of
// reason 'switch' names it (SwitchToPrepaid writes both in one
// transaction) — nothing is ended (RP37-3, D-P30-2): RP33-7's switch from
// a period in dunning cancels the subscription NOW, and its paid_through
// (the unpaid period's start) is already behind us, so EndByStatus would
// revoke a house the customer is paying another way. The house keeps its
// date and waits for the prepaid invoice; the entitlement_lapse job ends
// it at paid_through + ENTITLEMENT_GRACE like any house nobody renews
// (with the flag off it is counted would_clear, as every kept house is).
// Anything else: EndByStatus, unchanged.
func EndByStatusOf(ctx context.Context, tx *sql.Tx, tenantID, provider, ref string, now time.Time) (Ended, error) {
	if err := Lock(ctx, tx, tenantID); err != nil {
		return Ended{}, err
	}
	e, err := Get(ctx, tx, tenantID)
	if err != nil && !errors.Is(err, ErrNoEntitlement) {
		return Ended{}, err
	}
	if err == nil && ref != "" && e.LapsedAt == nil && e.Provider == provider && e.ProviderRef == ref &&
		e.Renewal == RenewalPrepaid && e.PendingProvider != "" {
		s, found, err := RenewalStopOf(ctx, tx, tenantID, provider, ref)
		if err != nil {
			return Ended{}, err
		}
		if found && s.Reason == ReasonSwitch {
			return Ended{Kept: true, PaidThrough: e.PaidThrough, Switched: true}, nil
		}
	}
	return EndByStatus(ctx, tx, tenantID, now)
}

// Resize changes a granted house's lines, keeping its date (the dashboard's
// in-place resize: Stripe has prorated it; the webhook that follows grants
// the same items). A tenant with no granted row gets its floors only.
func Resize(ctx context.Context, tx *sql.Tx, tenantID string, h usage.House) error {
	if err := Lock(ctx, tx, tenantID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE entitlements SET std_tb = $2, vault_tb = $3, pin_hot_tb = $4, updated_at = NOW()
		 WHERE tenant_id = $1 AND lapsed_at IS NULL
		   AND (std_tb, vault_tb, pin_hot_tb) IS DISTINCT FROM ($2, $3, $4)`,
		tenantID, h.StdTB(), h.VaultTB(), h.PinHotTB()); err != nil {
		return fmt.Errorf("resize tenant %s: %w", tenantID, err)
	}
	return usage.SetHouseTx(ctx, tx, tenantID, h)
}

// Get reads the tenant's entitlement (ErrNoEntitlement when there is none).
func Get(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, tenantID string) (Entitlement, error) {
	var e Entitlement
	var period, renewal string
	var ref, pending sql.NullString
	var lapsed sql.NullTime
	err := q.QueryRowContext(ctx, `
		SELECT tenant_id, std_tb, vault_tb, pin_hot_tb, period, paid_through, renewal, provider, provider_ref,
		       pending_provider, lapsed_at, created_at, updated_at
		  FROM entitlements WHERE tenant_id = $1`, tenantID).Scan(
		&e.TenantID, &e.Order.Std, &e.Order.Vault, &e.Order.PinHot, &period, &e.PaidThrough, &renewal,
		&e.Provider, &ref, &pending, &lapsed, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNoEntitlement
	}
	if err != nil {
		return e, fmt.Errorf("read entitlement of tenant %s: %w", tenantID, err)
	}
	e.Order.Period, e.Renewal, e.ProviderRef, e.PendingProvider = Period(period), Renewal(renewal), ref.String, pending.String
	if lapsed.Valid {
		t := lapsed.Time
		e.LapsedAt = &t
	}
	return e, nil
}

// Status is the read model of a tenant's house (the dashboard's, WP-PAY-2).
type Status struct {
	// Found is false for a tenant that never bought (the free tier).
	Found       bool
	Entitlement Entitlement
	// Granted: the house is in force (lapsed_at NULL).
	Granted bool
	// GraceEnds = paid_through + grace: the lapse job clears after it.
	GraceEnds time.Time
	// InGrace: paid_through has passed, the house is still granted.
	InGrace bool
}

// StatusOf reads the read model at now with the given grace.
func StatusOf(ctx context.Context, db *sql.DB, tenantID string, grace time.Duration, now time.Time) (Status, error) {
	e, err := Get(ctx, db, tenantID)
	if errors.Is(err, ErrNoEntitlement) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	s := Status{Found: true, Entitlement: e, Granted: e.LapsedAt == nil, GraceEnds: e.PaidThrough.Add(grace)}
	s.InGrace = s.Granted && now.After(e.PaidThrough)
	return s, nil
}

// HouseWriter writes a house's floors together with its entitlement — the
// billing.HouseQuotas the dashboard's resize uses, so no path changes the
// floors of a tenant the entitlement describes without changing it too.
type HouseWriter struct{ db *sql.DB }

// NewHouseWriter returns the writer over db.
func NewHouseWriter(db *sql.DB) *HouseWriter { return &HouseWriter{db: db} }

// SetHouse resizes the house in place (Resize) in one transaction.
func (w *HouseWriter) SetHouse(ctx context.Context, tenantID string, h usage.House) error {
	return w.inTx(ctx, func(tx *sql.Tx) error { return Resize(ctx, tx, tenantID, h) })
}

// ClearHouse clears it (Revoke) in one transaction.
func (w *HouseWriter) ClearHouse(ctx context.Context, tenantID string) error {
	return w.inTx(ctx, func(tx *sql.Tx) error { return Revoke(ctx, tx, tenantID) })
}

func (w *HouseWriter) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return InTx(ctx, w.db, fn)
}

// InTx runs fn in a transaction on db and commits it when fn succeeded.
func InTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
