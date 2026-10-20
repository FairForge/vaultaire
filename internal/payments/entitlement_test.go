package payments

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/FairForge/vaultaire/internal/usage"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fixture struct {
	db       *sql.DB
	qm       *usage.QuotaManager
	tenantID string
}

// newFixture opens the test database and creates a tenant of its own with a
// free-tier quota row; every row it touches is the tenant's.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tenantID := fmt.Sprintf("pay-%d-%d", time.Now().UnixNano(), rand.Intn(1_000_000))
	_, err = db.Exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'Pay', $2)`, tenantID, tenantID+"@test.local")
	require.NoError(t, err)
	qm := usage.NewQuotaManager(db)
	require.NoError(t, qm.CreateTenant(context.Background(), tenantID, "free", usage.FreeTierLimits.StorageBytes))
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM payments WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	return &fixture{db: db, qm: qm, tenantID: tenantID}
}

func (f *fixture) tx(t *testing.T, fn func(*sql.Tx) error) {
	t.Helper()
	require.NoError(t, InTx(context.Background(), f.db, fn))
}

func (f *fixture) entitlement(t *testing.T) Entitlement {
	t.Helper()
	e, err := Get(context.Background(), f.db, f.tenantID)
	require.NoError(t, err)
	return e
}

func (f *fixture) house(t *testing.T) (usage.House, bool) {
	t.Helper()
	h, ok, err := f.qm.GetHouse(context.Background(), f.tenantID)
	require.NoError(t, err)
	return h, ok
}

func TestGrant_Idempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	o := Order{Std: 3, Vault: 2, PinHot: 1, Period: PeriodMonthly}
	paid := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)

	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, o, paid, ProviderStripe, "sub_1", RenewalAuto)
	})
	first := f.entitlement(t)
	assert.Equal(t, o, first.Order)
	assert.True(t, paid.Equal(first.PaidThrough))
	assert.Equal(t, RenewalAuto, first.Renewal)
	assert.Equal(t, ProviderStripe, first.Provider)
	assert.Equal(t, "sub_1", first.ProviderRef)
	assert.Nil(t, first.LapsedAt)
	h, ok := f.house(t)
	require.True(t, ok, "the floors are written with the entitlement")
	assert.Equal(t, usage.HouseFromTB(3, 2, 1), h)

	// The replay changes no row (updated_at untouched).
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, o, paid, ProviderStripe, "sub_1", RenewalAuto)
	})
	again := f.entitlement(t)
	assert.Equal(t, first.UpdatedAt, again.UpdatedAt, "a replayed grant is a no-op")
	h, _ = f.house(t)
	assert.Equal(t, usage.HouseFromTB(3, 2, 1), h)
}

// A grant never shortens a running date (a stale event cannot take paid time
// away); a lapsed row is granted again at the date given.
func TestGrant_NeverShortensARunningDate_ReGrantsALapsedRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	o := Order{Std: 1, Period: PeriodMonthly}
	later := time.Now().Add(40 * 24 * time.Hour).UTC().Truncate(time.Second)
	earlier := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)

	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, o, later, ProviderStripe, "sub_1", RenewalAuto)
	})
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, o, earlier, ProviderStripe, "sub_1", RenewalAuto)
	})
	assert.True(t, later.Equal(f.entitlement(t).PaidThrough), "the later date stays")

	// Lapsed with a date in the past: a new grant takes the new date.
	past := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	_, err := f.db.Exec(`UPDATE entitlements SET paid_through = $2, lapsed_at = NOW() WHERE tenant_id = $1`, f.tenantID, past)
	require.NoError(t, err)
	f.tx(t, func(tx *sql.Tx) error { return Revoke(ctx, tx, f.tenantID) })
	_, ok := f.house(t)
	require.False(t, ok)

	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, o, earlier, ProviderStripe, "sub_2", RenewalAuto)
	})
	e := f.entitlement(t)
	assert.Nil(t, e.LapsedAt, "granted again")
	assert.True(t, earlier.Equal(e.PaidThrough))
	assert.Equal(t, "sub_2", e.ProviderRef)
	_, ok = f.house(t)
	assert.True(t, ok, "the floors are back")
}

// Extend adds a period to a running date, and restarts a lapsed one from now
// (and grants it again).
func TestExtend_FromTheLaterOfPaidThroughAndNow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	o := Order{Std: 2, Period: PeriodMonthly}
	running := time.Date(2030, 3, 10, 12, 0, 0, 0, time.UTC)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, o, running, ProviderBTCPay, "inv_1", RenewalPrepaid)
	})

	var got time.Time
	f.tx(t, func(tx *sql.Tx) (err error) { got, err = Extend(ctx, tx, f.tenantID, PeriodMonthly); return })
	assert.True(t, time.Date(2030, 4, 10, 12, 0, 0, 0, time.UTC).Equal(got), "running: from paid_through, got %s", got)

	// Lapsed: the date passed long ago, the floors were cleared.
	_, err := f.db.Exec(`UPDATE entitlements SET paid_through = NOW() - INTERVAL '100 days' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	f.tx(t, func(tx *sql.Tx) error { return Revoke(ctx, tx, f.tenantID) })
	f.tx(t, func(tx *sql.Tx) (err error) { got, err = Extend(ctx, tx, f.tenantID, PeriodAnnual); return })
	want := time.Now().UTC().AddDate(1, 0, 0)
	assert.WithinDuration(t, want, got, 2*time.Minute, "lapsed: from now")
	e := f.entitlement(t)
	assert.Nil(t, e.LapsedAt)
	h, ok := f.house(t)
	require.True(t, ok)
	assert.Equal(t, usage.HouseFromTB(2, 0, 0), h)

	_, err = Extend(ctx, nil, "x", Period("weekly"))
	assert.Error(t, err)
	err = InTx(ctx, f.db, func(tx *sql.Tx) error { _, err := Extend(ctx, tx, f.tenantID+"-none", PeriodMonthly); return err })
	assert.ErrorIs(t, err, ErrNoEntitlement)
}

// Month arithmetic is Postgres's in UTC: the 31st plus a month is the last
// day of the next month (and stays there).
func TestExtend_MonthEndRule(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, tc := range []struct{ from, want time.Time }{
		{time.Date(2031, 1, 31, 23, 30, 0, 0, time.UTC), time.Date(2031, 2, 28, 23, 30, 0, 0, time.UTC)},
		{time.Date(2032, 1, 31, 0, 0, 0, 0, time.UTC), time.Date(2032, 2, 29, 0, 0, 0, 0, time.UTC)}, // leap year
		{time.Date(2031, 3, 31, 6, 0, 0, 0, time.UTC), time.Date(2031, 4, 30, 6, 0, 0, 0, time.UTC)},
	} {
		_, err := f.db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, f.tenantID)
		require.NoError(t, err)
		f.tx(t, func(tx *sql.Tx) error {
			return Grant(ctx, tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, tc.from, ProviderManual, "", RenewalPrepaid)
		})
		var got time.Time
		f.tx(t, func(tx *sql.Tx) (err error) { got, err = Extend(ctx, tx, f.tenantID, PeriodMonthly); return })
		assert.True(t, tc.want.Equal(got), "%s + 1 month = %s, want %s", tc.from, got.UTC(), tc.want)
	}
}

func TestStopRenewal_AndRevoke(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	paid := time.Now().Add(20 * 24 * time.Hour)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, Order{Std: 1, Vault: 1, Period: PeriodAnnual}, paid, ProviderStripe, "sub_9", RenewalAuto)
	})
	_, err := f.db.Exec(`UPDATE tenant_quotas SET storage_used_bytes = 1234 WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)

	f.tx(t, func(tx *sql.Tx) error { return StopRenewal(ctx, tx, f.tenantID) })
	e := f.entitlement(t)
	assert.Equal(t, RenewalPrepaid, e.Renewal)
	assert.Nil(t, e.LapsedAt, "stopping renewal keeps the house to its date")

	f.tx(t, func(tx *sql.Tx) error { return Revoke(ctx, tx, f.tenantID) })
	e = f.entitlement(t)
	require.NotNil(t, e.LapsedAt)
	_, ok := f.house(t)
	assert.False(t, ok, "the floors are gone")
	used, limit, err := f.qm.GetUsage(ctx, f.tenantID)
	require.NoError(t, err)
	assert.Equal(t, int64(1234), used, "billing never deletes data: used bytes untouched")
	assert.Equal(t, usage.FreeTierLimits.StorageBytes, limit)
}

// The dashboard's in-place resize writes the entitlement's lines with the
// floors and keeps the date.
func TestHouseWriter_ResizeKeepsTheDate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	paid := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, paid, ProviderStripe, "sub_r", RenewalAuto)
	})
	w := NewHouseWriter(f.db)
	require.NoError(t, w.SetHouse(ctx, f.tenantID, usage.HouseFromTB(4, 2, 1)))
	e := f.entitlement(t)
	assert.Equal(t, Order{Std: 4, Vault: 2, PinHot: 1, Period: PeriodMonthly}, e.Order)
	assert.True(t, paid.Equal(e.PaidThrough))
	h, _ := f.house(t)
	assert.Equal(t, usage.HouseFromTB(4, 2, 1), h)

	require.NoError(t, w.ClearHouse(ctx, f.tenantID))
	assert.NotNil(t, f.entitlement(t).LapsedAt)
}

func TestStatusOf(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s, err := StatusOf(ctx, f.db, f.tenantID, DefaultGrace, time.Now())
	require.NoError(t, err)
	assert.False(t, s.Found, "never bought")

	paid := time.Now().Add(-24 * time.Hour)
	f.tx(t, func(tx *sql.Tx) error {
		return Grant(ctx, tx, f.tenantID, Order{Std: 1, Period: PeriodMonthly}, paid, ProviderStripe, "sub_s", RenewalAuto)
	})
	s, err = StatusOf(ctx, f.db, f.tenantID, DefaultGrace, time.Now())
	require.NoError(t, err)
	assert.True(t, s.Found)
	assert.True(t, s.Granted)
	assert.True(t, s.InGrace, "paid_through passed, still granted")
	assert.WithinDuration(t, paid.Add(DefaultGrace), s.GraceEnds, time.Second)
}

func TestGraceFromEnv(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	assert.Equal(t, DefaultGrace, GraceFromEnv(env(""), nil))
	assert.Equal(t, 336*time.Hour, GraceFromEnv(env("336h"), nil))
	assert.Equal(t, 2*time.Hour, GraceFromEnv(env("2h"), nil))
	assert.Equal(t, DefaultGrace, GraceFromEnv(env("30m"), nil), "under 1h")
	assert.Equal(t, DefaultGrace, GraceFromEnv(env("1441h"), nil), "over 60d")
	assert.Equal(t, DefaultGrace, GraceFromEnv(env("two weeks"), nil))
}

func TestGrant_RefusesABadOrder(t *testing.T) {
	ctx := context.Background()
	assert.Error(t, Grant(ctx, nil, "t", Order{Std: 1, Period: "weekly"}, time.Now(), ProviderStripe, "", RenewalAuto))
	assert.Error(t, Grant(ctx, nil, "t", Order{Std: 1, PinHot: 2, Period: PeriodMonthly}, time.Now(), ProviderStripe, "", RenewalAuto))
}

// --- the ledger ---

func TestSettle_IdempotentAndFailedThenSettled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p := Payment{TenantID: f.tenantID, Provider: ProviderStripe, ProviderID: "in_" + f.tenantID,
		Kind: KindCharge, Purpose: PurposeRenew, AmountCents: 499,
		Order: &Order{Std: 1, Period: PeriodMonthly}, Metadata: map[string]any{MetaIDs: map[string]any{"subscription": "sub_x"}}}

	f.tx(t, func(tx *sql.Tx) error { _, err := Fail(ctx, tx, p); return err })
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM payments WHERE provider_id = $1`, p.ProviderID).Scan(&status))
	assert.Equal(t, StatusFailed, status)

	at := time.Date(2300+rand.Intn(500), 5, 1, 0, 0, 0, 0, time.UTC) // a year of the test's own
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM invoice_counters WHERE year = $1`, at.Year()) })
	var s1, s2 Settled
	f.tx(t, func(tx *sql.Tx) (err error) { s1, err = Settle(ctx, tx, p, at); return })
	assert.True(t, s1.Newly)
	assert.Equal(t, InvoiceNumber(at.Year(), 1), s1.InvoiceNumber)
	f.tx(t, func(tx *sql.Tx) (err error) { s2, err = Settle(ctx, tx, p, at); return })
	assert.False(t, s2.Newly, "a replay is not a second payment")
	assert.Equal(t, s1.InvoiceNumber, s2.InvoiceNumber)
	assert.Equal(t, s1.ID, s2.ID)

	// A late failure notification never moves a settled row back.
	f.tx(t, func(tx *sql.Tx) error { _, err := Fail(ctx, tx, p); return err })
	require.NoError(t, f.db.QueryRow(`SELECT status FROM payments WHERE provider_id = $1`, p.ProviderID).Scan(&status))
	assert.Equal(t, StatusSettled, status)
}

// Ten concurrent settles in one year are numbered 1..10 with no gap and no
// duplicate; a settle whose transaction rolls back gives its number back.
func TestSettle_InvoiceNumbersGaplessUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	at := time.Date(2900+rand.Intn(90), 7, 1, 0, 0, 0, 0, time.UTC)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM invoice_counters WHERE year = $1`, at.Year()) })

	// One rolled back first: its number is reused.
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = Settle(ctx, tx, Payment{TenantID: f.tenantID, Provider: ProviderManual, ProviderID: "manual:rollback-" + f.tenantID,
		Kind: KindCharge, Purpose: PurposeManual, AmountCents: 1}, at)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	var wg sync.WaitGroup
	numbers := make([]string, 10)
	errs := make([]error, 10)
	for i := range numbers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = InTx(ctx, f.db, func(tx *sql.Tx) error {
				s, err := Settle(ctx, tx, Payment{TenantID: f.tenantID, Provider: ProviderManual,
					ProviderID: fmt.Sprintf("manual:%s-%d", f.tenantID, i), Kind: KindCharge, Purpose: PurposeManual, AmountCents: 100}, at)
				numbers[i] = s.InvoiceNumber
				return err
			})
		}(i)
	}
	wg.Wait()
	want := make([]string, 10)
	for i := range want {
		require.NoError(t, errs[i])
		want[i] = InvoiceNumber(at.Year(), i+1)
	}
	assert.ElementsMatch(t, want, numbers)
}

func TestLedger_ExpireInvalidateCredit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	base := Payment{TenantID: f.tenantID, Provider: ProviderBTCPay, Kind: KindCharge, Purpose: PurposeNew, AmountCents: 2400}
	a, b := base, base
	a.ProviderID, b.ProviderID = "btc-a-"+f.tenantID, "btc-b-"+f.tenantID
	f.tx(t, func(tx *sql.Tx) error {
		if _, err := RecordPending(ctx, tx, a); err != nil {
			return err
		}
		_, err := RecordPending(ctx, tx, b)
		return err
	})
	var ok bool
	f.tx(t, func(tx *sql.Tx) (err error) { ok, err = Expire(ctx, tx, ProviderBTCPay, a.ProviderID, true); return })
	assert.True(t, ok)
	f.tx(t, func(tx *sql.Tx) (err error) { ok, err = Invalidate(ctx, tx, ProviderBTCPay, b.ProviderID); return })
	assert.True(t, ok)
	f.tx(t, func(tx *sql.Tx) (err error) { ok, err = Expire(ctx, tx, ProviderBTCPay, b.ProviderID, false); return })
	assert.False(t, ok, "only an open row expires")

	c := Payment{TenantID: f.tenantID, Provider: ProviderManual, ProviderID: "manual:credit-" + f.tenantID, CreditCents: 250}
	f.tx(t, func(tx *sql.Tx) error { _, err := Credit(ctx, tx, c, time.Now()); return err })
	var kind, status string
	var number sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT kind, status, invoice_number FROM payments WHERE provider_id = $1`, c.ProviderID).
		Scan(&kind, &status, &number))
	assert.Equal(t, KindCredit, kind)
	assert.Equal(t, StatusSettled, status)
	assert.False(t, number.Valid, "a credit is not a receipt")
}

// RP20-4: an invalid order is a typed error a webhook can tell from a
// transient one; Grant refuses it before it touches the transaction.
func TestOrderValidate_ErrInvalidOrder(t *testing.T) {
	for _, o := range []Order{
		{Vault: 1, PinHot: 1, Period: PeriodMonthly},
		{Std: -1, Period: PeriodAnnual},
		{Std: 1, Period: "weekly"},
	} {
		assert.ErrorIs(t, o.Validate(), ErrInvalidOrder, "%+v", o)
		err := Grant(context.Background(), nil, "t", o, time.Now(), ProviderStripe, "sub", RenewalAuto)
		assert.ErrorIs(t, err, ErrInvalidOrder, "%+v", o)
	}
	assert.NoError(t, Order{Std: 1, PinHot: 1, Period: PeriodMonthly}.Validate())
}
