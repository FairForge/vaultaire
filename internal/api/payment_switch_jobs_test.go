package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-PAY-5 item 5 (docs/PAYMENTS.md § 4.3): the owner's bulk stop / resume
// of Stripe renewals. Every run is scoped to the fixture's own tenants
// (onlyTenants): the test database is shared.

// bulkStripe is a Stripe provider whose renewal calls are counted (and may
// block, to hold a run open).
type bulkStripe struct {
	payments.Provider
	mu             sync.Mutex
	stops, resumes []string
	hold           chan struct{}
}

func (b *bulkStripe) Name() string                                       { return payments.ProviderStripe }
func (b *bulkStripe) Renewal() payments.Renewal                          { return payments.RenewalAuto }
func (b *bulkStripe) Ready(context.Context) (payments.Readiness, string) { return payments.Ready, "" }
func (b *bulkStripe) StopRenewal(_ context.Context, tenantID string) error {
	if b.hold != nil {
		<-b.hold
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stops = append(b.stops, tenantID)
	return nil
}
func (b *bulkStripe) ResumeRenewal(_ context.Context, tenantID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resumes = append(b.resumes, tenantID)
	return nil
}

type bulkMail struct {
	mu sync.Mutex
	to []string
}

func (s *bulkMail) Send(_ context.Context, to, _, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.to = append(s.to, to)
	return nil
}

type bulkFixture struct {
	t      *testing.T
	db     *sql.DB
	server *Server
	stripe *bulkStripe
	mail   *bulkMail
	mine   map[string]bool
	admin  string
}

func setupBulkFixture(t *testing.T) *bulkFixture {
	t.Helper()
	jf := setupJobsFixture(t)
	f := &bulkFixture{t: t, db: jf.db, stripe: &bulkStripe{}, mail: &bulkMail{}, mine: map[string]bool{}, admin: uuid.New().String()}
	s := &Server{logger: zap.NewNop(), router: chi.NewRouter(), db: f.db, config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	s.payments = payments.NewRegistry(f.db, nil, 0, nil)
	s.payments.Register(f.stripe)
	s.jobs = newJobScheduler(f.db, zap.NewNop())
	s.stripeBulk = NewBulkRenewals(f.db, s.payments, zap.NewNop())
	s.stripeBulk.JobName = jf.name("bulk")
	s.stripeBulk.onlyTenants = func(id string) bool { return f.mine[id] }
	s.stripeBulk.Mailer = payments.NewMailer(f.db, f.mail, "https://stored.test", hosts.LegalEntity{Email: "support@stored.test"}, nil)
	s.jobs.Register(s.stripeBulk.spec())
	s.router.Route("/api/v1/admin", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), userIDKey, f.admin)))
			})
		})
		r.Post("/payments/stripe/stop-renewals", s.handleBulkStopRenewals)
		r.Post("/payments/stripe/resume-renewals", s.handleBulkResumeRenewals)
	})
	f.server = s
	return f
}

// tenant creates a tenant of the fixture's own with a Stripe house.
func (f *bulkFixture) tenant(renewal payments.Renewal, paidThrough time.Time) string {
	f.t.Helper()
	id := "bulk-" + uuid.New().String()[:12]
	_, err := f.db.Exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'B', $2)`, id, id+"@test.local")
	require.NoError(f.t, err)
	require.NoError(f.t, usage.NewQuotaManager(f.db).CreateTenant(context.Background(), id, "free", usage.FreeTierLimits.StorageBytes))
	require.NoError(f.t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		return payments.Grant(context.Background(), tx, id, payments.Order{Std: 1, Period: payments.PeriodMonthly}, paidThrough,
			payments.ProviderStripe, "sub_"+id, renewal)
	}))
	f.mine[id] = true
	f.t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM audit_logs WHERE tenant_id = $1`, `DELETE FROM entitlements WHERE tenant_id = $1`,
			`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, `DELETE FROM tenant_quotas WHERE tenant_id = $1`, `DELETE FROM tenants WHERE id = $1`} {
			_, _ = f.db.Exec(q, id)
		}
	})
	return id
}

func (f *bulkFixture) post(path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	f.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(f.t, json.NewEncoder(&buf).Encode(body))
	}
	w := httptest.NewRecorder()
	f.server.router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/payments/stripe/"+path, &buf))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func (f *bulkFixture) renewal(id string) payments.Renewal {
	f.t.Helper()
	e, err := payments.Get(context.Background(), f.db, id)
	require.NoError(f.t, err)
	return e.Renewal
}

func (f *bulkFixture) audits(id, action string) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = $2 AND metadata->>'reason' = 'bulk'`,
		id, action).Scan(&n))
	return n
}

// wait polls the job's row until the run that started last finished
// (a run started before `since` does not count).
func (f *bulkFixture) wait(since time.Time) jobRunRow {
	f.t.Helper()
	for i := 0; i < 100; i++ {
		rows, err := jobRunRows(context.Background(), f.db)
		require.NoError(f.t, err)
		if r, ok := rows[f.server.stripeBulk.JobName]; ok && r.Outcome != jobOutcomeRunning && r.LastFinished.Valid &&
			!r.LastStarted.Time.Before(since) {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.t.Fatal("the bulk run did not finish")
	return jobRunRow{}
}

func TestBulkStop_DryRun_WritesNothing(t *testing.T) {
	f := setupBulkFixture(t)
	pt := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	auto := f.tenant(payments.RenewalAuto, pt)
	f.tenant(payments.RenewalPrepaid, pt) // not a candidate

	// No body = a dry run.
	w, out := f.post("stop-renewals", nil)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, true, out["dry_run"])
	tenants, _ := out["tenants"].([]any)
	require.Len(t, tenants, 1)
	row := tenants[0].(map[string]any)
	assert.Equal(t, auto, row["tenant_id"])
	assert.Equal(t, pt.Format(time.RFC3339), row["paid_through"])
	assert.Equal(t, payments.RenewalAuto, f.renewal(auto), "nothing written")
	assert.Empty(t, f.stripe.stops, "Stripe never asked")
	assert.Empty(t, f.mail.to)
}

func TestBulkStop_PerTenantAuditAndMail(t *testing.T) {
	f := setupBulkFixture(t)
	a := f.tenant(payments.RenewalAuto, time.Now().Add(20*24*time.Hour))
	b := f.tenant(payments.RenewalAuto, time.Now().Add(5*24*time.Hour))

	since := time.Now().Add(-time.Second)
	w, out := f.post("stop-renewals", map[string]any{"dry_run": false})
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.Equal(t, "started", out["status"])
	r := f.wait(since)

	assert.Equal(t, jobOutcomeOK, r.Outcome, r.Error)
	assert.Equal(t, int64(2), r.Rows)
	for _, id := range []string{a, b} {
		assert.Equal(t, payments.RenewalPrepaid, f.renewal(id))
		assert.Equal(t, 1, f.audits(id, "billing.renewal_stopped"), "one audit row per tenant, reason bulk")
	}
	assert.ElementsMatch(t, []string{a, b}, f.stripe.stops)
	assert.ElementsMatch(t, []string{a + "@test.local", b + "@test.local"}, f.mail.to, "one mail each")
}

func TestBulkResume_OnlyFutureUnpending(t *testing.T) {
	f := setupBulkFixture(t)
	resumes := f.tenant(payments.RenewalAuto, time.Now().Add(20*24*time.Hour))
	past := f.tenant(payments.RenewalAuto, time.Now().Add(20*24*time.Hour))
	pending := f.tenant(payments.RenewalAuto, time.Now().Add(20*24*time.Hour))
	ownStop := f.tenant(payments.RenewalPrepaid, time.Now().Add(20*24*time.Hour)) // stopped by the customer, not the bulk
	_, _ = f.post("stop-renewals", map[string]any{"dry_run": false})
	f.wait(time.Now().Add(-time.Minute))
	_, err := f.db.Exec(`UPDATE entitlements SET paid_through = NOW() - INTERVAL '1 day' WHERE tenant_id = $1`, past)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE entitlements SET pending_provider = 'btcpay' WHERE tenant_id = $1`, pending)
	require.NoError(t, err)

	w, out := f.post("resume-renewals", map[string]any{"dry_run": true})
	require.Equal(t, http.StatusOK, w.Code)
	tenants, _ := out["tenants"].([]any)
	require.Len(t, tenants, 1, fmt.Sprint(out))
	assert.Equal(t, resumes, tenants[0].(map[string]any)["tenant_id"])

	time.Sleep(10 * time.Millisecond)
	since := time.Now()
	w, _ = f.post("resume-renewals", map[string]any{"dry_run": false})
	require.Equal(t, http.StatusAccepted, w.Code)
	f.wait(since)
	assert.Equal(t, []string{resumes}, f.stripe.resumes)
	assert.Equal(t, payments.RenewalAuto, f.renewal(resumes))
	assert.Equal(t, 1, f.audits(resumes, "billing.renewal_resumed"))
	for _, id := range []string{past, pending, ownStop} {
		assert.Equal(t, payments.RenewalPrepaid, f.renewal(id), id)
	}
}

func TestBulk_409WhileRunning(t *testing.T) {
	f := setupBulkFixture(t)
	f.tenant(payments.RenewalAuto, time.Now().Add(20*24*time.Hour))
	f.stripe.hold = make(chan struct{})

	w, _ := f.post("stop-renewals", map[string]any{"dry_run": false})
	require.Equal(t, http.StatusAccepted, w.Code)
	again, out := f.post("resume-renewals", map[string]any{"dry_run": false})
	close(f.stripe.hold)
	f.wait(time.Now().Add(-time.Minute))

	assert.Equal(t, http.StatusConflict, again.Code)
	assert.Equal(t, "already_running", out["error"])
}
