package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/payments"
	dbtestutil "github.com/FairForge/vaultaire/internal/testutil"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-PAY-1: the entitlement_lapse job. Every run is scoped to the fixture's
// own tenants (onlyTenants): the test database is shared, and other packages
// write floor rows of their own.

type lapseFixture struct {
	t      *testing.T
	db     *sql.DB
	qm     *usage.QuotaManager
	fl     *flags.Service
	runner *EntitlementLapseRunner
	mine   map[string]bool
}

type fakeReady map[string]bool

func (f fakeReady) UnreadyWithin(_ context.Context, provider string, _ time.Duration, _ time.Time) bool {
	return f[provider]
}

func setupLapseFixture(t *testing.T) *lapseFixture {
	t.Helper()
	db, err := sql.Open("postgres", dbtestutil.DSN())
	require.NoError(t, err)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Skipf("test database unreachable (%v)", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fl := flags.New(db, zap.NewNop())
	registerFlags(fl)
	require.NoError(t, fl.Refresh(context.Background()))
	f := &lapseFixture{t: t, db: db, qm: usage.NewQuotaManager(db), fl: fl, mine: map[string]bool{}}
	f.runner = NewEntitlementLapseRunner(db, fl, zap.NewNop(), 14*24*time.Hour)
	f.runner.onlyTenants = func(id string) bool { return f.mine[id] }
	return f
}

// tenant creates a tenant of the fixture's own with a free-tier quota row.
func (f *lapseFixture) tenant() string {
	f.t.Helper()
	id := "lapse-" + uuid.New().String()[:12]
	_, err := f.db.Exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'L', $2)`, id, id+"@test.local")
	require.NoError(f.t, err)
	require.NoError(f.t, f.qm.CreateTenant(context.Background(), id, "free", usage.FreeTierLimits.StorageBytes))
	f.mine[id] = true
	f.t.Cleanup(func() {
		_ = f.fl.Unset(context.Background(), flagEntitlementLapse, id)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, id)
		_, _ = f.db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, id)
		_, _ = f.db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, id)
		_, _ = f.db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, id)
		_, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, id)
	})
	return id
}

// granted gives the tenant a house paid through paidThrough.
func (f *lapseFixture) granted(tenantID string, paidThrough time.Time, provider string) {
	f.t.Helper()
	require.NoError(f.t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		return payments.Grant(context.Background(), tx, tenantID, payments.Order{Std: 2, Vault: 1, Period: payments.PeriodMonthly},
			paidThrough, provider, "ref-"+tenantID, payments.RenewalAuto)
	}))
}

func (f *lapseFixture) house(tenantID string) (usage.House, bool) {
	f.t.Helper()
	h, ok, err := f.qm.GetHouse(context.Background(), tenantID)
	require.NoError(f.t, err)
	return h, ok
}

func (f *lapseFixture) entitlement(tenantID string) payments.Entitlement {
	f.t.Helper()
	e, err := payments.Get(context.Background(), f.db, tenantID)
	require.NoError(f.t, err)
	return e
}

func (f *lapseFixture) run() EntitlementLapseResult {
	f.t.Helper()
	res, err := f.runner.RunOnce(context.Background())
	require.NoError(f.t, err)
	return res
}

func TestLapseJob_FlagOff_CountsOnly(t *testing.T) {
	// Arrange: a house paid through 20 days ago (grace 14 d), flag at its
	// default (off).
	f := setupLapseFixture(t)
	id := f.tenant()
	f.granted(id, time.Now().Add(-20*24*time.Hour), payments.ProviderStripe)

	// Act
	res := f.run()

	// Assert: counted, nothing written.
	assert.Equal(t, 1, res.Candidates)
	assert.Equal(t, 1, res.WouldClear)
	assert.Equal(t, 0, res.Cleared)
	_, ok := f.house(id)
	assert.True(t, ok, "the floors are kept while the flag is off")
	assert.Nil(t, f.entitlement(id).LapsedAt)
}

func TestLapseJob_ClearsPastGrace_UsedBytesUntouched(t *testing.T) {
	// Arrange: one tenant past grace, one inside it; the flag on for both.
	f := setupLapseFixture(t)
	past, inside := f.tenant(), f.tenant()
	f.granted(past, time.Now().Add(-15*24*time.Hour), payments.ProviderBTCPay)
	f.granted(inside, time.Now().Add(-5*24*time.Hour), payments.ProviderBTCPay)
	_, err := f.db.Exec(`UPDATE tenant_quotas SET storage_used_bytes = 7777 WHERE tenant_id = $1`, past)
	require.NoError(t, err)
	for _, id := range []string{past, inside} {
		require.NoError(t, f.fl.Set(context.Background(), flagEntitlementLapse, id, true, "test"))
	}

	// Act
	res := f.run()

	// Assert
	assert.Equal(t, 1, res.Candidates)
	assert.Equal(t, 1, res.Cleared)
	assert.Equal(t, 0, res.WouldClear)
	_, ok := f.house(past)
	assert.False(t, ok, "past grace: cleared")
	require.NotNil(t, f.entitlement(past).LapsedAt)
	used, limit, err := f.qm.GetUsage(context.Background(), past)
	require.NoError(t, err)
	assert.Equal(t, int64(7777), used, "billing never deletes data")
	assert.Equal(t, usage.FreeTierLimits.StorageBytes, limit)
	var audited int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'billing.house_lapsed'`, past).Scan(&audited))
	assert.Equal(t, 1, audited)
	_, ok = f.house(inside)
	assert.True(t, ok, "inside grace: kept")

	// A second run finds nothing (lapsed_at is set).
	res = f.run()
	assert.Equal(t, 0, res.Candidates)
}

func TestLapseJob_SkipsUnreadyProvider(t *testing.T) {
	f := setupLapseFixture(t)
	id := f.tenant()
	f.granted(id, time.Now().Add(-30*24*time.Hour), payments.ProviderStripe)
	require.NoError(t, f.fl.Set(context.Background(), flagEntitlementLapse, id, true, "test"))
	f.runner.Ready = fakeReady{payments.ProviderStripe: true}

	res := f.run()

	assert.Equal(t, 1, res.SkippedUnready)
	assert.Equal(t, 0, res.Cleared)
	assert.Contains(t, strings.Join(res.Errors, ";"), "provider_unready")
	_, ok := f.house(id)
	assert.True(t, ok, "a house is never lost because our processor was down")
}

func TestLapseJob_ReconcileHealsFloors(t *testing.T) {
	// Arrange: a granted house whose floors drifted (one deleted, the total
	// edited by hand).
	f := setupLapseFixture(t)
	id := f.tenant()
	f.granted(id, time.Now().Add(20*24*time.Hour), payments.ProviderStripe)
	_, err := f.db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1 AND floor = 'vault'`, id)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE tenant_quotas SET storage_limit_bytes = 1 WHERE tenant_id = $1`, id)
	require.NoError(t, err)

	// Act
	res := f.run()

	// Assert
	assert.Equal(t, 1, res.Healed)
	h, ok := f.house(id)
	require.True(t, ok)
	assert.Equal(t, usage.HouseFromTB(2, 1, 0), h)
	_, limit, err := f.qm.GetUsage(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, 3*usage.TB, limit)

	assert.Equal(t, 0, f.run().Healed, "nothing left to heal")
}

// WP-PAY-7: the pre-097 backfill is gone (ahead of migration 100) — floors with no
// entitlement row (a state prod never had) are neither backfilled nor
// touched; only granted rows are the job's.
func TestLapseJob_FloorsWithoutRow_Untouched(t *testing.T) {
	f := setupLapseFixture(t)
	id := f.tenant()
	require.NoError(t, f.qm.SetHouse(context.Background(), id, usage.HouseFromTB(1, 1, 0)))

	res := f.run()

	_, err := payments.Get(context.Background(), f.db, id)
	assert.ErrorIs(t, err, payments.ErrNoEntitlement, "no row is invented")
	h, ok := f.house(id)
	assert.True(t, ok)
	assert.Equal(t, usage.HouseFromTB(1, 1, 0), h)
	assert.Zero(t, res.Healed)
}

// Through the scheduler: the result lands in job_runs.result.
func TestLapseJob_ThroughTheScheduler_RecordsTheResult(t *testing.T) {
	f := setupLapseFixture(t)
	id := f.tenant()
	f.granted(id, time.Now().Add(-20*24*time.Hour), payments.ProviderStripe)
	jf := setupJobsFixture(t)
	f.runner.JobName = jf.name("entitlement_lapse")
	job := jf.sched.Register(f.runner.spec())
	require.NotNil(t, job)
	assert.True(t, job.spec.daily())
	assert.Equal(t, 7, job.spec.Hour)
	assert.Equal(t, 30, job.spec.Minute)

	_, err := job.RunNow(context.Background())
	require.NoError(t, err)

	var raw []byte
	require.NoError(t, f.db.QueryRow(`SELECT result FROM job_runs WHERE job = $1`, f.runner.JobName).Scan(&raw))
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	for _, k := range []string{"candidates", "would_clear", "cleared", "skipped_unready", "healed"} {
		assert.Contains(t, got, k)
	}
	assert.EqualValues(t, 1, got["would_clear"])
}

// The three series are on /metrics from boot.
func TestEntitlementMetrics_OnTheEndpoint(t *testing.T) {
	f := setupLapseFixture(t)
	s := &Server{db: f.db, logger: zap.NewNop()}
	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, series := range []string{
		`vaultaire_entitlements_expiring{window="7d"}`,
		`vaultaire_entitlements_expiring{window="30d"}`,
		`vaultaire_entitlements_expiring{window="in_grace"}`,
		`vaultaire_entitlements_lapsed_total`,
		`vaultaire_payments_total{kind="charge",outcome="settled",provider="stripe"}`,
	} {
		assert.Contains(t, body, series)
	}
}

// The export carries the entitlement and every ledger row of the tenant,
// paged (WP-PAY-1; TestExportSectionsCoverSchema pins the schema side).
func TestExport_CarriesTheEntitlementAndThePayments(t *testing.T) {
	f := setupLapseFixture(t)
	id := f.tenant()
	f.granted(id, time.Now().Add(10*24*time.Hour), payments.ProviderStripe)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM payments WHERE tenant_id = $1`, id) })
	for i := 0; i < 3; i++ {
		require.NoError(t, payments.InTx(ctx, f.db, func(tx *sql.Tx) error {
			_, err := payments.Fail(ctx, tx, payments.Payment{TenantID: id, Provider: payments.ProviderStripe,
				ProviderID: fmt.Sprintf("in_x_%s_%d", id, i), Kind: payments.KindCharge, Purpose: payments.PurposeRenew, AmountCents: int64(100 + i)})
			return err
		}))
	}

	var buf strings.Builder
	require.NoError(t, renderExport(ctx, f.db, &buf, exportSubject{TenantID: id}, 1))

	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(buf.String()), &doc))
	ent, ok := doc["entitlement"].(map[string]any)
	require.True(t, ok, "entitlement section")
	assert.Equal(t, "stripe", ent["provider"])
	assert.EqualValues(t, 2, ent["standard_tb"])
	pays, ok := doc["payments"].([]any)
	require.True(t, ok, "payments section")
	assert.Len(t, pays, 3, "every row, across pages of one")
}

// WP-PAY-2: the job reads the providers' readiness history from the
// registry (payments.AlwaysReady before): a provider unready 2 h ago — ready
// now — still keeps its tenants' houses for this run.
func TestLapseJob_ReadsProviderReadyHistory(t *testing.T) {
	// Arrange
	f := setupLapseFixture(t)
	id := f.tenant()
	f.granted(id, time.Now().Add(-30*24*time.Hour), payments.ProviderStripe)
	require.NoError(t, f.fl.Set(context.Background(), flagEntitlementLapse, id, true, "test"))
	reg := payments.NewRegistry(f.db, f.fl, 14*24*time.Hour, nil)
	reg.Register(readyProvider{})
	reg.History().Record(payments.ProviderStripe, payments.Unready, time.Now().Add(-2*time.Hour))
	f.runner.Ready = reg

	// Act
	res := f.run()

	// Assert
	assert.Equal(t, 1, res.SkippedUnready)
	assert.Equal(t, 0, res.Cleared)
	_, ok := f.house(id)
	assert.True(t, ok)
}

// readyProvider is a Stripe-named provider that is ready now.
type readyProvider struct{ payments.Provider }

func (readyProvider) Name() string                                       { return payments.ProviderStripe }
func (readyProvider) Ready(context.Context) (payments.Readiness, string) { return payments.Ready, "" }
