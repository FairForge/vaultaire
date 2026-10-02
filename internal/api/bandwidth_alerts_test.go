package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The warning ladder of the egress allowance (WP-R10-9, part D). DB-backed,
// one tenant per test (egressFixture); the alerter reads the same allowance
// and the same month counter as the throttle.

type fakeBandwidthEmailSender struct {
	mu   sync.Mutex
	sent []bwEmail
	err  error
}

type bwEmail struct {
	to, subject, html, text string
}

func (f *fakeBandwidthEmailSender) Send(_ context.Context, to, subject, html, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, bwEmail{to, subject, html, text})
	return nil
}

func (f *fakeBandwidthEmailSender) emails() []bwEmail {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]bwEmail, len(f.sent))
	copy(cp, f.sent)
	return cp
}

// alerter is the ladder wired like the server wires it: the fixture's live
// counter is its reader. Tests call checkTenantAlerts for their own tenant —
// the hourly pass walks every tenant on the shared database.
func (f *egressFixture) alerter(sender *fakeBandwidthEmailSender) *BandwidthAlerter {
	a := NewBandwidthAlerter(f.db, zap.NewNop())
	a.SetEmailSender(sender)
	a.SetEgressReader(f.meter)
	return a
}

// pctOf is pct % of n, rounded up.
func pctOf(n int64, pct int64) int64 { return (n*pct + 99) / 100 }

func (f *egressFixture) alertRows() map[int]bool {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT threshold_pct, last_fired_at IS NOT NULL FROM bandwidth_alerts WHERE tenant_id = $1`, f.tenantID)
	require.NoError(f.t, err)
	defer func() { _ = rows.Close() }()
	out := map[int]bool{}
	for rows.Next() {
		var pct int
		var fired bool
		require.NoError(f.t, rows.Scan(&pct, &fired))
		out[pct] = fired
	}
	require.NoError(f.t, rows.Err())
	return out
}

// Review R13-16: the throttle counted ingress + egress, the alerter egress
// only — two definitions of "used". Now both read one counter, and uploads
// never count.
func TestEgressAlerts_AgreeWithTheThrottleOnUsed(t *testing.T) {
	// Arrange: 64 MiB of quota → 32 MiB of egress. The tenant has uploaded
	// three times that and downloaded nothing.
	f := setupEgressFixture(t, 64*mib, testThrottle(), nil)
	f.enforced.Store(true)
	sender := &fakeBandwidthEmailSender{}
	a := f.alerter(sender)
	ctx := context.Background()
	f.put("obj.bin", 8*mib)
	f.srv.bandwidthTracker.recordOn(time.Now(), f.tenantID, "local", 96*mib, 0)
	f.srv.bandwidthTracker.Flush()

	// Act 1
	a.checkTenantAlerts(ctx, f.tenantID)
	st, err := f.meter.EgressStatus(ctx, f.tenantID)
	require.NoError(t, err)
	fast := f.do(http.MethodGet, f.s3Path("obj.bin"), nil)

	// Assert 1: 104 MiB of ingress is not egress — no notice, no throttle.
	assert.Empty(t, sender.emails())
	assert.False(t, st.Over)
	assert.Equal(t, int64(0), st.UsedBytes)
	assert.Less(t, fast.elapsed, egressFastBound)

	// Act 2: three more downloads (32 MiB in all) — still in flight as far
	// as the database is concerned: nothing was flushed.
	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusOK, f.do(http.MethodGet, f.s3Path("obj.bin"), nil).status)
	}
	a.checkTenantAlerts(ctx, f.tenantID)
	st, err = f.meter.EgressStatus(ctx, f.tenantID)
	require.NoError(t, err)
	tn := f.meter.tenant(ctx, f.tenantID)
	d := f.meter.decide(ctx, tn)

	// Assert 2: the alerter saw the unflushed bytes the throttle sees, and
	// they agree to the byte.
	assert.Equal(t, 32*mib, st.UsedBytes)
	assert.Equal(t, d.used, st.UsedBytes)
	assert.Equal(t, d.over, st.Over)
	assert.True(t, st.Throttled)
	emails := sender.emails()
	require.Len(t, emails, 3, "80 %, 95 % and the 100 % notice")
	for _, e := range emails {
		assert.Equal(t, "egress-"+f.tenantID[:8]+"@test.local", e.to)
		assert.Contains(t, e.text, "egress allowance")
		assert.NotContains(t, e.subject+e.text, "bandwidth limit")
	}
}

func TestEgressAlerts_Ladder(t *testing.T) {
	// Arrange: a 32 MiB allowance.
	f := setupEgressFixture(t, 64*mib, testThrottle(), nil)
	sender := &fakeBandwidthEmailSender{}
	a := f.alerter(sender)
	ctx := context.Background()
	reset := usage.EgressResetAt(time.Now()).Format("January 2, 2006")

	// Act 1: 79 % — nothing, and no row is created for a step not crossed.
	f.setUsed(pctOf(32*mib, 79) - 2)
	a.checkTenantAlerts(ctx, f.tenantID)

	// Assert 1
	assert.Empty(t, sender.emails())
	assert.Empty(t, f.alertRows(), "rows are created when a step is crossed, never seeded")

	// Act 2: 85 %.
	f.setUsed(pctOf(32*mib, 85))
	a.checkTenantAlerts(ctx, f.tenantID)

	// Assert 2
	emails := sender.emails()
	require.Len(t, emails, 1)
	assert.Equal(t, "You've used 85% of your stored.ge egress allowance", emails[0].subject)
	assert.Contains(t, emails[0].text, "27.2 MB of your 32.0 MB egress allowance")
	assert.Contains(t, emails[0].text, "4.0 MB/s")
	assert.Contains(t, emails[0].text, reset)
	assert.Equal(t, map[int]bool{80: true}, f.alertRows())

	// Act 3: 120 % with the throttle flag off for this tenant.
	f.setUsed(pctOf(32*mib, 120))
	a.checkTenantAlerts(ctx, f.tenantID)

	// Assert 3: the 95 % warning goes out; the 100 % notice says downloads
	// are rate-limited, which is not true yet, so it waits unfired.
	emails = sender.emails()
	require.Len(t, emails, 2)
	assert.Contains(t, emails[1].subject, "120%")
	assert.Equal(t, map[int]bool{80: true, 95: true, 100: false}, f.alertRows())

	// Act 4: the flag is flipped.
	f.enforced.Store(true)
	a.checkTenantAlerts(ctx, f.tenantID)

	// Assert 4: the notice states the rate and the reset date.
	emails = sender.emails()
	require.Len(t, emails, 3)
	assert.Equal(t, "Your stored.ge egress allowance for this month is used up", emails[2].subject)
	assert.Contains(t, emails[2].text, "rate-limited to 4.0 MB/s in total")
	assert.Contains(t, emails[2].text, "resets on "+reset+" (UTC)")
	assert.Contains(t, emails[2].text, "Nothing is billed")
	assert.NotContains(t, emails[2].text, "never refused")
	assert.Equal(t, map[int]bool{80: true, 95: true, 100: true}, f.alertRows())

	// Act 5: the next hourly passes.
	a.checkTenantAlerts(ctx, f.tenantID)
	a.checkTenantAlerts(ctx, f.tenantID)

	// Assert 5: once per step per month.
	assert.Len(t, sender.emails(), 3)

	// Act 6: last month's stamps do not silence this month.
	_, err := f.db.Exec(`UPDATE bandwidth_alerts SET last_fired_at = $2 WHERE tenant_id = $1`,
		f.tenantID, usage.EgressMonthStart(time.Now()).Add(-time.Hour))
	require.NoError(t, err)
	a.checkTenantAlerts(ctx, f.tenantID)

	// Assert 6
	assert.Len(t, sender.emails(), 6)
}

func TestEgressAlerts_DisabledRowAndAdminOverride(t *testing.T) {
	// Arrange: an admin override of 10 MiB replaces the plan's 32 MiB.
	f := setupEgressFixture(t, 64*mib, testThrottle(), nil)
	sender := &fakeBandwidthEmailSender{}
	a := f.alerter(sender)
	ctx := context.Background()
	_, err := f.db.Exec(`UPDATE tenant_quotas SET bandwidth_limit_bytes = $2 WHERE tenant_id = $1`, f.tenantID, 10*mib)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO bandwidth_alerts (tenant_id, threshold_pct, alert_type, enabled) VALUES ($1, 80, 'email', false)`, f.tenantID)
	require.NoError(t, err)
	f.setUsed(pctOf(10*mib, 97)) // 97 % of the override, 30 % of the plan

	// Act
	a.checkTenantAlerts(ctx, f.tenantID)

	// Assert: measured against the override; the silenced step stays silent.
	emails := sender.emails()
	require.Len(t, emails, 1)
	assert.Contains(t, emails[0].subject, "97%")
	assert.Contains(t, emails[0].text, "10.0 MB egress allowance")
	assert.Equal(t, map[int]bool{80: false, 95: true}, f.alertRows())
}

// The R13-09 behaviour is kept: a failed send is not stamped, the next pass
// retries.
func TestEgressAlerts_FailedSendIsRetriedNextPass(t *testing.T) {
	f := setupEgressFixture(t, 64*mib, testThrottle(), nil)
	sender := &fakeBandwidthEmailSender{err: errors.New("provider down")}
	a := f.alerter(sender)
	ctx := context.Background()
	f.setUsed(pctOf(32*mib, 90))

	// Act 1: the provider is down.
	a.checkTenantAlerts(ctx, f.tenantID)

	// Assert 1
	assert.Empty(t, sender.emails())
	assert.Equal(t, map[int]bool{80: false}, f.alertRows(), "last_fired_at must not be written after a failed send")

	// Act 2: the provider is back.
	sender.mu.Lock()
	sender.err = nil
	sender.mu.Unlock()
	a.checkTenantAlerts(ctx, f.tenantID)

	// Assert 2
	assert.Len(t, sender.emails(), 1)
	assert.Equal(t, map[int]bool{80: true}, f.alertRows())
}

// The hourly pass picks its tenants with one set-based query: those with
// egress recorded this UTC month. (The test reads the candidate list and
// runs the check for its own tenants only — the full pass would write alert
// rows for every other test's tenant on the shared database.)
func TestEgressAlerts_PassFindsTenantsWithEgressThisMonth(t *testing.T) {
	// Arrange: one tenant at 90 % with flushed rows, one with ingress only.
	over := setupEgressFixture(t, 64*mib, testThrottle(), nil)
	quiet := setupEgressFixture(t, 64*mib, testThrottle(), nil)
	sender := &fakeBandwidthEmailSender{}
	a := NewBandwidthAlerter(over.db, zap.NewNop())
	a.SetEmailSender(sender)
	over.srv.bandwidthTracker.recordOn(time.Now(), over.tenantID, "", 0, 32*mib*90/100)
	over.srv.bandwidthTracker.Flush()
	quiet.srv.bandwidthTracker.recordOn(time.Now(), quiet.tenantID, "", 500*mib, 0)
	quiet.srv.bandwidthTracker.Flush()

	// Act: the candidates, then the check with no live counter wired (the
	// recorded month is the counter's value for a tenant it has not loaded).
	candidates, err := a.alertCandidates(context.Background())
	require.NoError(t, err)
	for _, id := range candidates {
		if id == over.tenantID || id == quiet.tenantID {
			a.checkTenantAlerts(context.Background(), id)
		}
	}

	// Assert
	assert.Contains(t, candidates, over.tenantID)
	assert.NotContains(t, candidates, quiet.tenantID, "ingress is not egress")
	emails := sender.emails()
	require.Len(t, emails, 1)
	assert.Equal(t, "egress-"+over.tenantID[:8]+"@test.local", emails[0].to)
	assert.Equal(t, map[int]bool{80: true}, over.alertRows())
	assert.Empty(t, quiet.alertRows())
}

func TestBandwidthAlerts_NilSafe(t *testing.T) {
	var a *BandwidthAlerter
	n, err := a.checkBandwidthAlerts(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)

	n, err = NewBandwidthAlerter((*sql.DB)(nil), zap.NewNop()).checkBandwidthAlerts(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
}
