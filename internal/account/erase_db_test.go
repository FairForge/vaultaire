package account

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

// WP-R10-3 stage c: the row erasure. Successor of WP-8's
// TestExecuteDeletion_RemovesAllTenantData (api package) with the survivors
// R9 area 5 / R13 listed seeded as well, the kept ledgers asserted kept and
// scrubbed, and the cancel-wins guard.

type eraseFixture struct {
	t        *testing.T
	db       *sql.DB
	svc      *Service
	userID   string
	tenantID string
	email    string
	suffix   string
	bucket   string
	hash     string
}

func setupEraseFixture(t *testing.T) *eraseFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}

	f := &eraseFixture{t: t, db: db, svc: NewService(db, zap.NewNop())}
	f.userID = uuid.New().String()
	f.tenantID = uuid.New().String() // UUID string so the chunk tables accept it
	f.suffix = f.userID[:8]
	f.email = fmt.Sprintf("erase-%s@example.com", f.suffix)
	f.bucket = "erase-bkt-" + f.suffix
	f.hash = strings.Repeat("0", 64-len(f.suffix)) + f.suffix

	t.Cleanup(func() {
		// Residue after a failed run: the deleted list itself is the cleanup,
		// bound to this fixture's ids only.
		for _, r := range Deleted {
			arg, ok := keyArg(r.Key, f.userID, f.tenantID, f.email)
			if ok {
				_, _ = db.ExecContext(ctx, r.SQL, arg)
			}
		}
		_, _ = db.ExecContext(ctx, `DELETE FROM stripe_events WHERE event_id = $1`, "evt_erase_"+f.suffix)
		_, _ = db.ExecContext(ctx, `DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenantID)
		_, _ = db.ExecContext(ctx, `DELETE FROM global_content_index WHERE plaintext_hash = $1`, f.hash)
	})
	return f
}

func (f *eraseFixture) exec(q string, args ...any) {
	f.t.Helper()
	_, err := f.db.ExecContext(context.Background(), q, args...)
	require.NoError(f.t, err, "fixture: %s", q)
}

func (f *eraseFixture) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.db.QueryRowContext(context.Background(), q, args...).Scan(&n), q)
	return n
}

// seed writes one row into every table the prompt and R9/R13 named, past
// due and pending.
func (f *eraseFixture) seed(scheduledAt time.Time) {
	u, tn, b, s := f.userID, f.tenantID, f.bucket, f.suffix
	f.exec(`INSERT INTO users (id, email, password_hash, company, status, deletion_scheduled_at, deletion_reason, created_at, updated_at)
	        VALUES ($1, $2, 'x', 'Erase Co', 'pending_deletion', $3, 'test', NOW(), NOW())`, u, f.email, scheduledAt)
	f.exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, stripe_customer_id) VALUES ($1, 'Erase Co', $2, $3, $4, $5)`,
		tn, f.email, "VKER"+s, "SKER"+s, "cus_erase_"+s)
	f.exec(`INSERT INTO api_keys (id, user_id, name, key_id, secret_hash) VALUES ($1, $2, 'primary', $3, 'hash')`,
		uuid.New().String(), u, "VLT_ER"+s)
	f.exec(`INSERT INTO tenant_quotas (tenant_id) VALUES ($1)`, tn)
	f.exec(`INSERT INTO tenant_floor_quotas (tenant_id, floor, storage_limit_bytes) VALUES ($1, 'standard', 1)`, tn)
	f.exec(`INSERT INTO quota_usage_events (tenant_id, operation, bytes_delta, object_key) VALUES ($1, 'RESERVE', 1024, 'k')`, tn)
	f.exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, $2)`, tn, b)
	f.exec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, size_bytes, etag) VALUES ($1, $2, 'k', 'v1', 1024, 'e')`, tn, b)
	f.exec(`INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date) VALUES ($1, $2, 'k', 'COMPLIANCE', NOW() + INTERVAL '10 years')`, tn, b)
	f.exec(`INSERT INTO object_locations (tenant_id, bucket, object_key, backend_name) VALUES ($1, $2, 'k', 'local')`, tn, b)
	f.exec(`INSERT INTO smart_demotions (tenant_id, bucket, object_key, etag, size_bytes, hot_backend, cold_backend, reason) VALUES ($1, $2, 'k', 'e', 1, 'idrive', 'lyve', 'idle')`, tn, b)
	f.exec(`INSERT INTO multipart_uploads (upload_id, tenant_id, bucket, object_key, status) VALUES ($1, $2, $3, 'big', 'active')`, "upload-"+strings.Repeat("a", 24)+s, tn, b)
	f.exec(`INSERT INTO multipart_parts (upload_id, part_number, etag, size_bytes) VALUES ($1, 1, 'p', 5)`, "upload-"+strings.Repeat("a", 24)+s)
	f.exec(`INSERT INTO bucket_notifications (tenant_id, bucket, event_filter, target_type, target_url) VALUES ($1, $2, 's3:ObjectCreated:*', 'webhook', 'https://example.com/n')`, tn, b)
	f.exec(`INSERT INTO global_content_index (plaintext_hash, backend_id, storage_key, size_bytes) VALUES ($1, 'local', $2, 1024) ON CONFLICT DO NOTHING`, f.hash, "_chunks/"+f.hash)
	f.exec(`INSERT INTO tenant_chunk_refs (tenant_id, bucket_name, object_key, chunk_index, chunk_offset, plaintext_hash) VALUES ($1, $2, 'chunked', 0, 0, $3)`, tn, b, f.hash)
	f.exec(`INSERT INTO object_metadata (tenant_id, bucket_name, object_key, total_size, chunk_count, logical_size) VALUES ($1, $2, 'chunked', 1024, 1, 1024)`, tn, b)
	f.exec(`INSERT INTO events (id, type, tenant_id) VALUES ($1, 'object.created', $2)`, "ev-er-"+s, tn)
	f.exec(`INSERT INTO webhook_endpoints (id, tenant_id, url, secret) VALUES ($1, $2, 'https://example.com/hook', 'whsec')`, "wh-er-"+s, tn)
	f.exec(`INSERT INTO webhook_deliveries (id, webhook_id, event_id, status) VALUES ($1, $2, $3, 'success')`, "whd-er-"+s, "wh-er-"+s, "ev-er-"+s)
	f.exec(`INSERT INTO s3_access_log (tenant_id, bucket, object_key, operation, status_code, bytes_sent, bytes_received, source_ip, user_agent, request_id, error_code)
	        VALUES ($1, $2, 'k', 'GetObject', 200, 1, 0, '10.0.0.1', 'ua', 'rid', '')`, tn, b)
	f.exec(`INSERT INTO cdn_access_log (tenant_id, bucket, object_key, bytes_sent, country, referer) VALUES ($1, $2, 'k', 1, 'US', '')`, tn, b)
	f.exec(`INSERT INTO cdn_stats_daily (tenant_id, bucket, date, requests, bytes_sent) VALUES ($1, $2, CURRENT_DATE, 1, 1)`, tn, b)
	f.exec(`INSERT INTO bandwidth_usage_daily (tenant_id, date, ingress_bytes, egress_bytes) VALUES ($1, CURRENT_DATE, 1, 1)`, tn)
	f.exec(`INSERT INTO bandwidth_alerts (tenant_id, threshold_pct, alert_type) VALUES ($1, 80, 'email')`, tn)
	f.exec(`INSERT INTO idempotency_cache (tenant_id, idempotency_key, method, path, response_status) VALUES ($2, $1, 'POST', '/x', 200)`, "idem-"+s, tn)
	f.exec(`INSERT INTO sts_tokens (access_key, secret_key, tenant_id, parent_key_id, expires_at) VALUES ($1, 'sk', $2, 'parent', NOW() + INTERVAL '1 hour')`, "ASIAER"+s, tn)
	f.exec(`INSERT INTO dashboard_sessions (id, user_id, tenant_id, email, expires_at) VALUES ($1, $2, $3, $4, NOW() + INTERVAL '1 day')`, "sess-"+s, u, tn, f.email)
	f.exec(`INSERT INTO user_mfa (user_id, secret) VALUES ($1, 'totp')`, u)
	f.exec(`INSERT INTO oauth_accounts (user_id, provider, provider_id, email) VALUES ($1, 'google', $2, $3)`, u, "oauth-er-"+s, f.email)
	f.exec(`INSERT INTO user_activities (id, user_id, action) VALUES ($1, $2, 'login')`, uuid.New().String(), u)
	f.exec(`INSERT INTO tenant_encryption_keys (tenant_id, seed, public_key) VALUES ($1, $2, $3)`, tn, []byte("seed"), []byte("pub"))
	f.exec(`INSERT INTO artifacts (tenant_id, container, name, size) VALUES ($1, $2, 'k', 1)`, tn, b)
	f.exec(`INSERT INTO account_exports (user_id, tenant_id, status) VALUES ($1, $2, 'completed')`, u, tn)
	f.exec(`INSERT INTO admin_notes (tenant_id, admin_user_id, note) VALUES ($1, $2, 'note')`, tn, u)
	f.exec(`INSERT INTO waitlist_signups (email, source, ip_address, user_agent) VALUES ($1, 'landing', '10.0.0.9', 'ua')`, f.email)
	f.exec(`INSERT INTO feature_flags (flag_key, tenant_id, enabled) VALUES ('chunking', $1, FALSE)`, tn)
	// Kept: the Stripe ledger and the operator audit rows about the tenant;
	// the user's own audit rows go.
	f.exec(`INSERT INTO stripe_events (event_id, event_type, tenant_id, data) VALUES ($1, 'invoice.paid', $2, '{}')`, "evt_erase_"+s, tn)
	f.exec(`INSERT INTO audit_logs (user_id, tenant_id, event_type, action, result, ip, user_agent, performed_by)
	        VALUES ($1, $2, 'key', 'key.created', 'success', '203.0.113.5', 'curl', $1)`, u, tn)
	f.exec(`INSERT INTO audit_logs (user_id, tenant_id, event_type, action, result, ip, user_agent, performed_by)
	        VALUES (NULL, $1, 'admin', 'admin.tenant_suspended', 'success', '203.0.113.9', 'Mozilla', NULL)`, tn)
}

func TestEraseRows_RemovesEveryListedTableKeepsTheLedgers(t *testing.T) {
	f := setupEraseFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	ctx := context.Background()

	due, err := f.svc.ListDue(ctx, time.Now(), 1000)
	require.NoError(t, err)
	var found *Due
	for i := range due {
		if due[i].UserID == f.userID {
			found = &due[i]
		}
	}
	require.NotNil(t, found, "ListDue must return the past-due user")
	assert.Equal(t, f.tenantID, found.TenantID, "tenant joined by e-mail")

	res, err := f.svc.EraseRows(ctx, f.userID, f.tenantID, f.email, time.Now())
	require.NoError(t, err, "EraseRows must not trip a foreign key")
	assert.Greater(t, res.Total, int64(30), "rows removed: %v", res.Rows)

	for _, r := range Deleted {
		if r.SQL[:6] != "DELETE" {
			continue
		}
		arg, _ := keyArg(r.Key, f.userID, f.tenantID, f.email)
		// Turn the DELETE into a COUNT with the same predicate.
		where := r.SQL[strings.Index(r.SQL, "WHERE"):]
		n := f.count("SELECT COUNT(*) FROM "+r.Table+" "+where, arg)
		assert.Zero(t, n, "%s must be empty for the erased account", r.Table)
	}
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM multipart_parts WHERE upload_id = $1`, "upload-"+strings.Repeat("a", 24)+f.suffix), "parts cascade with the upload")

	// Kept and scrubbed.
	var tid sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT tenant_id FROM stripe_events WHERE event_id = $1`, "evt_erase_"+f.suffix).Scan(&tid))
	assert.False(t, tid.Valid, "stripe_events row kept with tenant_id nulled")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1`, f.tenantID), "the operator's row about the tenant is kept; the user's own row is gone")
	var ip, ua sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT ip::text, user_agent FROM audit_logs WHERE tenant_id = $1`, f.tenantID).Scan(&ip, &ua))
	assert.False(t, ip.Valid, "ip scrubbed")
	assert.False(t, ua.Valid, "user agent scrubbed")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM global_content_index WHERE plaintext_hash = $1`, f.hash), "shared dedup row survives (marked for GC)")
	var marked bool
	require.NoError(t, f.db.QueryRow(`SELECT marked_for_deletion FROM global_content_index WHERE plaintext_hash = $1`, f.hash).Scan(&marked))
	assert.True(t, marked, "ref count released set-based → zero → marked")

	// Second call: the user is gone → ErrNotPending, nothing to do.
	_, err = f.svc.EraseRows(ctx, f.userID, f.tenantID, f.email, time.Now())
	assert.ErrorIs(t, err, ErrNotPending)
}

func TestEraseRows_RefusesWhenCancelWon(t *testing.T) {
	f := setupEraseFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	ctx := context.Background()
	require.NoError(t, f.svc.Cancel(ctx, f.userID))

	_, err := f.svc.EraseRows(ctx, f.userID, f.tenantID, f.email, time.Now())
	assert.ErrorIs(t, err, ErrNotPending)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM object_versions WHERE tenant_id = $1`, f.tenantID), "nothing erased")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1`, f.userID))

	still, err := f.svc.StillDue(ctx, f.userID, time.Now())
	require.NoError(t, err)
	assert.False(t, still)
}

func TestEraseRows_RefusesBeforeTheDate(t *testing.T) {
	f := setupEraseFixture(t)
	f.seed(time.Now().Add(24 * time.Hour))
	ctx := context.Background()
	_, err := f.svc.EraseRows(ctx, f.userID, f.tenantID, f.email, time.Now())
	assert.ErrorIs(t, err, ErrNotPending)
	due, err := f.svc.ListDue(ctx, time.Now(), 1000)
	require.NoError(t, err)
	for _, d := range due {
		assert.NotEqual(t, f.userID, d.UserID, "a future date is not due")
	}
}

func TestSchedule_DBRoundTrip(t *testing.T) {
	f := setupEraseFixture(t)
	f.exec(`INSERT INTO users (id, email, password_hash, company, created_at, updated_at) VALUES ($1, $2, 'x', 'Erase Co', NOW(), NOW())`, f.userID, f.email)
	ctx := context.Background()
	first, err := f.svc.Schedule(ctx, f.userID, f.tenantID, strings.Repeat("r", MaxReasonLen+10))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(GracePeriod), first, 5*time.Second)
	second, err := f.svc.Schedule(ctx, f.userID, f.tenantID, "again")
	require.NoError(t, err)
	assert.True(t, first.Equal(second), "idempotent")
	st, err := f.svc.GetStatus(ctx, f.userID)
	require.NoError(t, err)
	assert.True(t, st.Scheduled)
	assert.Len(t, st.Reason, MaxReasonLen, "reason capped in the row")
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM users WHERE id::text = $1`, f.userID).Scan(&status))
	assert.Equal(t, StatusPending, status)
	require.NoError(t, f.svc.Cancel(ctx, f.userID))
	assert.ErrorIs(t, f.svc.Cancel(ctx, f.userID), ErrNoPendingDeletion)
}

func TestEraseRows_RefusesWhileObjectsRemain(t *testing.T) {
	f := setupEraseFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name) VALUES ($1, $2, 'still-here', 1, 'e', 'idrive')`, f.tenantID, f.bucket)
	ctx := context.Background()
	_, err := f.svc.EraseRows(ctx, f.userID, f.tenantID, f.email, time.Now())
	assert.ErrorIs(t, err, ErrObjectsRemain, "a head row means bytes on a backend — never orphan them")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1`, f.userID), "nothing erased")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM tenants WHERE id = $1`, f.tenantID))
}
