package payments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"
)

// The provider interface (WP-PAY-2, docs/PAYMENTS.md § 2): a provider is one
// way of taking money for a house — Stripe today (internal/billing), BTCPay
// next. Everything provider-neutral (the record, the guard, the read model)
// lives here and never imports a provider SDK; the dashboard reaches a
// provider only through a Registry.

// Checkout is a request to start paying for a house.
type Checkout struct {
	TenantID, Email string
	Order           Order
	// Purpose is the ledger's: PurposeNew, PurposeRenew, PurposeResize,
	// PurposeSwitch.
	Purpose              string
	ReturnURL, CancelURL string
	// CreditCents is a credit the ledger holds for the tenant (a provider
	// that cannot apply one ignores it; Stripe does, BTCPay subtracts it).
	CreditCents int64
}

// ResizeOutcome is what a provider did with a resize: changed the house in
// place (Applied = the house it now bills, written to the record by the
// Registry) or sent the customer somewhere to pay the difference
// (RedirectURL). The design's name for it is "Resize"; that is the name of
// the record's own resize function here (D-P19-B1).
type ResizeOutcome struct {
	Applied     *Order
	RedirectURL string
}

// Readiness is a provider's answer to "can you take a payment now?"
// (D-P20-1, RP22-1): Unready = no (no key, a price mismatch, a missing
// price id — a verdict); Ready = yes; Pending = configured, but no
// verification has completed yet (the minute after a boot, Stripe not yet
// reached). Pending is never recorded as an outage and never counts as
// unready for the lapse job; it is not Ready for new business either. The
// zero value is Unready, as a provider with no key.
type Readiness int

const (
	// Unready: a verdict — the provider cannot take a payment.
	Unready Readiness = iota
	// Ready: configured and verified.
	Ready
	// Pending: configured, verification not completed yet.
	Pending
)

// String names the state for logs and the admin page.
func (r Readiness) String() string {
	switch r {
	case Ready:
		return "ready"
	case Pending:
		return "pending"
	default:
		return "unready"
	}
}

// Provider is one way of taking money (docs/PAYMENTS.md § 2.2). Customer
// creation is not on it: it is an implementation detail of StartCheckout
// (no third party knows a tenant until it buys — 43.2).
type Provider interface {
	Name() string
	// Renewal is what this provider's payments imply (Stripe: auto).
	Renewal() Renewal
	// Ready: Ready when configured AND verified (Stripe: key set, prices
	// match), Pending while a verification has not completed, else
	// Unready and why.
	Ready(ctx context.Context) (Readiness, string)
	StartCheckout(ctx context.Context, c Checkout) (redirectURL string, err error)
	Resize(ctx context.Context, tenantID string, o Order) (ResizeOutcome, error)
	// StopRenewal / ResumeRenewal: Stripe cancel_at_period_end true/false.
	StopRenewal(ctx context.Context, tenantID string) error
	ResumeRenewal(ctx context.Context, tenantID string) error
	// Cancel stops the money now (the deletion runner): idempotent;
	// ErrProviderUnconfigured when the provider cannot be reached at all.
	Cancel(ctx context.Context, tenantID string) error
	ManageURL(ctx context.Context, tenantID, returnURL string) (string, error)
	// Webhook is mounted at /webhook/<Name()> while the provider's secrets
	// are set — whatever its flag (late notifications must land).
	Webhook() http.Handler
}

var (
	// ErrUnknownProvider: no provider of that name is registered.
	ErrUnknownProvider = errors.New("unknown payment provider")
	// ErrProviderDisabled: flag pay_<name> is off for the tenant (409
	// provider_disabled — "no new business", never "cancel everyone").
	ErrProviderDisabled = errors.New("provider_disabled")
	// ErrProviderUnavailable: the provider is not Ready (not configured, or
	// its prices are not verified).
	ErrProviderUnavailable = errors.New("provider_unavailable")
	// ErrProviderUnconfigured: the provider has no credentials at all — it
	// cannot even cancel.
	ErrProviderUnconfigured = errors.New("provider_unconfigured")
	// ErrOtherProviderLive: the tenant's house is renewed by another
	// provider (409 other_provider_live, the double-billing guard § 2.5).
	ErrOtherProviderLive = errors.New("other_provider_live")
	// ErrLiveSubscription: the provider already bills this tenant (Stripe's
	// HasLiveSubscription — R10-05).
	ErrLiveSubscription = errors.New("live_subscription")
)

// FlagPrefix + a provider's name is its flag (pay_stripe, pay_btcpay).
const FlagPrefix = "pay_"

// FlagEntitlementLapse is the flag under which the date decides (WP-PAY-1):
// the lapse job clears a house past paid_through + grace, and a provider's
// terminal event only stops the renewal (registered by internal/api).
const FlagEntitlementLapse = "entitlement_lapse"

// FlagSource is the slice of flags.Service the registry reads.
type FlagSource interface {
	Enabled(key, tenantID string) bool
}

// Registry holds the providers and is the dashboard's one door to money:
// which providers a tenant may use, the house's read model, checkout behind
// the double-billing guard, resize, the manage link.
type Registry struct {
	db     *sql.DB
	flags  FlagSource
	grace  time.Duration
	logger *zap.Logger
	ready  *ReadyLog

	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry returns an empty registry. A nil flag source turns every
// provider off for new business (no flags = off, the dashboard's rule).
func NewRegistry(db *sql.DB, fl FlagSource, grace time.Duration, logger *zap.Logger) *Registry {
	if grace <= 0 {
		grace = DefaultGrace
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Registry{db: db, flags: fl, grace: grace, logger: logger, ready: NewReadyLog(),
		providers: map[string]Provider{}}
}

// Register adds p and exports its readiness series at once.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	r.providers[p.Name()] = p
	r.mu.Unlock()
	r.observe(context.Background(), p, time.Now())
}

// Provider returns the named provider whatever its flag (cancel, manage
// link, webhooks), nil when none is registered.
func (r *Registry) Provider(name string) Provider {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[name]
}

// Names lists the registered providers, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Grace is the registry's ENTITLEMENT_GRACE.
func (r *Registry) Grace() time.Duration { return r.grace }

// For returns the provider a tenant may start new business with:
// ErrUnknownProvider, ErrProviderDisabled (flag pay_<name> off for the
// tenant), ErrProviderUnavailable (not Ready), in that order.
func (r *Registry) For(ctx context.Context, tenantID, name string) (Provider, error) {
	p := r.Provider(name)
	if p == nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, name)
	}
	if r.flags == nil || !r.flags.Enabled(FlagPrefix+name, tenantID) {
		return nil, fmt.Errorf("%s for tenant %s: %w", name, tenantID, ErrProviderDisabled)
	}
	if st, why := p.Ready(ctx); st != Ready {
		return nil, fmt.Errorf("%s: %w (%s): %s", name, ErrProviderUnavailable, st, why)
	}
	return p, nil
}

// Enabled lists the providers a tenant may start new business with now
// (flag on, Ready) — what the billing page offers.
func (r *Registry) Enabled(ctx context.Context, tenantID string) []string {
	var out []string
	for _, n := range r.Names() {
		if _, err := r.For(ctx, tenantID, n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// Ready reports whether a tenant may check out with the named provider,
// and why not ("provider_disabled", the provider's own reason, …).
func (r *Registry) Ready(ctx context.Context, tenantID, name string) (bool, string) {
	if _, err := r.For(ctx, tenantID, name); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// Status is the house's read model at now (StatusOf with the grace).
func (r *Registry) Status(ctx context.Context, tenantID string) (Status, error) {
	if r.db == nil {
		return Status{}, nil
	}
	return StatusOf(ctx, r.db, tenantID, r.grace, time.Now())
}

// StartCheckout starts the named provider's checkout behind the
// double-billing guard (StartCheckout, the package function).
func (r *Registry) StartCheckout(ctx context.Context, provider string, c Checkout) (string, error) {
	return StartCheckout(ctx, r, c.TenantID, provider, c)
}

// checkoutLockTimeout bounds the guard's wait for the entitlement lock
// (RP22-4): the lock is held across up to three Stripe round trips, so a
// webhook of the same tenant, or a pool exhausted by waiters, must turn
// into a clean "try again in a minute" (ErrProviderUnavailable), never a
// hang. A var for the test that proves it.
var checkoutLockTimeout = 10 * time.Second

// lockTimedOut reports Postgres's lock_not_available (55P03), what a
// `SET LOCAL lock_timeout` turns a wait into.
func lockTimedOut(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "55P03"
}

// StartCheckout is the double-billing guard (docs/PAYMENTS.md § 2.5): under
// the tenant's entitlement lock — the lock every grant takes, so a webhook
// and a checkout cannot pass each other — it refuses ErrOtherProviderLive
// when the tenant's granted house is renewed (renewal 'auto') by another
// provider, then runs the provider's own StartCheckout (Stripe's
// HasLiveSubscription is the second guard, for a second Stripe
// subscription). The lock is held across the provider call, and waited
// for at most checkoutLockTimeout (RP22-4).
func StartCheckout(ctx context.Context, r *Registry, tenantID, provider string, c Checkout) (string, error) {
	p, err := r.For(ctx, tenantID, provider)
	if err != nil {
		return "", err
	}
	c.TenantID = tenantID
	if r.db == nil {
		return p.StartCheckout(ctx, c)
	}
	var url string
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
		var other, renewal string
		err := tx.QueryRowContext(ctx,
			`SELECT provider, renewal FROM entitlements WHERE tenant_id = $1 AND lapsed_at IS NULL`, tenantID).
			Scan(&other, &renewal)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("read entitlement of tenant %s: %w", tenantID, err)
		case other != provider && Renewal(renewal) == RenewalAuto:
			return fmt.Errorf("tenant %s is renewed by %s: %w", tenantID, other, ErrOtherProviderLive)
		}
		u, err := p.StartCheckout(ctx, c)
		url = u
		return err
	})
	return url, err
}

// houseProvider is the provider that bills the tenant's house now: the
// entitlement row's, else stripe (a house granted before 097 has no row).
func (r *Registry) houseProvider(ctx context.Context, tenantID string) (string, error) {
	if r.db == nil {
		return ProviderStripe, nil
	}
	e, err := Get(ctx, r.db, tenantID)
	if errors.Is(err, ErrNoEntitlement) {
		return ProviderStripe, nil
	}
	if err != nil {
		return "", err
	}
	return e.Provider, nil
}

// Resize changes the tenant's house with the provider that bills it (flag
// and readiness as a checkout: 409 provider_disabled when it is off) and,
// when the provider changed it in place, writes the record and the floors
// in one transaction (payments.Resize — the webhook that follows grants the
// same items).
func (r *Registry) Resize(ctx context.Context, tenantID string, o Order) (ResizeOutcome, error) {
	name, err := r.houseProvider(ctx, tenantID)
	if err != nil {
		return ResizeOutcome{}, err
	}
	p, err := r.For(ctx, tenantID, name)
	if err != nil {
		return ResizeOutcome{}, err
	}
	out, err := p.Resize(ctx, tenantID, o)
	if err != nil || out.Applied == nil || r.db == nil {
		return out, err
	}
	applied := *out.Applied
	if err := InTx(ctx, r.db, func(tx *sql.Tx) error { return Resize(ctx, tx, tenantID, applied.House()) }); err != nil {
		// The provider has the new house; the webhook writes it again.
		r.logger.Error("payments: resize applied at the provider but not recorded — the webhook will",
			zap.String("tenant", tenantID), zap.String("provider", name), zap.Error(err))
	}
	return out, nil
}

// ManageURL is the provider's self-service page for the tenant's house (the
// Stripe portal). Never flag-gated: managing a payment stops or changes
// money, it is not new business.
func (r *Registry) ManageURL(ctx context.Context, tenantID, returnURL string) (string, error) {
	name, err := r.houseProvider(ctx, tenantID)
	if err != nil {
		return "", err
	}
	p := r.Provider(name)
	if p == nil {
		return "", fmt.Errorf("%w: %q", ErrUnknownProvider, name)
	}
	return p.ManageURL(ctx, tenantID, returnURL)
}

// --- readiness ----------------------------------------------------------

// ReadyLog remembers when each provider was last seen unready. In memory:
// a restart forgets an outage older than the process (RP21-1).
type ReadyLog struct {
	mu          sync.Mutex
	lastUnready map[string]time.Time
}

// NewReadyLog returns an empty log.
func NewReadyLog() *ReadyLog { return &ReadyLog{lastUnready: map[string]time.Time{}} }

// Record notes one observation: only Unready is an outage; Ready and
// Pending (no verdict yet) leave the log alone.
func (l *ReadyLog) Record(provider string, st Readiness, at time.Time) {
	if st != Unready {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if at.After(l.lastUnready[provider]) {
		l.lastUnready[provider] = at
	}
}

// UnreadyWithin reports an unready observation in (now - window, now].
func (l *ReadyLog) UnreadyWithin(_ context.Context, provider string, window time.Duration, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.lastUnready[provider]
	return ok && !t.Before(now.Add(-window))
}

// History is the registry's readiness log.
func (r *Registry) History() *ReadyLog { return r.ready }

// UnreadyWithin makes the registry the lapse job's ReadyHistory: a provider
// unready NOW counts (and is recorded); one pending now is not unready
// (RP22-1) — the log answers for it as for a ready one. A provider that is
// not registered (manual) is never unready.
func (r *Registry) UnreadyWithin(ctx context.Context, provider string, window time.Duration, now time.Time) bool {
	if p := r.Provider(provider); p != nil {
		if r.observe(ctx, p, now) == Unready {
			return true
		}
	}
	return r.ready.UnreadyWithin(ctx, provider, window, now)
}

// observe asks the provider once, records the answer and exports the gauge
// (1 for Ready; 0 for Unready and for Pending).
func (r *Registry) observe(ctx context.Context, p Provider, now time.Time) Readiness {
	st, _ := p.Ready(ctx)
	r.ready.Record(p.Name(), st, now)
	v := 0.0
	if st == Ready {
		v = 1
	}
	providerReady.WithLabelValues(p.Name()).Set(v)
	return st
}

// RecordReadiness observes every registered provider once.
func (r *Registry) RecordReadiness(ctx context.Context, now time.Time) {
	for _, n := range r.Names() {
		if p := r.Provider(n); p != nil {
			r.observe(ctx, p, now)
		}
	}
}

// StartReadiness records every provider's readiness every interval (60 s
// in production) until ctx ends: the gauge
// vaultaire_payment_provider_ready{provider} and the log the lapse job reads.
func (r *Registry) StartReadiness(ctx context.Context, every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				r.RecordReadiness(ctx, now)
			}
		}
	}()
}
