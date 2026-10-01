package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/crypto"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R10-3: the account-deletion runner. A tenant past its date with objects
// on two backends (one chunked), an active multipart upload, a Stripe
// subscription, sessions and rows in every ledger is erased by one run;
// Stripe is cancelled exactly once; a second run is a no-op; a run holding
// the lock refuses another; a cancel mid-walk stops the walk; a failing
// backend defers the tenant to the next day.

type fakeStripe struct {
	calls atomic.Int32
	err   error
}

func (f *fakeStripe) CancelSubscription(_ context.Context, _ string) error {
	f.calls.Add(1)
	return f.err
}

// flakyDriver is a local driver whose Delete fails on demand.
type flakyDriver struct {
	*drivers.LocalDriver
	fail atomic.Bool
}

func (d *flakyDriver) Delete(ctx context.Context, container, artifact string) error {
	if d.fail.Load() {
		return errors.New("second: connection reset by peer")
	}
	return d.LocalDriver.Delete(ctx, container, artifact)
}

type deletionFixture struct {
	t        *testing.T
	db       *sql.DB
	eng      *engine.CoreEngine
	primary  string // data dir of the primary
	second   *flakyDriver
	secDir   string
	stripe   *fakeStripe
	auth     *auth.AuthService
	sessions dashauth.SessionStore
	runner   *AccountDeletionRunner

	userID, tenantID, email, bucket, suffix, uploadID, hash string
}

func setupDeletionFixture(t *testing.T) *deletionFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	logger := zap.NewNop()

	f := &deletionFixture{t: t, db: db, stripe: &fakeStripe{}}
	f.userID = uuid.New().String()
	f.tenantID = uuid.New().String()
	f.suffix = f.userID[:8]
	f.email = "runner-" + f.suffix + "@test.local"
	f.bucket = "runner-bkt-" + f.suffix
	f.uploadID = "upload-" + strings.Repeat("d", 24) + f.suffix
	f.hash = "cc" + strings.Repeat("0", 54) + f.suffix

	f.primary = t.TempDir()
	f.secDir = t.TempDir()
	f.eng = engine.NewEngine(nil, logger, nil)
	f.eng.AddDriver("local", drivers.NewLocalDriver(f.primary, logger))
	f.second = &flakyDriver{LocalDriver: drivers.NewLocalDriver(f.secDir, logger)}
	f.eng.AddDriver("second", f.second)
	f.eng.SetPrimary("local")

	f.auth = auth.NewAuthService(nil, nil)
	f.sessions = dashauth.NewMemoryStore()
	gci := crypto.NewGlobalContentIndex(db)
	qm := usage.NewQuotaManager(db)
	f.runner = NewAccountDeletionRunner(db, logger, f.eng, gci, qm, account.NewService(db, logger))
	f.runner.Stripe = f.stripe
	f.runner.Auth = f.auth
	f.runner.Sessions = f.sessions
	f.runner.BatchSize = 2
	// The shared test database holds other packages' past-due accounts.
	f.runner.onlyDue = func(d account.Due) bool { return d.UserID == f.userID }

	t.Cleanup(func() {
		ctx := context.Background()
		for _, r := range account.Deleted {
			if r.Key == account.ByTenant {
				_, _ = db.ExecContext(ctx, r.SQL, f.tenantID)
			} else if r.Key == account.ByUser {
				_, _ = db.ExecContext(ctx, r.SQL, f.userID)
			}
		}
		_, _ = db.ExecContext(ctx, `DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenantID)
		_, _ = db.ExecContext(ctx, `DELETE FROM stripe_events WHERE event_id = $1`, "evt_runner_"+f.suffix)
		_, _ = db.ExecContext(ctx, `DELETE FROM global_content_index WHERE plaintext_hash = $1`, f.hash)
		_, _ = db.ExecContext(ctx, `DELETE FROM job_runs WHERE job = $1`, accountDeletionJob)
		_ = os.RemoveAll(multipartDir(f.uploadID))
	})
	return f
}

func (f *deletionFixture) exec(q string, args ...any) {
	f.t.Helper()
	_, err := f.db.ExecContext(context.Background(), q, args...)
	require.NoError(f.t, err, "fixture: %s", q)
}

func (f *deletionFixture) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.db.QueryRowContext(context.Background(), q, args...).Scan(&n), q)
	return n
}

func (f *deletionFixture) container() string { return f.tenantID + "_" + f.bucket }

func (f *deletionFixture) writeBlob(dir, key, body string) {
	f.t.Helper()
	p := filepath.Join(dir, f.container(), key)
	require.NoError(f.t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(f.t, os.WriteFile(p, []byte(body), 0o600))
}

// seed: user past due, tenant with a subscription, three whole objects (two
// on the primary, one on "second"), one chunked object, a locked object, an
// active multipart upload with a staged part, sessions, ledgers.
func (f *deletionFixture) seed(scheduledAt time.Time) {
	u, tn, b := f.userID, f.tenantID, f.bucket
	f.exec(`INSERT INTO users (id, email, password_hash, company, status, deletion_scheduled_at, deletion_reason, created_at, updated_at)
	        VALUES ($1, $2, 'x', 'Runner Co', 'pending_deletion', $3, 'runner test', NOW(), NOW())`, u, f.email, scheduledAt)
	f.exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, stripe_customer_id, stripe_subscription_id, subscription_status, plan)
	        VALUES ($1, 'Runner Co', $2, $3, $4, $5, $6, 'active', 'house')`, tn, f.email, "VKRN"+f.suffix, "SKRN"+f.suffix, "cus_runner_"+f.suffix, "sub_runner_"+f.suffix)
	f.exec(`INSERT INTO api_keys (id, user_id, name, key_id, secret_hash) VALUES ($1, $2, 'primary', $3, 'hash')`, uuid.New().String(), u, "VLT_RN"+f.suffix)
	f.exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier) VALUES ($1, 1000000, 4096, 'standard')`, tn)
	f.exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, $2)`, tn, b)
	for _, o := range []struct{ key, backend, dir string }{
		{"a/one.txt", "local", f.primary}, {"a/two.txt", "local", f.primary}, {"b/three.txt", "second", f.secDir},
	} {
		f.writeBlob(o.dir, o.key, "bytes of "+o.key)
		f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name) VALUES ($1, $2, $3, 1024, 'e', $4)`, tn, b, o.key, o.backend)
	}
	// A demoted object: routed to "second" (cold) with an unreclaimed hot copy
	// on the primary — two backends hold bytes (WP-R6-1's second-copy class).
	f.writeBlob(f.secDir, "d/demoted.bin", "cold bytes")
	f.writeBlob(f.primary, "d/demoted.bin", "hot bytes")
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name) VALUES ($1, $2, 'd/demoted.bin', 1024, 'e', 'second')`, tn, b)
	f.exec(`INSERT INTO smart_demotions (tenant_id, bucket, object_key, etag, size_bytes, hot_backend, cold_backend, reason) VALUES ($1, $2, 'd/demoted.bin', 'e', 1024, 'local', 'second', 'idle')`, tn, b)
	// Object Lock on one of them: the tenant owns the lock; erasure wins.
	f.exec(`INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date) VALUES ($1, $2, 'a/one.txt', 'COMPLIANCE', NOW() + INTERVAL '5 years')`, tn, b)
	// Chunked: head row + manifest; the chunk blob lives in _global and is GC's.
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, is_chunked) VALUES ($1, $2, 'c/big.bin', 1024, 'e', 'local', TRUE)`, tn, b)
	f.exec(`INSERT INTO global_content_index (plaintext_hash, backend_id, storage_key, size_bytes, ref_count) VALUES ($1, 'local', $2, 1024, 1) ON CONFLICT DO NOTHING`, f.hash, "_chunks/"+f.hash)
	f.exec(`INSERT INTO tenant_chunk_refs (tenant_id, bucket_name, object_key, chunk_index, chunk_offset, plaintext_hash) VALUES ($1, $2, 'c/big.bin', 0, 0, $3)`, tn, b, f.hash)
	f.exec(`INSERT INTO object_metadata (tenant_id, bucket_name, object_key, total_size, chunk_count, logical_size) VALUES ($1, $2, 'c/big.bin', 1024, 1, 1024)`, tn, b)
	// Active multipart upload with a staged part on disk.
	f.exec(`INSERT INTO multipart_uploads (upload_id, tenant_id, bucket, object_key, status) VALUES ($1, $2, $3, 'm/part.bin', 'active')`, f.uploadID, tn, b)
	f.exec(`INSERT INTO multipart_parts (upload_id, part_number, etag, size_bytes) VALUES ($1, 1, 'p', 5)`, f.uploadID)
	require.NoError(f.t, os.MkdirAll(multipartDir(f.uploadID), 0o750))
	require.NoError(f.t, os.WriteFile(partFilePath(f.uploadID, 1), []byte("part1"), 0o600))
	// Sessions and credentials.
	f.exec(`INSERT INTO dashboard_sessions (id, user_id, tenant_id, email, expires_at) VALUES ($1, $2, $3, $4, NOW() + INTERVAL '1 day')`, "sess-rn-"+f.suffix, u, tn, f.email)
	f.exec(`INSERT INTO sts_tokens (access_key, secret_key, tenant_id, parent_key_id, expires_at) VALUES ($1, 'sk', $2, 'parent', NOW() + INTERVAL '1 hour')`, "ASIARN"+f.suffix, tn)
	// Ledgers kept and scrubbed.
	f.exec(`INSERT INTO stripe_events (event_id, event_type, tenant_id, data) VALUES ($1, 'invoice.paid', $2, '{}')`, "evt_runner_"+f.suffix, tn)
	f.exec(`INSERT INTO audit_logs (user_id, tenant_id, event_type, action, result, ip, user_agent, performed_by) VALUES ($1, $2, 'key', 'key.created', 'success', '203.0.113.5', 'curl', $1)`, u, tn)
	f.exec(`INSERT INTO audit_logs (tenant_id, event_type, action, result, ip, user_agent) VALUES ($1, 'admin', 'admin.tenant_suspended', 'success', '203.0.113.9', 'Mozilla')`, tn)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestAccountDeletionRunner_ErasesEverythingOnce(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	ctx := context.Background()
	// An in-memory session for the user (the dashboard's store in this test).
	tok, err := f.sessions.Create(ctx, dashauth.SessionData{UserID: f.userID, TenantID: f.tenantID, Email: f.email}, time.Hour)
	require.NoError(t, err)

	res, err := f.runner.RunOnce(ctx)
	require.NoError(t, err, "run: %+v", res)
	require.Len(t, res.Tenants, 1, "one due tenant")
	te := res.Tenants[0]
	assert.Equal(t, outcomeErased, te.Outcome, "%+v", te)
	assert.Equal(t, f.tenantID, te.TenantID)
	assert.Equal(t, 4, te.ObjectsDeleted, "four whole objects")
	assert.Equal(t, 1, te.ChunkedReleased, "one manifest released")
	assert.Equal(t, 1, te.LockedErased, "the locked object is counted")
	assert.Equal(t, 1, te.MultipartAborted)
	assert.Equal(t, 0, te.ObjectFailures)

	// Stage a: Stripe once.
	assert.Equal(t, int32(1), f.stripe.calls.Load(), "Stripe cancel called exactly once")
	// Stage b: bytes gone from the RECORDED backend, manifest released.
	assert.False(t, fileExists(filepath.Join(f.primary, f.container(), "a/one.txt")))
	assert.False(t, fileExists(filepath.Join(f.primary, f.container(), "a/two.txt")))
	assert.False(t, fileExists(filepath.Join(f.secDir, f.container(), "b/three.txt")), "deleted on 'second', not guessed at the primary")
	assert.False(t, fileExists(filepath.Join(f.secDir, f.container(), "d/demoted.bin")), "cold copy gone")
	assert.False(t, fileExists(filepath.Join(f.primary, f.container(), "d/demoted.bin")), "the unreclaimed hot copy is gone too")
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM smart_demotions WHERE tenant_id = $1`, f.tenantID))
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id::text = $1`, f.tenantID))
	var refs int
	var marked bool
	require.NoError(t, f.db.QueryRow(`SELECT ref_count, marked_for_deletion FROM global_content_index WHERE plaintext_hash = $1`, f.hash).Scan(&refs, &marked))
	assert.Equal(t, 0, refs)
	assert.True(t, marked, "the chunk row is GC's to sweep, never deleted here")
	assert.False(t, fileExists(multipartDir(f.uploadID)), "staging dir removed")
	// Stage c: rows.
	for _, tbl := range []string{"object_head_cache", "object_versions", "object_locks", "multipart_uploads", "buckets", "sts_tokens", "tenant_quotas", "dashboard_sessions"} {
		assert.Zero(t, f.count("SELECT COUNT(*) FROM "+tbl+" WHERE tenant_id = $1", f.tenantID), tbl)
	}
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1`, f.userID))
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM tenants WHERE id = $1`, f.tenantID))
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM api_keys WHERE user_id::text = $1`, f.userID))
	var tid sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT tenant_id FROM stripe_events WHERE event_id = $1`, "evt_runner_"+f.suffix).Scan(&tid))
	assert.False(t, tid.Valid, "stripe_events kept, tenant_id nulled")
	// Stage d: sessions revoked (memory store too).
	gone, err := f.sessions.Get(ctx, tok)
	require.NoError(t, err)
	assert.Nil(t, gone, "in-memory dashboard session revoked")
	// Stage e: the audit row and job_runs.
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'account.erased'`, f.tenantID))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'admin.tenant_suspended' AND ip IS NULL AND user_agent IS NULL`, f.tenantID), "operator row kept, scrubbed")
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM audit_logs WHERE user_id::text = $1`, f.userID), "the user's own rows are gone (privacy policy)")
	var outcome string
	require.NoError(t, f.db.QueryRow(`SELECT last_outcome FROM job_runs WHERE job = $1`, accountDeletionJob).Scan(&outcome))
	assert.Equal(t, "ok", outcome)

	// A second run finds nothing and calls nobody.
	res, err = f.runner.RunOnce(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Tenants)
	assert.Equal(t, int32(1), f.stripe.calls.Load())
}

// Packages run in parallel on one test database, and internal/billing and
// internal/account seed their own past-due accounts to call EraseRows on.
// RunOnce walks every due account, so this fixture's runs erased theirs
// mid-test ("account is not pending deletion") and counted them in its own
// result — main went red twice in the day after #529. The fixture's runner
// sees its own account only.
func TestAccountDeletionRunner_FixtureLeavesOtherPackagesDueAccountsAlone(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	otherUser := uuid.New().String()
	otherTenant := "other-pkg-" + otherUser[:8]
	otherEmail := otherTenant + "@test.local"
	f.exec(`INSERT INTO users (id, email, password_hash, company, status, deletion_scheduled_at, created_at, updated_at)
	        VALUES ($1, $2, 'x', 'Other Pkg', 'pending_deletion', NOW() - INTERVAL '2 hours', NOW(), NOW())`, otherUser, otherEmail)
	f.exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, 'Other Pkg', $2, $3, $4)`,
		otherTenant, otherEmail, "VKOP"+otherUser[:8], "SKOP"+otherUser[:8])
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, otherTenant)
		_, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, otherTenant)
		_, _ = f.db.Exec(`DELETE FROM users WHERE id::text = $1`, otherUser)
	})

	res, err := f.runner.RunOnce(context.Background())
	require.NoError(t, err, "run: %+v", res)
	require.Len(t, res.Tenants, 1, "only the fixture's own account")
	assert.Equal(t, f.tenantID, res.Tenants[0].TenantID)
	assert.Equal(t, outcomeErased, res.Tenants[0].Outcome)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1 AND status = 'pending_deletion'`, otherUser), "another package's due account is still there")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM tenants WHERE id = $1`, otherTenant))
}

func TestAccountDeletionRunner_StripeFailureDefersTheTenantAndTouchesNoObject(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	f.stripe.err = errors.New("stripe: 503")
	ctx := context.Background()

	res, err := f.runner.RunOnce(ctx)
	require.Error(t, err, "a deferred tenant makes the run an error outcome")
	require.Len(t, res.Tenants, 1)
	assert.Equal(t, outcomeDeferred, res.Tenants[0].Outcome)
	assert.True(t, fileExists(filepath.Join(f.primary, f.container(), "a/one.txt")), "no object touched before billing is settled")
	assert.Equal(t, 5, f.count(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, f.tenantID))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1`, f.userID))
	var cancelled sql.NullTime
	require.NoError(t, f.db.QueryRow(`SELECT deletion_stripe_cancelled_at FROM tenants WHERE id = $1`, f.tenantID).Scan(&cancelled))
	assert.False(t, cancelled.Valid)

	// Tomorrow Stripe answers: the cancel is retried once, the stamp set, the
	// tenant erased.
	f.stripe.err = nil
	res, err = f.runner.RunOnce(ctx)
	require.NoError(t, err)
	require.Len(t, res.Tenants, 1)
	assert.Equal(t, outcomeErased, res.Tenants[0].Outcome)
	assert.Equal(t, int32(2), f.stripe.calls.Load())
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1`, f.userID))
}

func TestAccountDeletionRunner_NoStripeConfiguredButSubscriptionSetDefers(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	f.runner.Stripe = nil
	res, err := f.runner.RunOnce(context.Background())
	require.Error(t, err)
	require.Len(t, res.Tenants, 1)
	assert.Equal(t, outcomeDeferred, res.Tenants[0].Outcome)
	assert.Contains(t, res.Tenants[0].Error, "STRIPE_SECRET_KEY")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1`, f.userID), "never erase a paying customer's data without cancelling the bill")
}

func TestAccountDeletionRunner_BackendFailureLeavesTheRowAndResumesNextRun(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	f.second.fail.Store(true)
	ctx := context.Background()

	res, err := f.runner.RunOnce(ctx)
	require.Error(t, err)
	require.Len(t, res.Tenants, 1)
	te := res.Tenants[0]
	assert.Equal(t, outcomeDeferred, te.Outcome)
	assert.Equal(t, 2, te.ObjectFailures, "both objects on the dead backend")
	assert.Equal(t, 2, te.ObjectsDeleted, "the primary's objects went")
	assert.Equal(t, 2, f.count(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND is_chunked = FALSE`, f.tenantID), "the rows on the dead backend stay")
	assert.True(t, fileExists(filepath.Join(f.secDir, f.container(), "b/three.txt")), "bytes untouched on the failing backend")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1`, f.userID), "rows not erased while an object remains")
	assert.Equal(t, int32(1), f.stripe.calls.Load())
	var cancelled sql.NullTime
	require.NoError(t, f.db.QueryRow(`SELECT deletion_stripe_cancelled_at FROM tenants WHERE id = $1`, f.tenantID).Scan(&cancelled))
	assert.True(t, cancelled.Valid, "the Stripe stamp records the cancel")

	// Stripe's customer.subscription.deleted delivery clears the id, so
	// tomorrow's run has nothing left to cancel.
	f.exec(`UPDATE tenants SET stripe_subscription_id = NULL, subscription_status = 'canceled' WHERE id = $1`, f.tenantID)
	f.second.fail.Store(false)
	res, err = f.runner.RunOnce(ctx)
	require.NoError(t, err)
	require.Len(t, res.Tenants, 1)
	assert.Equal(t, outcomeErased, res.Tenants[0].Outcome)
	assert.True(t, res.Tenants[0].StripeCancelled, "the stamp still says the runner cancelled it")
	assert.Equal(t, int32(1), f.stripe.calls.Load(), "Stripe not called again")
	assert.False(t, fileExists(filepath.Join(f.secDir, f.container(), "b/three.txt")))
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1`, f.userID))
}

// The stamp says an earlier run cancelled a subscription — not that the one
// on the row today is cancelled. A run that stamps and is then deferred
// leaves the account alive (D-16); the customer may cancel the deletion, buy
// again, and schedule a second deletion months later. That run must cancel
// the new subscription before it erases, or Stripe bills an account that no
// longer exists.
func TestAccountDeletionRunner_StaleStripeStampDoesNotSkipANewSubscription(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	ctx := context.Background()

	// Run 1: Stripe cancelled and stamped, then a backend refuses → deferred.
	f.second.fail.Store(true)
	res, err := f.runner.RunOnce(ctx)
	require.Error(t, err)
	require.Len(t, res.Tenants, 1)
	require.Equal(t, outcomeDeferred, res.Tenants[0].Outcome)
	require.Equal(t, int32(1), f.stripe.calls.Load())
	require.Equal(t, 1, f.count(`SELECT COUNT(*) FROM tenants WHERE id = $1 AND deletion_stripe_cancelled_at IS NOT NULL`, f.tenantID), "fixture: the stamp is set")

	// The customer changes their mind, the webhook clears the cancelled
	// subscription, they check out again, and later schedule a new deletion.
	require.NoError(t, account.NewService(f.db, zap.NewNop()).Cancel(ctx, f.userID))
	f.exec(`UPDATE tenants SET stripe_subscription_id = $2, subscription_status = 'active' WHERE id = $1`, f.tenantID, "sub_again_"+f.suffix)
	f.exec(`UPDATE users SET status = 'pending_deletion', deletion_scheduled_at = NOW() - INTERVAL '1 hour' WHERE id::text = $1`, f.userID)

	f.second.fail.Store(false)
	res, err = f.runner.RunOnce(ctx)
	require.NoError(t, err, "run: %+v", res)
	require.Len(t, res.Tenants, 1)
	assert.Equal(t, outcomeErased, res.Tenants[0].Outcome)
	assert.True(t, res.Tenants[0].StripeCancelled)
	assert.Equal(t, int32(2), f.stripe.calls.Load(), "the subscription bought after the first cancel is cancelled before the erase")
}

func TestAccountDeletionRunner_CancelDuringTheWalkStopsIt(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	ctx := context.Background()
	svc := account.NewService(f.db, zap.NewNop())
	batches := 0
	f.runner.beforeBatch = func() {
		batches++
		if batches == 2 {
			require.NoError(t, svc.Cancel(ctx, f.userID))
		}
	}

	res, err := f.runner.RunOnce(ctx)
	require.NoError(t, err, "a cancelled tenant is not an error")
	require.Len(t, res.Tenants, 1)
	te := res.Tenants[0]
	assert.Equal(t, outcomeCancelled, te.Outcome)
	assert.Equal(t, 2, te.ObjectsDeleted, "the first batch's objects are gone — say so in the note")
	assert.Equal(t, 3, f.count(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, f.tenantID), "the rest stay")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1 AND status = 'active' AND deletion_scheduled_at IS NULL`, f.userID), "the account lives")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM tenants WHERE id = $1`, f.tenantID))
	var used int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.tenantID).Scan(&used))
	assert.Equal(t, int64(4096-2*1024), used, "quota released for the objects that went")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'account.erasure_cancelled'`, f.tenantID))
}

func TestAccountDeletionRunner_ConcurrentRunIsRefused(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	ctx := context.Background()
	conn, err := f.db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	var got bool
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, accountDeletionLockKey).Scan(&got))
	require.True(t, got)

	_, err = f.runner.RunOnce(ctx)
	assert.ErrorIs(t, err, errJobAlreadyRunning)
	assert.Equal(t, int32(0), f.stripe.calls.Load())
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1`, f.userID))

	s := &Server{logger: zap.NewNop(), db: f.db, accountDeletion: f.runner}
	rr := doJSON(t, s.handleAccountDeletionTrigger, "POST", "/api/v1/admin/account-deletion")
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "already_running")

	_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, accountDeletionLockKey)
	rr = doJSON(t, s.handleAccountDeletionTrigger, "POST", "/api/v1/admin/account-deletion")
	assert.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"erased":1`)
	assert.GreaterOrEqual(t, f.count(`SELECT COUNT(*) FROM audit_logs WHERE action = 'admin.account_deletion' AND metadata->>'erased' = '1' AND timestamp > NOW() - INTERVAL '1 minute'`), 1, "the trigger writes an audit row")
}

func TestAccountDeletionRunner_NotDueIsNotTouched(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(48 * time.Hour))
	res, err := f.runner.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Empty(t, res.Tenants)
	assert.Equal(t, int32(0), f.stripe.calls.Load())
	assert.Equal(t, 5, f.count(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, f.tenantID))
}

func TestAccountDeletionRunner_DueFollowsTheDailySchedule(t *testing.T) {
	f := setupDeletionFixture(t)
	fixed := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f.runner.now = func() time.Time { return fixed }
	due, err := f.runner.due(context.Background())
	require.NoError(t, err)
	assert.True(t, due, "never ran: due")
	for _, c := range []struct {
		last time.Time
		want bool
	}{
		{time.Date(2026, 10, 2, 4, 45, 0, 0, time.UTC), false},
		{time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC), true},
		{time.Date(2026, 9, 28, 5, 0, 0, 0, time.UTC), true},
	} {
		f.exec(`INSERT INTO job_runs (job, last_success_at, last_outcome) VALUES ($1, $2, 'ok')
			ON CONFLICT (job) DO UPDATE SET last_success_at = EXCLUDED.last_success_at`, accountDeletionJob, c.last)
		due, err := f.runner.due(context.Background())
		require.NoError(t, err)
		assert.Equal(t, c.want, due, fmt.Sprintf("last success %s", c.last))
	}
}
