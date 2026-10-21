package btcpay

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type flagsOn map[string]bool

func (f flagsOn) Enabled(key, _ string) bool { return f[key] }

// registry is a registry with pay_btcpay on and the provider checked Ready.
func (f *fixture) registry(t *testing.T) *payments.Registry {
	t.Helper()
	r := payments.NewRegistry(f.db, flagsOn{"pay_btcpay": true}, 0, zap.NewNop())
	f.p.Check(context.Background())
	r.Register(f.p)
	return r
}

func oneTB() payments.Order { return payments.Order{Std: 1, Period: payments.PeriodMonthly} }

func TestBTCPay_StartCheckout_PendingRowBeforeInvoice(t *testing.T) {
	// Arrange: the fake answers the create only after the test has looked
	// at the ledger — the row must already be there, pending, committed.
	f := newFixture(t)
	r := f.registry(t)
	var rowsAtCreate map[string]string
	orig := f.fake.ts.Config.Handler
	f.fake.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Path == "/api/v1/stores/"+fakeStore+"/invoices" {
			rowsAtCreate = f.ledger(t)
		}
		orig.ServeHTTP(w, req)
	})

	// Act
	url, err := r.StartCheckout(context.Background(), payments.ProviderBTCPay,
		payments.Checkout{TenantID: f.tenantID, Order: oneTB(), Purpose: payments.PurposeNew, ReturnURL: "https://stored.test/dashboard/billing?paid=1"})

	// Assert: one pending row under a provisional id at create time; after,
	// the invoice id on it, the checkout link returned, nothing granted.
	require.NoError(t, err)
	assert.Equal(t, "https://pay.test/i/FakeInv1", url)
	require.Len(t, rowsAtCreate, 1)
	for id, st := range rowsAtCreate {
		assert.Contains(t, id, "pending:")
		assert.Equal(t, payments.StatusPending, st)
	}
	assert.Equal(t, map[string]string{"FakeInv1": payments.StatusPending}, f.ledger(t))
	require.Len(t, f.fake.created, 1)
	req := f.fake.created[0]
	assert.Equal(t, "USD", req.Currency)
	assert.Equal(t, f.tenantID, req.Metadata["tenant_id"])
	assert.Equal(t, payments.PurposeNew, req.Metadata["purpose"])
	assert.Equal(t, 30, req.Checkout.ExpirationMinutes)
	assert.Equal(t, 1440, req.Checkout.MonitoringMinutes)
	assert.Equal(t, "https://stored.test/dashboard/billing?paid=1", req.Checkout.RedirectURL)
	_, err = payments.Get(context.Background(), f.db, f.tenantID)
	assert.ErrorIs(t, err, payments.ErrNoEntitlement, "no grant before Settled")
}

func TestBTCPay_StartCheckout_SubtractsCredit(t *testing.T) {
	// Arrange: $3.00 of credit from an earlier overpayment.
	f := newFixture(t)
	r := f.registry(t)
	require.NoError(t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		_, err := payments.SetCredit(context.Background(), tx, f.tenantID, payments.ProviderBTCPay, "overpaid:Old1", 300, "test", time.Now())
		return err
	}))
	charge := payments.QuoteHouse(1, 0, 0, payments.PeriodMonthly).ChargeCents

	// Act
	_, err := r.StartCheckout(context.Background(), payments.ProviderBTCPay,
		payments.Checkout{TenantID: f.tenantID, Order: oneTB(), Purpose: payments.PurposeNew})

	// Assert: the invoice is for the charge less the credit; the row
	// records both.
	require.NoError(t, err)
	require.Len(t, f.fake.created, 1)
	assert.Equal(t, usdString(charge-300), f.fake.created[0].Amount)
	var amount, credit int64
	require.NoError(t, f.db.QueryRow(`SELECT amount_cents, credit_cents FROM payments WHERE provider_id = 'FakeInv1'`).Scan(&amount, &credit))
	assert.Equal(t, charge-300, amount)
	assert.Equal(t, int64(300), credit)
}

func TestBTCPay_StartCheckout_CreditCoversAll_GrantsWithoutInvoiceOrNumber(t *testing.T) {
	f := newFixture(t)
	r := f.registry(t)
	require.NoError(t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		_, err := payments.SetCredit(context.Background(), tx, f.tenantID, payments.ProviderBTCPay, "overpaid:Big1", 1_000_00, "test", time.Now())
		return err
	}))

	url, err := r.StartCheckout(context.Background(), payments.ProviderBTCPay,
		payments.Checkout{TenantID: f.tenantID, Order: oneTB(), Purpose: payments.PurposeNew, ReturnURL: "https://stored.test/back"})

	require.NoError(t, err)
	assert.Equal(t, "https://stored.test/back", url)
	assert.Empty(t, f.fake.created, "no invoice")
	e := f.entitlement(t)
	assert.Equal(t, payments.ProviderBTCPay, e.Provider)
	assert.Equal(t, payments.RenewalPrepaid, e.Renewal)
	var numbered int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM payments WHERE tenant_id = $1 AND invoice_number IS NOT NULL`, f.tenantID).Scan(&numbered))
	assert.Zero(t, numbered, "a $0 payment takes no receipt number")
	charge := payments.QuoteHouse(1, 0, 0, payments.PeriodMonthly).ChargeCents
	assert.Equal(t, int64(1_000_00)-charge, f.credit(t))
}

func TestBTCPay_StartCheckout_SpeedPolicyByAmount(t *testing.T) {
	for _, tc := range []struct {
		cents int64
		want  string
	}{{499, SpeedMedium}, {19999, SpeedMedium}, {20000, SpeedLowMedium}, {120000, SpeedLowMedium}} {
		assert.Equal(t, tc.want, speedPolicy(tc.cents), "%d cents", tc.cents)
	}
	// And on the wire: a 1 TB month is small, 10 TB of Standard for a year
	// is not.
	f := newFixture(t)
	r := f.registry(t)
	_, err := r.StartCheckout(context.Background(), payments.ProviderBTCPay, payments.Checkout{TenantID: f.tenantID, Order: oneTB()})
	require.NoError(t, err)
	big := payments.Order{Std: 10, Period: payments.PeriodAnnual}
	require.GreaterOrEqual(t, payments.QuoteHouse(10, 0, 0, payments.PeriodAnnual).ChargeCents, int64(20000))
	_, err = r.StartCheckout(context.Background(), payments.ProviderBTCPay, payments.Checkout{TenantID: f.tenantID, Order: big})
	require.NoError(t, err)
	require.Len(t, f.fake.created, 2)
	assert.Equal(t, SpeedMedium, f.fake.created[0].Checkout.SpeedPolicy)
	assert.Equal(t, SpeedLowMedium, f.fake.created[1].Checkout.SpeedPolicy)
}

func TestBTCPay_StartCheckout_RefusedWhileStripeRenewsAuto(t *testing.T) {
	// Arrange: a house renewed automatically by Stripe.
	f := newFixture(t)
	r := f.registry(t)
	require.NoError(t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		return payments.Grant(context.Background(), tx, f.tenantID, oneTB(), time.Now().Add(20*24*time.Hour),
			payments.ProviderStripe, "sub_x", payments.RenewalAuto)
	}))

	_, err := r.StartCheckout(context.Background(), payments.ProviderBTCPay, payments.Checkout{TenantID: f.tenantID, Order: oneTB()})

	assert.ErrorIs(t, err, payments.ErrOtherProviderLive)
	assert.Empty(t, f.fake.created)
	assert.Empty(t, f.ledger(t), "no pending row either")
}

func TestBTCPay_Ready_UnreadyAfterTwoFailures_ReadyAfterOne(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	st, _ := f.p.Ready(ctx)
	assert.Equal(t, payments.Pending, st, "before the first check")

	f.fake.mu.Lock()
	f.fake.healthErr = http.StatusBadGateway
	f.fake.mu.Unlock()
	f.p.Check(ctx)
	st, _ = f.p.Ready(ctx)
	assert.Equal(t, payments.Pending, st, "one failure is not a verdict")
	f.p.Check(ctx)
	st, why := f.p.Ready(ctx)
	assert.Equal(t, payments.Unready, st)
	assert.Contains(t, why, "health")

	f.fake.mu.Lock()
	f.fake.healthErr = 0
	f.fake.storeErr = http.StatusForbidden // a key that cannot read its store
	f.fake.mu.Unlock()
	f.p.Check(ctx)
	st, why = f.p.Ready(ctx)
	assert.Equal(t, payments.Unready, st)
	assert.Contains(t, why, "store")

	f.fake.mu.Lock()
	f.fake.storeErr = 0
	f.fake.mu.Unlock()
	f.p.Check(ctx)
	st, _ = f.p.Ready(ctx)
	assert.Equal(t, payments.Ready, st, "one success")
	f.fake.mu.Lock()
	f.fake.healthErr = http.StatusBadGateway
	f.fake.mu.Unlock()
	f.p.Check(ctx)
	st, _ = f.p.Ready(ctx)
	assert.Equal(t, payments.Ready, st, "one failure after Ready keeps it Ready")
}

func TestBTCPay_Resize_TopUpAmount(t *testing.T) {
	day := 24 * time.Hour
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	m := func(std, vault int) payments.Order {
		return payments.Order{Std: std, Vault: vault, Period: payments.PeriodMonthly}
	}
	y := func(std, vault int) payments.Order {
		return payments.Order{Std: std, Vault: vault, Period: payments.PeriodAnnual}
	}
	charge := func(o payments.Order) int64 {
		return payments.QuoteHouse(o.Std, o.Vault, o.PinHot, o.Period).ChargeCents
	}
	cases := []struct {
		name        string
		old, new    payments.Order
		paidThrough time.Time
		remaining   int
		period      int
	}{
		{"monthly, half the month left", m(1, 0), m(2, 0), now.Add(15 * day), 15, 30},
		{"monthly, last hours count as a day", m(1, 0), m(3, 0), now.Add(2 * time.Hour), 1, 30},
		{"annual, 100 days left", y(1, 0), y(2, 1), now.Add(100 * day), 100, 365},
		{"annual, the whole year left", y(2, 0), y(5, 0), now.AddDate(1, 0, 0), 365, 365},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tu, err := PriceTopUp(tc.old, tc.new, tc.paidThrough, now)
			require.NoError(t, err)
			assert.Equal(t, tc.remaining, tu.RemainingDays)
			assert.InDelta(t, tc.period, tu.PeriodDays, 1)
			want := float64(charge(tc.new)-charge(tc.old)) * float64(tu.RemainingDays) / float64(tu.PeriodDays)
			assert.InDelta(t, want, float64(tu.Cents), 0.5)
			assert.Positive(t, tu.Cents)
		})
	}
	// Down: no money moves. A period switch: refused.
	tu, err := PriceTopUp(m(3, 0), m(1, 0), now.Add(10*day), now)
	require.NoError(t, err)
	assert.Zero(t, tu.Cents)
	_, err = PriceTopUp(m(1, 0), y(1, 0), now.Add(10*day), now)
	assert.ErrorIs(t, err, ErrPeriodSwitch)

	// Through the provider: an annual house resized up opens a resize
	// invoice for the pro-rated difference, with the inputs printed.
	f := newFixture(t)
	r := f.registry(t)
	require.NoError(t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		return payments.Grant(context.Background(), tx, f.tenantID, y(1, 0), time.Now().Add(100*day),
			payments.ProviderBTCPay, "Inv0", payments.RenewalPrepaid)
	}))
	out, err := r.Resize(context.Background(), f.tenantID, y(2, 1))
	require.NoError(t, err)
	assert.Nil(t, out.Applied, "the house changes when the top-up settles")
	assert.Contains(t, out.RedirectURL, "https://pay.test/i/")
	require.Len(t, f.fake.created, 1)
	req := f.fake.created[0]
	assert.Equal(t, payments.PurposeResize, req.Metadata["purpose"])
	top, ok := req.Metadata["top_up"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, top, "remaining_days")
	assert.Contains(t, top, "formula")
	assert.Equal(t, 1, f.entitlement(t).Order.Std, "unchanged until paid")
}

func TestBTCPay_NoOps(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	assert.Equal(t, "btcpay", f.p.Name())
	assert.Equal(t, payments.RenewalPrepaid, f.p.Renewal())
	assert.NoError(t, f.p.StopRenewal(ctx, f.tenantID))
	assert.NoError(t, f.p.ResumeRenewal(ctx, f.tenantID))
	assert.NoError(t, f.p.Cancel(ctx, f.tenantID))
	u, err := f.p.ManageURL(ctx, f.tenantID, "x")
	assert.NoError(t, err)
	assert.Empty(t, u)
}
