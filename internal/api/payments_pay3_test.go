package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-PAY-3 (Stage 1a Prompt 20): the payment_reminders job, the house
// lapsed mail, the admin money routes, the metrics and the rule file.

// recordingSender is an email.Sender that keeps what it was given.
type recordingSender struct {
	mu   sync.Mutex
	sent []sentMail
	err  error
}

type sentMail struct{ to, subject, html string }

func (s *recordingSender) Send(_ context.Context, to, subject, html, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, sentMail{to, subject, html})
	return nil
}

func (s *recordingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (f *lapseFixture) prepaid(tenantID string, paidThrough time.Time) {
	f.t.Helper()
	require.NoError(f.t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		return payments.Grant(context.Background(), tx, tenantID, payments.Order{Std: 1, Period: payments.PeriodAnnual},
			paidThrough, payments.ProviderManual, "", payments.RenewalPrepaid)
	}))
}

func (f *lapseFixture) reminders(sender *recordingSender, now func() time.Time) *PaymentRemindersRunner {
	f.t.Helper()
	m := payments.NewMailer(f.db, sender, "https://stored.test", hosts.Defaults(8000).Legal(), zap.NewNop())
	r := NewPaymentRemindersRunner(f.db, zap.NewNop(), 14*24*time.Hour, m, f.fl)
	r.onlyTenants = func(id string) bool { return f.mine[id] }
	r.JobName = "test_payment_reminders_" + uuid.New().String()[:8]
	if now != nil {
		r.now = now
	}
	return r
}

func TestReminders_EachStepOnce(t *testing.T) {
	// Arrange: a prepaid house paid through 13 days from now.
	f := setupLapseFixture(t)
	id := f.tenant("")
	pt := time.Now().Add(13 * 24 * time.Hour).Truncate(time.Second)
	f.prepaid(id, pt)
	// The grace steps go out only where the lapse job would act (RP24-2).
	require.NoError(t, f.fl.Set(context.Background(), flagEntitlementLapse, id, true, "test"))
	sender := &recordingSender{}
	clock := time.Now()
	r := f.reminders(sender, func() time.Time { return clock })

	// Act + Assert: t-14d once.
	res, err := r.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Sent)
	require.Equal(t, 1, sender.count())
	assert.Equal(t, id+"@test.local", sender.sent[0].to)
	assert.Contains(t, sender.sent[0].subject, "13 more days")
	res, err = r.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, res.Sent, "once per period")
	assert.Equal(t, 1, sender.count())

	// t-3d, in grace, last day: one each.
	for _, at := range []time.Time{pt.Add(-2 * 24 * time.Hour), pt.Add(36 * time.Hour), pt.Add(14*24*time.Hour - 6*time.Hour)} {
		clock = at
		res, err = r.RunOnce(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 1, res.Sent, "at %s", at)
		res, err = r.RunOnce(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 0, res.Sent)
	}
	assert.Equal(t, 4, sender.count())
	assert.Contains(t, sender.sent[2].subject, "grace period")
	assert.Contains(t, sender.sent[3].subject, "Last day")
	var sent []byte
	require.NoError(t, f.db.QueryRow(`SELECT reminders_sent FROM entitlements WHERE tenant_id = $1`, id).Scan(&sent))
	var m map[string]string
	require.NoError(t, json.Unmarshal(sent, &m))
	assert.Len(t, m, 4)
	assert.Contains(t, m, payments.ReminderKey(payments.StepBefore14d, pt))
}

func TestReminders_PrepaidOnly(t *testing.T) {
	f := setupLapseFixture(t)
	id := f.tenant("sub_auto")
	f.granted(id, time.Now().Add(10*24*time.Hour), payments.ProviderStripe) // renewal auto
	sender := &recordingSender{}
	r := f.reminders(sender, nil)

	res, err := r.RunOnce(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 0, res.Candidates)
	assert.Zero(t, sender.count(), "a house a provider renews gets no reminder")
}

func TestReminders_NotAfterLapse(t *testing.T) {
	f := setupLapseFixture(t)
	id := f.tenant("")
	f.prepaid(id, time.Now().Add(10*24*time.Hour))
	_, err := f.db.Exec(`UPDATE entitlements SET lapsed_at = NOW() WHERE tenant_id = $1`, id)
	require.NoError(t, err)
	sender := &recordingSender{}

	res, err := f.reminders(sender, nil).RunOnce(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 0, res.Candidates)
	assert.Zero(t, sender.count())
}

func TestReminders_NewPeriodResets(t *testing.T) {
	f := setupLapseFixture(t)
	id := f.tenant("")
	pt := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	f.prepaid(id, pt)
	sender := &recordingSender{}
	clock := time.Now()
	r := f.reminders(sender, func() time.Time { return clock })
	res, err := r.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.Sent)

	// Renewed: a year on. 14 days before the new date the step is due again.
	require.NoError(t, payments.InTx(context.Background(), f.db, func(tx *sql.Tx) error {
		_, err := payments.Extend(context.Background(), tx, id, payments.PeriodAnnual)
		return err
	}))
	e := f.entitlement(id)
	clock = e.PaidThrough.Add(-10 * 24 * time.Hour)
	res, err = r.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Sent, "the new period's t-14d")
	assert.Equal(t, 2, sender.count())
}

// RP24-2: the grace steps ("in grace, N days left", "your house ends
// tomorrow") announce an ending only the lapse job performs, so they go out
// only where entitlement_lapse is ON for the tenant; the before-steps go
// regardless. A withheld step is counted, not marked sent.
func TestReminders_GraceStepsOnlyWhereTheLapseJobWouldAct(t *testing.T) {
	f := setupLapseFixture(t)
	flagOff := f.tenant("")
	flagOn := f.tenant("")
	pt := time.Now().Add(-36 * time.Hour).Truncate(time.Second) // the second day of grace: t+1d is due
	f.prepaid(flagOff, pt)
	f.prepaid(flagOn, pt)
	require.NoError(t, f.fl.Set(context.Background(), flagEntitlementLapse, flagOn, true, "test"))
	sender := &recordingSender{}
	r := f.reminders(sender, nil)

	res, err := r.RunOnce(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 2, res.Candidates)
	assert.Equal(t, 1, res.Sent, "the flag-on tenant's t+1d")
	assert.Equal(t, 1, res.SkippedFlagOff, "the flag-off tenant's is withheld, a note")
	require.Equal(t, 1, sender.count())
	assert.Equal(t, flagOn+"@test.local", sender.sent[0].to)

	// Not marked: the flag turning on inside the window sends it.
	require.NoError(t, f.fl.Set(context.Background(), flagEntitlementLapse, flagOff, true, "test"))
	res, err = r.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Sent)
	assert.Equal(t, 0, res.SkippedFlagOff)

	// A before-step goes out whatever the flag: a third tenant, flag off,
	// 10 days ahead.
	before := f.tenant("")
	f.prepaid(before, time.Now().Add(10*24*time.Hour))
	res, err = r.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Sent, "t-14d is true either way")
	assert.Equal(t, 0, res.SkippedFlagOff)
}

// A send that fails is counted, noted on an ok run, and retried next hour.
func TestReminders_FailedSendIsRetried(t *testing.T) {
	f := setupLapseFixture(t)
	id := f.tenant("")
	f.prepaid(id, time.Now().Add(10*24*time.Hour))
	sender := &recordingSender{err: fmt.Errorf("smtp down")}
	r := f.reminders(sender, nil)

	res, err := r.RunOnce(context.Background())
	require.NoError(t, err, "one mail that failed is a note, not a failed run")
	assert.Equal(t, 1, res.Failed)
	assert.Len(t, res.Errors, 1)
	sender.err = nil
	res, err = r.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Sent)
}

// The job is registered with the scheduler (hourly) and its series are
// on /metrics at 0 from boot — every (step, outcome) pair.
func TestRemindersJob_RegisteredAndMetricPrecreated(t *testing.T) {
	jf := setupJobsFixture(t)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	s := NewServer(&config.Config{Server: config.ServerConfig{Port: 8000}}, zap.NewNop(), eng, nil, jf.db)
	j := s.jobs.job(paymentRemindersJobName)
	require.NotNil(t, j)
	assert.Equal(t, time.Hour, j.spec.Every)
	assert.Equal(t, 5*time.Minute, j.spec.MaxRunTime)

	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, step := range payments.ReminderSteps {
		for _, outcome := range []string{"sent", "failed"} {
			series := fmt.Sprintf(`vaultaire_payment_reminders_total{outcome="%s",step="%s"}`, outcome, step)
			assert.True(t, strings.Contains(body, "\n"+series+" "), "series exists from boot: %s", series)
		}
	}
	for _, series := range []string{
		`vaultaire_payment_provider_enabled{provider="stripe"} 0`,
		`vaultaire_payment_webhook_total{outcome="error",provider="stripe"} `,
		`vaultaire_stripe_house_invalid_total `,
	} {
		assert.True(t, strings.Contains(body, "\n"+series), "exported from boot: %s", series)
	}
}

// The lapse job mails the customer after its Revoke committed.
func TestLapseJob_SendsHouseLapsedMail(t *testing.T) {
	f := setupLapseFixture(t)
	id := f.tenant("")
	f.prepaid(id, time.Now().Add(-30*24*time.Hour))
	require.NoError(t, f.fl.Set(context.Background(), flagEntitlementLapse, id, true, "test"))
	sender := &recordingSender{}
	f.runner.Mailer = payments.NewMailer(f.db, sender, "https://stored.test", hosts.Defaults(8000).Legal(), zap.NewNop())

	res := f.run()

	assert.Equal(t, 1, res.Cleared)
	require.Equal(t, 1, sender.count())
	assert.Equal(t, id+"@test.local", sender.sent[0].to)
	assert.Contains(t, sender.sent[0].subject, "has ended")
	assert.Contains(t, sender.sent[0].html, "https://stored.test/dashboard/billing")
}

// vaultaire_entitlement_lapse_skipped_unready is read from job_runs.result.
func TestLapseSkippedUnreadyGauge_FromJobRuns(t *testing.T) {
	jf := setupJobsFixture(t)
	name := "test_lapse_gauge_" + uuid.New().String()[:8]
	_, err := jf.db.Exec(`INSERT INTO job_runs (job, last_started_at, last_finished_at, last_outcome, result)
	                      VALUES ($1, NOW(), NOW(), 'ok', '{"skipped_unready": 2, "cleared": 0}')`, name)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = jf.db.Exec(`DELETE FROM job_runs WHERE job = $1`, name) })

	c := newLapseSkippedCollector(jf.db, name, zap.NewNop())
	v, ok := c.read()
	assert.True(t, ok)
	assert.Equal(t, 2.0, v)
	none := newLapseSkippedCollector(jf.db, name+"-none", zap.NewNop())
	_, ok = none.read()
	assert.False(t, ok, "no run yet: no series")
}

// --- the rule file ---

// The payments rule file parses, names the five rules, reads series the
// server exports from boot, and passes promtool when it is on PATH.
func TestPaymentsRules_Parse(t *testing.T) {
	file, raw := readRuleFile(t, "vaultaire-payments.yml")
	require.Len(t, file.Groups, 1)
	rules := map[string]struct{ expr, severity string }{}
	for _, r := range file.Groups[0].Rules {
		rules[r.Alert] = struct{ expr, severity string }{r.Expr, r.Labels["severity"]}
	}
	for name, sev := range map[string]string{"PaymentProviderDown": "critical", "PaymentWebhookFailing": "warning",
		"EntitlementLapseSkippedUnready": "warning", "StripeHouseInvalid": "warning", "PaymentRemindersFailing": "warning"} {
		require.Contains(t, rules, name)
		assert.Equal(t, sev, rules[name].severity, name)
	}
	assert.Contains(t, rules["PaymentProviderDown"].expr, "vaultaire_payment_provider_enabled == 1")
	assert.Contains(t, rules["PaymentWebhookFailing"].expr, `vaultaire_payment_webhook_total{outcome="error"}`)
	assert.Contains(t, rules["EntitlementLapseSkippedUnready"].expr, "min_over_time(vaultaire_entitlement_lapse_skipped_unready[30h]) > 0")
	assert.Contains(t, rules["StripeHouseInvalid"].expr, "increase(vaultaire_stripe_house_invalid_total[1h]) > 0")
	assert.Contains(t, rules["PaymentRemindersFailing"].expr, `vaultaire_payment_reminders_total{outcome="failed"}`)
	for _, r := range file.Groups[0].Rules {
		assert.False(t, regexp.MustCompile(`[{,]\s*job\s*=`).MatchString(r.Expr), "%s selects on Prometheus's own job label", r.Alert)
	}
	for _, series := range []string{"vaultaire_payment_provider_ready", "vaultaire_payment_provider_enabled", "vaultaire_payment_webhook_total",
		"vaultaire_entitlement_lapse_skipped_unready", "vaultaire_stripe_house_invalid_total", "vaultaire_payment_reminders_total"} {
		assert.True(t, strings.Contains(raw, series), "the rule file documents %s", series)
	}
	// The jobs rule watches the new interval job.
	jobs, _ := readRuleFile(t, "vaultaire-jobs.yml")
	for _, r := range jobs.Groups[0].Rules {
		if r.Alert == "PeriodicJobStale" {
			assert.Contains(t, r.Expr, "payment_reminders")
		}
	}

	promtool, err := exec.LookPath("promtool")
	if err != nil {
		t.Log("promtool not on PATH: the unit test runs on the box (deploy/monitoring/tests/vaultaire-payments_test.yml)")
		return
	}
	dir := t.TempDir()
	for _, f := range []string{"vaultaire-payments.yml", "tests/vaultaire-payments_test.yml"} {
		b, err := os.ReadFile("../../deploy/monitoring/" + f)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, filepath.Base(f)), b, 0o600))
	}
	out, err := exec.Command(promtool, "check", "rules", filepath.Join(dir, "vaultaire-payments.yml")).CombinedOutput()
	require.NoError(t, err, string(out))
	cmd := exec.Command(promtool, "test", "rules", "vaultaire-payments_test.yml")
	cmd.Dir = dir
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}
