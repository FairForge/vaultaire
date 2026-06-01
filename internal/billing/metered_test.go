package billing

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeMeterSender records every meter event instead of calling Stripe.
type fakeMeterSender struct {
	calls []meterCall
	err   error
}

type meterCall struct {
	eventName  string
	customerID string
	value      int64
	identifier string
}

func (f *fakeMeterSender) send(_ context.Context, eventName, customerID string, value int64, identifier string, _ time.Time) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.calls = append(f.calls, meterCall{eventName, customerID, value, identifier})
	return "evt_" + identifier, nil
}

func TestReportDaily_MeteredTiersOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	fake := &fakeMeterSender{}
	r := &MeteredReporter{
		stripe: &StripeService{}, db: db, logger: zap.NewNop(),
		storageMeter: "storage_bytes", egressMeter: "egress_bytes", sender: fake,
	}

	// Only standard/performance tenants come back from the query (the WHERE clause
	// filters vault*/free server-side), so the mock returns exactly those.
	mock.ExpectQuery(`SELECT tq.tenant_id, tq.storage_used_bytes`).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "storage_used_bytes", "stripe_customer_id"}).
			AddRow("t-std", int64(2*1024*1024*1024*1024), "cus_std").
			AddRow("t-perf", int64(1024*1024*1024*1024), "cus_perf"))

	// t-std: storage existence check (none) → INSERT; egress sum → existence → INSERT.
	expectReport(mock, "t-std", "storage_bytes")
	expectEgressSum(mock, "t-std", 0)
	expectReport(mock, "t-std", "egress_bytes")
	// t-perf: same shape.
	expectReport(mock, "t-perf", "storage_bytes")
	expectEgressSum(mock, "t-perf", 0)
	expectReport(mock, "t-perf", "egress_bytes")

	date := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)
	require.NoError(t, r.ReportDaily(context.Background(), date))
	require.NoError(t, mock.ExpectationsWereMet())

	// Two tenants × two meters = four events; all metered tiers, none skipped.
	require.Len(t, fake.calls, 4)
	names := map[string]int{}
	for _, c := range fake.calls {
		names[c.eventName]++
	}
	assert.Equal(t, 2, names["storage_bytes"])
	assert.Equal(t, 2, names["egress_bytes"])
}

func TestReportDaily_Idempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	fake := &fakeMeterSender{}
	r := &MeteredReporter{
		stripe: &StripeService{}, db: db, logger: zap.NewNop(),
		storageMeter: "storage_bytes", egressMeter: "egress_bytes", sender: fake,
	}
	date := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)

	// First run: no existing rows → sends + inserts both meters.
	mock.ExpectQuery(`SELECT tq.tenant_id, tq.storage_used_bytes`).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "storage_used_bytes", "stripe_customer_id"}).
			AddRow("t1", int64(1024*1024*1024*1024), "cus_1"))
	expectReport(mock, "t1", "storage_bytes")
	expectEgressSum(mock, "t1", 0)
	expectReport(mock, "t1", "egress_bytes")
	require.NoError(t, r.ReportDaily(context.Background(), date))

	// Second run for the same date: existence checks find rows → no send, no insert.
	mock.ExpectQuery(`SELECT tq.tenant_id, tq.storage_used_bytes`).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "storage_used_bytes", "stripe_customer_id"}).
			AddRow("t1", int64(1024*1024*1024*1024), "cus_1"))
	expectReportExists(mock, "t1", "storage_bytes")
	expectEgressSum(mock, "t1", 0)
	expectReportExists(mock, "t1", "egress_bytes")
	require.NoError(t, r.ReportDaily(context.Background(), date))

	require.NoError(t, mock.ExpectationsWereMet())
	// Two sends from the first run only; the re-run sent nothing.
	assert.Len(t, fake.calls, 2)
}

func TestReportDaily_SkipsNoStripeCustomer(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	fake := &fakeMeterSender{}
	r := &MeteredReporter{
		stripe: &StripeService{}, db: db, logger: zap.NewNop(),
		storageMeter: "storage_bytes", egressMeter: "egress_bytes", sender: fake,
	}

	mock.ExpectQuery(`SELECT tq.tenant_id, tq.storage_used_bytes`).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "storage_used_bytes", "stripe_customer_id"}).
			AddRow("t-nocust", int64(1024*1024*1024*1024), ""))
	// No per-tenant queries expected: the tenant is skipped before any meter call.

	date := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)
	require.NoError(t, r.ReportDaily(context.Background(), date))
	require.NoError(t, mock.ExpectationsWereMet())
	assert.Empty(t, fake.calls)
}

func TestReportDaily_StorageAndEgress(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	fake := &fakeMeterSender{}
	r := &MeteredReporter{
		stripe: &StripeService{}, db: db, logger: zap.NewNop(),
		storageMeter: "storage_bytes", egressMeter: "egress_bytes", sender: fake,
	}

	const storage = int64(3 * 1024 * 1024 * 1024 * 1024)
	const egress = int64(500 * 1024 * 1024 * 1024)

	mock.ExpectQuery(`SELECT tq.tenant_id, tq.storage_used_bytes`).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "storage_used_bytes", "stripe_customer_id"}).
			AddRow("t1", storage, "cus_1"))
	expectReport(mock, "t1", "storage_bytes")
	expectEgressSum(mock, "t1", egress)
	expectReport(mock, "t1", "egress_bytes")

	date := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)
	require.NoError(t, r.ReportDaily(context.Background(), date))
	require.NoError(t, mock.ExpectationsWereMet())

	require.Len(t, fake.calls, 2)
	byMeter := map[string]meterCall{}
	for _, c := range fake.calls {
		byMeter[c.eventName] = c
	}
	assert.Equal(t, storage, byMeter["storage_bytes"].value)
	assert.Equal(t, egress, byMeter["egress_bytes"].value)
	assert.Equal(t, "cus_1", byMeter["storage_bytes"].customerID)
	assert.Equal(t, "t1-storage_bytes-2026-05-30", byMeter["storage_bytes"].identifier)
}

func TestReportDaily_StripeFailureLeavesNoRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Sender errors → reportMeter must NOT insert a row (so the next run retries).
	fake := &fakeMeterSender{err: assertErr("stripe down")}
	r := &MeteredReporter{
		stripe: &StripeService{}, db: db, logger: zap.NewNop(),
		storageMeter: "storage_bytes", egressMeter: "egress_bytes", sender: fake,
	}

	mock.ExpectQuery(`SELECT tq.tenant_id, tq.storage_used_bytes`).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "storage_used_bytes", "stripe_customer_id"}).
			AddRow("t1", int64(1024*1024*1024*1024), "cus_1"))
	// storage: existence check (none), then NO insert (send failed).
	mock.ExpectQuery(`SELECT 1 FROM metered_usage_reports`).
		WithArgs("t1", "storage_bytes", "2026-05-30").
		WillReturnError(sqlNoRows())
	expectEgressSum(mock, "t1", 0)
	// egress: existence check (none), then NO insert (send failed).
	mock.ExpectQuery(`SELECT 1 FROM metered_usage_reports`).
		WithArgs("t1", "egress_bytes", "2026-05-30").
		WillReturnError(sqlNoRows())

	date := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)
	require.NoError(t, r.ReportDaily(context.Background(), date))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSpendingCap_AlertAt80And95(t *testing.T) {
	capCents := int64(10000) // $100.00 cap

	cases := []struct {
		name         string
		accruedCents int64
		wantAlerts   []string // labels expected to be inserted (and thus emit)
	}{
		{"below 80%", 5000, nil},
		{"at 85%", 8500, []string{"alert:80"}},
		{"at 96%", 9600, []string{"alert:80", "alert:95"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()

			r := &MeteredReporter{
				stripe: &StripeService{}, db: db, logger: zap.NewNop(),
				storageMeter: "storage_bytes", egressMeter: "egress_bytes",
				// no emailer: cap alert records an event but sends no email.
			}

			// One performance tenant with a cap.
			mock.ExpectQuery(`SELECT tenant_id, tier, spending_cap_cents`).
				WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "tier", "spending_cap_cents"}).
					AddRow("t1", "performance", capCents))

			// accrued: derive storage bytes that map to tc.accruedCents at $6/TB.
			storageBytes := centsToStorageBytes(tc.accruedCents, pricePerTBPerformance)
			mock.ExpectQuery(`SELECT\s+COALESCE\(\(SELECT value FROM metered_usage_reports`).
				WillReturnRows(sqlmock.NewRows([]string{"storage", "egress"}).
					AddRow(storageBytes, int64(0)))

			for _, label := range tc.wantAlerts {
				// guard insert returns 1 row → newly alerted → event insert follows.
				mock.ExpectExec(`INSERT INTO metered_usage_reports`).
					WithArgs("t1", label, sqlmock.AnyArg(), sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(1, 1))
				mock.ExpectExec(`INSERT INTO events`).
					WillReturnResult(sqlmock.NewResult(0, 1))
			}

			r.checkSpendingCaps(context.Background())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestSpendingCap_GuardSuppressesRepeat(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	r := &MeteredReporter{
		stripe: &StripeService{}, db: db, logger: zap.NewNop(),
		storageMeter: "storage_bytes", egressMeter: "egress_bytes",
	}

	mock.ExpectQuery(`SELECT tenant_id, tier, spending_cap_cents`).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "tier", "spending_cap_cents"}).
			AddRow("t1", "standard", int64(10000)))
	mock.ExpectQuery(`SELECT\s+COALESCE\(\(SELECT value FROM metered_usage_reports`).
		WillReturnRows(sqlmock.NewRows([]string{"storage", "egress"}).
			AddRow(centsToStorageBytes(8500, pricePerTBStandard), int64(0)))

	// 80% crossed, but the guard insert affects 0 rows (already alerted) → NO event.
	mock.ExpectExec(`INSERT INTO metered_usage_reports`).
		WithArgs("t1", "alert:80", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	r.checkSpendingCaps(context.Background())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestStartMeteredReporting_NilSafe(t *testing.T) {
	// Nil stripe + nil db must not panic and must not start a goroutine that touches them.
	require.NotPanics(t, func() {
		r := NewMeteredReporter(nil, nil, zap.NewNop(), "storage_bytes", "egress_bytes")
		r.StartMeteredReporting(context.Background())
	})
	// A nil receiver is also tolerated.
	require.NotPanics(t, func() {
		var r *MeteredReporter
		r.StartMeteredReporting(context.Background())
	})
}

func TestAccruedCents(t *testing.T) {
	tb := int64(1024 * 1024 * 1024 * 1024)
	assert.Equal(t, int64(399), AccruedCents("standard", tb, 0))    // $3.99
	assert.Equal(t, int64(600), AccruedCents("performance", tb, 0)) // $6.00
	assert.Equal(t, int64(0), AccruedCents("vault3", tb, 0))        // fixed-price tier
	assert.Equal(t, int64(0), AccruedCents("free", tb, 0))
	assert.Equal(t, int64(1197), AccruedCents("standard", 3*tb, 0)) // $11.97
}

// --- helpers ---

func expectReport(mock sqlmock.Sqlmock, tenant, meter string) {
	mock.ExpectQuery(`SELECT 1 FROM metered_usage_reports`).
		WithArgs(tenant, meter, sqlmock.AnyArg()).
		WillReturnError(sqlNoRows())
	mock.ExpectExec(`INSERT INTO metered_usage_reports`).
		WithArgs(tenant, meter, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
}

func expectReportExists(mock sqlmock.Sqlmock, tenant, meter string) {
	mock.ExpectQuery(`SELECT 1 FROM metered_usage_reports`).
		WithArgs(tenant, meter, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
}

func expectEgressSum(mock sqlmock.Sqlmock, tenant string, egress int64) {
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(egress_bytes\), 0\)`).
		WithArgs(tenant, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(egress))
}

func centsToStorageBytes(cents int64, ratePerTB float64) int64 {
	tb := float64(cents) / 100 / ratePerTB
	return int64(tb * bytesPerTB)
}

func sqlNoRows() error { return sql.ErrNoRows }

type assertErr string

func (e assertErr) Error() string { return string(e) }
