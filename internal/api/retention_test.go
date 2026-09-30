package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Review R13-14 (checklist item 5): the nightly retention job. Every policy
// table gets one row past its period and one fresh row; the job deletes
// exactly the old ones, nulls the waitlist PII instead of the row, never
// touches audit_logs, and refuses to run twice at once.

type retentionFixture struct {
	t        *testing.T
	db       *sql.DB
	tenantID string
	email    string
	webhook  string
	oldEvent string
	newEvent string
	job      *RetentionJob
}

func setupRetentionFixture(t *testing.T) *retentionFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	id := uuid.New().String()[:8]
	f := &retentionFixture{t: t, db: db, tenantID: "ret-" + id, email: "ret-" + id + "@test.local",
		webhook: uuid.New().String(), oldEvent: "evt-old-" + id, newEvent: "evt-new-" + id}
	mustExec := func(q string, args ...any) {
		t.Helper()
		_, err := db.Exec(q, args...)
		require.NoError(t, err, q)
	}
	mustExec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1,$2,$3,$4,$5)`,
		f.tenantID, "retention", f.email, "AK-"+f.tenantID, "SK-"+f.tenantID)
	mustExec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier) VALUES ($1, 1000, 0, 'standard')`, f.tenantID)
	mustExec(`INSERT INTO webhook_endpoints (id, tenant_id, url, event_filter, secret, enabled) VALUES ($1, $2, 'https://example.com/hook', '{*}', 'whsec_x', TRUE)`, f.webhook, f.tenantID)
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM webhook_endpoints WHERE tenant_id = $1`,
			`DELETE FROM events WHERE tenant_id = $1`,
			`DELETE FROM s3_access_log WHERE tenant_id = $1`,
			`DELETE FROM stripe_events WHERE tenant_id = $1`,
			`DELETE FROM cdn_access_log WHERE tenant_id = $1`,
			`DELETE FROM access_patterns WHERE tenant_id = $1`,
			`DELETE FROM audit_logs WHERE tenant_id = $1`,
			`DELETE FROM tenant_quotas WHERE tenant_id = $1`,
			`DELETE FROM tenants WHERE id = $1`,
		} {
			_, _ = db.Exec(q, f.tenantID)
		}
		_, _ = db.Exec(`DELETE FROM waitlist_signups WHERE email LIKE $1`, "ret-"+id+"%")
		_, _ = db.Exec(`DELETE FROM job_runs WHERE job = 'retention'`)
	})

	old := time.Now().Add(-100 * 24 * time.Hour)
	fresh := time.Now().Add(-time.Hour)
	// s3_access_log (30 d)
	for _, at := range []time.Time{old, fresh} {
		mustExec(`INSERT INTO s3_access_log (tenant_id, bucket, object_key, operation, status_code, bytes_sent, bytes_received, source_ip, user_agent, request_id, error_code, logged_at)
			VALUES ($1,'b','k','GetObject',200,1,0,'10.0.0.1','ua','rid','',$2)`, f.tenantID, at)
	}
	// events (90 d) — the old one carries a delivery (cascade)
	mustExec(`INSERT INTO events (id, type, tenant_id, data, created_at) VALUES ($1,'object.created',$2,'{}',$3)`, f.oldEvent, f.tenantID, old)
	mustExec(`INSERT INTO events (id, type, tenant_id, data, created_at) VALUES ($1,'object.created',$2,'{}',$3)`, f.newEvent, f.tenantID, fresh)
	// webhook_deliveries (30 d)
	mustExec(`INSERT INTO webhook_deliveries (id, webhook_id, event_id, status, response_code, response_body, latency_ms, created_at) VALUES ($1,$2,$3,'delivered',200,'',5,$4)`,
		uuid.New().String(), f.webhook, f.newEvent, old.Add(50*24*time.Hour)) // 50 d old, on the fresh event
	mustExec(`INSERT INTO webhook_deliveries (id, webhook_id, event_id, status, response_code, response_body, latency_ms, created_at) VALUES ($1,$2,$3,'delivered',200,'',5,$4)`,
		uuid.New().String(), f.webhook, f.newEvent, fresh)
	// quota_usage_events (90 d)
	mustExec(`INSERT INTO quota_usage_events (tenant_id, operation, bytes_delta, object_key, "timestamp") VALUES ($1,'RESERVE',1,'',$2)`, f.tenantID, old)
	mustExec(`INSERT INTO quota_usage_events (tenant_id, operation, bytes_delta, object_key, "timestamp") VALUES ($1,'RESERVE',1,'',$2)`, f.tenantID, fresh)
	// stripe_events (90 d)
	mustExec(`INSERT INTO stripe_events (event_id, event_type, processed_at, tenant_id, data) VALUES ($1,'x',$2,$3,'{}')`, "evt_old_"+id, old, f.tenantID)
	mustExec(`INSERT INTO stripe_events (event_id, event_type, processed_at, tenant_id, data) VALUES ($1,'x',$2,$3,'{}')`, "evt_new_"+id, fresh, f.tenantID)
	// cdn_access_log (2 d)
	mustExec(`INSERT INTO cdn_access_log (tenant_id, bucket, object_key, bytes_sent, country, referer, accessed_at) VALUES ($1,'pub','k',1,'US','',$2)`, f.tenantID, time.Now().Add(-3*24*time.Hour))
	mustExec(`INSERT INTO cdn_access_log (tenant_id, bucket, object_key, bytes_sent, country, referer, accessed_at) VALUES ($1,'pub','k',1,'US','',$2)`, f.tenantID, fresh)
	// access_patterns (90 d on last_seen)
	mustExec(`INSERT INTO access_patterns (tenant_id, container, artifact_key, operation, access_time, first_seen, last_seen) VALUES ($1,'c','old','GET',$2,$2,$2)`, f.tenantID, old)
	mustExec(`INSERT INTO access_patterns (tenant_id, container, artifact_key, operation, access_time, first_seen, last_seen) VALUES ($1,'c','new','GET',$2,$2,$2)`, f.tenantID, fresh)
	// waitlist_signups (PII nulled after 90 d, row kept)
	mustExec(`INSERT INTO waitlist_signups (email, source, ip_address, user_agent, created_at) VALUES ($1,'landing','10.0.0.9','ua-old',$2)`, "ret-"+id+"-old@test.local", old)
	mustExec(`INSERT INTO waitlist_signups (email, source, ip_address, user_agent, created_at) VALUES ($1,'landing','10.0.0.9','ua-new',$2)`, "ret-"+id+"-new@test.local", fresh)
	// audit_logs: never pruned
	mustExec(`INSERT INTO audit_logs (tenant_id, event_type, action, result, severity, "timestamp", created_at) VALUES ($1,'admin','admin.test','success','info',$2,$2)`, f.tenantID, old.Add(-365*24*time.Hour))

	f.job = NewRetentionJob(db, zap.NewNop())
	require.NotNil(t, f.job)
	f.job.BatchSize = 1 // exercise the batch loop
	return f
}

func (f *retentionFixture) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.db.QueryRow(q, args...).Scan(&n))
	return n
}

func TestRetention_PrunesOnlyRowsPastTheirPeriod(t *testing.T) {
	f := setupRetentionFixture(t)

	res, err := f.job.RunOnce(context.Background())
	require.NoError(t, err, "%+v", res)

	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM s3_access_log WHERE tenant_id = $1`, f.tenantID))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM events WHERE tenant_id = $1`, f.tenantID))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM events WHERE id = $1`, f.newEvent))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = $1`, f.webhook))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM quota_usage_events WHERE tenant_id = $1`, f.tenantID))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM stripe_events WHERE tenant_id = $1`, f.tenantID))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM cdn_access_log WHERE tenant_id = $1`, f.tenantID))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM access_patterns WHERE tenant_id = $1`, f.tenantID))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1`, f.tenantID), "audit_logs are never pruned")

	// Waitlist: both rows kept; only the old one lost its IP + user agent.
	var oldIP, oldUA, newIP, newUA string
	require.NoError(t, f.db.QueryRow(`SELECT ip_address, user_agent FROM waitlist_signups WHERE email = $1`, "ret-"+f.tenantID[4:]+"-old@test.local").Scan(&oldIP, &oldUA))
	require.NoError(t, f.db.QueryRow(`SELECT ip_address, user_agent FROM waitlist_signups WHERE email = $1`, "ret-"+f.tenantID[4:]+"-new@test.local").Scan(&newIP, &newUA))
	assert.Empty(t, oldIP, "old sign-up IP scrubbed")
	assert.Empty(t, oldUA, "old sign-up UA scrubbed")
	assert.Equal(t, "10.0.0.9", newIP)
	assert.Equal(t, "ua-new", newUA)

	// Per-table counts are reported (at least our rows; the shared DB may hold more).
	for _, tbl := range []string{"s3_access_log", "events", "quota_usage_events", "stripe_events", "cdn_access_log", "webhook_deliveries", "access_patterns", "waitlist_signups"} {
		assert.GreaterOrEqual(t, res.Tables[tbl], int64(1), tbl)
	}
	assert.Empty(t, res.Errors)

	// job_runs recorded the success and the job is no longer due.
	last, err := lastJobSuccess(context.Background(), f.db, "retention")
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), last, time.Minute)
	due, err := f.job.due(context.Background())
	require.NoError(t, err)
	assert.False(t, due)

	// A second run is a no-op.
	res, err = f.job.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(0), res.Total)
}

func TestRetention_SecondConcurrentRunIsRefused(t *testing.T) {
	f := setupRetentionFixture(t)
	conn, err := f.db.Conn(context.Background())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	var got bool
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock($1)`, retentionLockKey).Scan(&got))
	require.True(t, got)

	_, err = f.job.RunOnce(context.Background())
	assert.ErrorIs(t, err, errJobAlreadyRunning)
	assert.Equal(t, 2, f.count(`SELECT COUNT(*) FROM s3_access_log WHERE tenant_id = $1`, f.tenantID), "nothing pruned while the lock is held")

	s := &Server{logger: zap.NewNop(), db: f.db, retention: f.job}
	rr := doJSON(t, s.handleRetentionTrigger, "POST", "/api/v1/admin/retention")
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())

	_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, retentionLockKey)
	rr = doJSON(t, s.handleRetentionTrigger, "POST", "/api/v1/admin/retention")
	assert.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"s3_access_log":`)
}

func TestRetention_DueFollowsTheDailyScheduleAndCatchesUp(t *testing.T) {
	f := setupRetentionFixture(t)
	fixed := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	f.job.now = func() time.Time { return fixed }

	// Never ran: due.
	due, err := f.job.due(context.Background())
	require.NoError(t, err)
	assert.True(t, due)

	assert.Equal(t, time.Date(2026, 9, 30, 3, 30, 0, 0, time.UTC), f.job.lastScheduled(fixed))
	assert.Equal(t, time.Date(2026, 9, 29, 3, 30, 0, 0, time.UTC), f.job.lastScheduled(time.Date(2026, 9, 30, 1, 0, 0, 0, time.UTC)))

	// Succeeded after today's 03:30: not due. Succeeded before it: due
	// (this is the catch-up after a restart that ate the tick, R13-07).
	for _, c := range []struct {
		last time.Time
		want bool
	}{
		{time.Date(2026, 9, 30, 3, 45, 0, 0, time.UTC), false},
		{time.Date(2026, 9, 30, 2, 0, 0, 0, time.UTC), true},
		{time.Date(2026, 9, 27, 3, 45, 0, 0, time.UTC), true},
	} {
		_, err := f.db.Exec(`INSERT INTO job_runs (job, last_success_at, last_outcome) VALUES ('retention', $1, 'ok')
			ON CONFLICT (job) DO UPDATE SET last_success_at = EXCLUDED.last_success_at`, c.last)
		require.NoError(t, err)
		due, err := f.job.due(context.Background())
		require.NoError(t, err)
		assert.Equal(t, c.want, due, fmt.Sprintf("last success %s", c.last))
	}
}

func TestRetention_PoliciesMatchThePolicyPage(t *testing.T) {
	// The numbers the privacy policy and the DPA state (WP-R14-7). Change
	// both places together.
	want := map[string]time.Duration{
		"s3_access_log": 30 * 24 * time.Hour, "events": 90 * 24 * time.Hour, "quota_usage_events": 90 * 24 * time.Hour,
		"stripe_events": 90 * 24 * time.Hour, "cdn_access_log": 2 * 24 * time.Hour, "webhook_deliveries": 30 * 24 * time.Hour,
		"access_patterns": 90 * 24 * time.Hour, "waitlist_signups": 90 * 24 * time.Hour,
	}
	got := map[string]time.Duration{}
	for _, p := range defaultRetentionPolicies() {
		got[p.Table] = p.MaxAge
		if p.Table == "waitlist_signups" {
			assert.Equal(t, retentionScrubColumns, p.Action)
			assert.ElementsMatch(t, []string{"ip_address", "user_agent"}, p.ScrubColumns)
		} else {
			assert.Equal(t, retentionDelete, p.Action, p.Table)
		}
		assert.NotEqual(t, "audit_logs", p.Table)
	}
	assert.Equal(t, want, got)
}
