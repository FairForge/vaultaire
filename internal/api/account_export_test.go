package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R10-3b / WP-R12-1: the asynchronous GDPR export. One service behind the
// management API, the dashboard and (formerly) the compliance route; the
// render streams section by section into an object of the tenant's system
// bucket `_exports` through the customer write path; the row is the audit
// trail; a presigned URL on the tenant's primary pair is the download; the
// retention job removes the object after 7 days.
//
// Every test here touches only its own tenant's rows (the test database is
// shared between packages); the job runs scoped to the fixture's user.

type exportFixture struct {
	t                    *testing.T
	db                   *sql.DB
	eng                  *engine.CoreEngine
	dir                  string
	svc                  *AccountExportService
	sched                *jobScheduler
	sj                   *scheduledJob
	server               *Server
	userID               string
	tenantID             string
	email                string
	accessKey, secretKey string
	suffix               string
	buckets              []string
	// secrets the fixture wrote — none may appear in an export body
	secrets map[string]string
}

func setupExportFixture(t *testing.T) *exportFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	logger := zap.NewNop()

	f := &exportFixture{t: t, db: db, dir: t.TempDir(), secrets: map[string]string{}}
	f.userID = uuid.New().String()
	f.tenantID = "tenant-exp-" + f.userID[:8]
	f.suffix = f.userID[:8]
	f.email = "export-" + f.suffix + "@test.local"
	f.accessKey = "VKEXP" + strings.ToUpper(f.suffix)
	f.secretKey = "SKEXP" + f.suffix + "secret"
	f.buckets = []string{"exp-a-" + f.suffix, "exp-b-" + f.suffix}

	f.eng = engine.NewEngine(nil, logger, nil)
	f.eng.AddDriver("local", drivers.NewLocalDriver(f.dir, logger))
	f.eng.SetPrimary("local")

	qm := usage.NewQuotaManager(db)
	gci := crypto.NewGlobalContentIndex(db)
	f.svc = NewAccountExportService(db, f.eng, qm, gci, "http://s3.test.local", logger)
	f.svc.JobName = "test_account_export_" + f.suffix
	f.svc.PageSize = 2
	f.svc.onlyUserID = f.userID
	f.sched = newJobScheduler(db, logger)
	f.sj = f.sched.Register(f.svc.spec())
	require.NotNil(t, f.sj)

	f.server = &Server{logger: logger, router: chi.NewRouter(), engine: f.eng, db: db, testMode: true,
		quotaManager: qm, gci: gci, accountExports: f.svc}

	t.Cleanup(func() {
		ctx := context.Background()
		for _, r := range account.Deleted {
			switch r.Key {
			case account.ByTenant:
				_, _ = db.ExecContext(ctx, r.SQL, f.tenantID)
			case account.ByUser:
				_, _ = db.ExecContext(ctx, r.SQL, f.userID)
			case account.ByEmail:
				_, _ = db.ExecContext(ctx, r.SQL, f.email)
			}
		}
		_, _ = db.ExecContext(ctx, `DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenantID)
		_, _ = db.ExecContext(ctx, `DELETE FROM job_runs WHERE job = $1`, f.svc.JobName)
	})
	return f
}

func (f *exportFixture) exec(q string, args ...any) {
	f.t.Helper()
	_, err := f.db.ExecContext(context.Background(), q, args...)
	require.NoError(f.t, err, "fixture: %s", q)
}

func (f *exportFixture) tenant() *tenant.Tenant {
	return &tenant.Tenant{ID: f.tenantID, Namespace: "tenant/" + f.tenantID + "/"}
}

// seed: a user with a password and MFA, a tenant with a house, two buckets
// with five objects between them (the page size is 2: the keyset crosses the
// bucket boundary), versions, a lock, keys, OAuth, sessions, a webhook with a
// signing secret, events, bandwidth, a waitlist row.
func (f *exportFixture) seed() {
	u, tn := f.userID, f.tenantID
	f.secrets["password_hash"] = "$2a$10$exportpwhash" + f.suffix
	f.secrets["tenant_secret_key"] = f.secretKey
	f.secrets["api_secret_hash"] = "apisecrethash" + f.suffix
	f.secrets["api_secret_key"] = "apisecretkey" + f.suffix
	f.secrets["mfa_secret"] = "MFASECRET" + strings.ToUpper(f.suffix)
	f.secrets["backup_code"] = "backupcode" + f.suffix
	f.secrets["webhook_secret"] = "whsec_export" + f.suffix
	f.secrets["verify_token"] = "verifytoken" + f.suffix
	f.secrets["sts_secret"] = "stssecret" + f.suffix

	f.exec(`INSERT INTO users (id, email, password_hash, company, status, role, email_verified, email_verify_token,
	                           signup_referrer, signup_utm_source, created_at, updated_at)
	        VALUES ($1, $2, $3, 'Export Co', 'active', 'user', TRUE, $4, 'https://ref.example', 'newsletter', NOW(), NOW())`,
		u, f.email, f.secrets["password_hash"], f.secrets["verify_token"])
	f.exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, plan, intent_std_tb, intent_vault_tb, intent_room, house_period, slug)
	        VALUES ($1, 'Export Co', $2, $3, $4, 'house', 2, 1, 'attic', 'annual', $5)`, tn, f.email, f.accessKey, f.secretKey, "exp-"+f.suffix)
	f.exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier) VALUES ($1, 3000000000000, 4096, 'standard')`, tn)
	f.exec(`INSERT INTO tenant_floor_quotas (tenant_id, floor, storage_limit_bytes, storage_used_bytes) VALUES ($1, 'standard', 2000000000000, 4096), ($1, 'vault', 1000000000000, 0)`, tn)
	for _, b := range f.buckets {
		f.exec(`INSERT INTO buckets (tenant_id, name, visibility, region, versioning_status, object_lock_enabled, cors_origins, metadata)
		        VALUES ($1, $2, 'private', 'us-central-1', 'Enabled', TRUE, 'https://app.example', '{"tags":{"team":"blue"}}')`, tn, b)
	}
	for i, o := range []struct{ bucket, key, backend, floor string }{
		{f.buckets[0], "a/1.txt", "local", "standard"}, {f.buckets[0], "a/2.txt", "local", "standard"}, {f.buckets[0], "z/3.txt", "geyser", "vault"},
		{f.buckets[1], "b/4.txt", "local", "standard"}, {f.buckets[1], "b/5.txt", "local", "standard"},
	} {
		f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, floor, metadata, tags)
		        VALUES ($1, $2, $3, $4, $5, 'text/plain', $6, $7, '{"x-amz-meta-owner":"me"}', '{"env":"prod"}')`,
			tn, o.bucket, o.key, 100+i, fmt.Sprintf("etag-%d", i), o.backend, o.floor)
	}
	f.exec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, size_bytes, etag, content_type, is_latest, is_delete_marker, backend_name)
	        VALUES ($1, $2, 'a/1.txt', 'v1', 100, 'etag-0', 'text/plain', TRUE, FALSE, 'local')`, tn, f.buckets[0])
	f.exec(`INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date) VALUES ($1, $2, 'a/1.txt', 'GOVERNANCE', NOW() + INTERVAL '1 year')`, tn, f.buckets[0])
	f.exec(`INSERT INTO api_keys (id, user_id, name, key_id, secret_hash, secret_key, permissions, bucket_scope, last_used)
	        VALUES ($1, $2, 'ci key', $3, $4, $5, '["read"]', $6, NOW())`,
		uuid.New().String(), u, "VLT_EXP"+f.suffix, f.secrets["api_secret_hash"], f.secrets["api_secret_key"], "{"+f.buckets[0]+"}")
	f.exec(`INSERT INTO user_mfa (user_id, secret, enabled, backup_codes) VALUES ($1, $2, TRUE, $3)`, u, f.secrets["mfa_secret"], "{"+f.secrets["backup_code"]+"}")
	f.exec(`INSERT INTO oauth_accounts (id, user_id, provider, provider_id, email, name) VALUES ($1, $2, 'github', 'gh-12345', $3, 'Export Person')`, uuid.New().String(), u, f.email)
	f.exec(`INSERT INTO dashboard_sessions (id, user_id, tenant_id, email, expires_at, ip_address, user_agent) VALUES ($1, $2, $3, $4, NOW() + INTERVAL '1 day', '203.0.113.9', 'ExportBrowser/1.0')`, "sess-exp-"+f.suffix, u, tn, f.email)
	f.exec(`INSERT INTO webhook_endpoints (id, tenant_id, url, event_filter, secret, enabled) VALUES ($1, $2, 'https://hooks.example/in', '{object.created}', $3, TRUE)`, uuid.New().String(), tn, f.secrets["webhook_secret"])
	f.exec(`INSERT INTO sts_tokens (access_key, secret_key, tenant_id, parent_key_id, expires_at) VALUES ($1, $2, $3, 'parent', NOW() + INTERVAL '1 hour')`, "ASIAEXP"+f.suffix, f.secrets["sts_secret"], tn)
	for i := 0; i < 3; i++ {
		f.exec(`INSERT INTO events (id, type, tenant_id, data, created_at) VALUES ($1, 'bucket.created', $2, '{"bucket":"x"}', NOW() - make_interval(mins => $3))`, fmt.Sprintf("evt-exp-%s-%d", f.suffix, i), tn, i)
	}
	f.exec(`INSERT INTO bandwidth_usage_daily (tenant_id, date, ingress_bytes, egress_bytes, requests_count) VALUES ($1, CURRENT_DATE, 10, 20, 3)`, tn)
	f.exec(`INSERT INTO waitlist_signups (email, source, ip_address, user_agent, referrer, utm_source) VALUES ($1, 'landing', '198.51.100.7', 'UA', 'https://ref.example', 'newsletter')`, f.email)
	f.exec(`INSERT INTO audit_logs (user_id, tenant_id, event_type, action, resource, result, ip, user_agent) VALUES ($1, $2, 'auth', 'auth.login_succeeded', $3, 'success', '203.0.113.9', 'ExportBrowser/1.0')`, u, tn, "user:"+u)
}

// run drives one scheduler run of the export job and returns its report.
func (f *exportFixture) run() jobReport {
	f.t.Helper()
	rep, err := f.sj.RunNow(context.Background())
	require.NoError(f.t, err)
	return rep
}

func (f *exportFixture) status(id string) *ExportStatus {
	f.t.Helper()
	st, err := f.svc.Get(context.Background(), id, f.userID)
	require.NoError(f.t, err)
	return st
}

// body reads the export object through the S3 GET handler (test mode: the
// tenant comes from the context) and parses it.
func (f *exportFixture) body(st *ExportStatus) map[string]any {
	f.t.Helper()
	req := httptest.NewRequest("GET", "/"+tenant.ExportsBucket+"/"+st.ObjectKey, nil).WithContext(s3Ctx(context.Background(), f.tenant()))
	rr := httptest.NewRecorder()
	f.server.handleS3Request(rr, req)
	require.Equal(f.t, 200, rr.Code, rr.Body.String())
	for name, secret := range f.secrets {
		assert.NotContains(f.t, rr.Body.String(), secret, "the export body carries the %s", name)
	}
	var out map[string]any
	require.NoError(f.t, json.Unmarshal(rr.Body.Bytes(), &out), "the export is one JSON document")
	return out
}

func (f *exportFixture) exportFiles() []string {
	var files []string
	_ = filepath.Walk(filepath.Join(f.dir, f.tenantID+"_"+tenant.ExportsBucket), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	return files
}

// --- the happy path ---------------------------------------------------------

func TestAccountExport_RequestThenRunWritesOneStreamedObject(t *testing.T) {
	// Arrange
	f := setupExportFixture(t)
	f.seed()

	// Act: request, then the job's run.
	id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
	assert.Equal(t, "pending", f.status(id).Status, "the request only records the row")
	rep := f.run()

	// Assert: the row, the system bucket, the head row, the object.
	assert.Equal(t, int64(1), rep.Rows)
	st := f.status(id)
	require.Equal(t, "completed", st.Status, st.Error)
	assert.False(t, st.Expired)
	assert.WithinDuration(t, time.Now().Add(exportTTL), st.ExpiresAt, time.Minute)
	assert.Greater(t, st.SizeBytes, int64(0))
	assert.NotEmpty(t, st.ETag)
	assert.True(t, strings.HasPrefix(st.ObjectKey, "account-export-"), st.ObjectKey)

	var vis string
	var versioning sql.NullString
	var lock bool
	require.NoError(t, f.db.QueryRow(`SELECT visibility, versioning_status, COALESCE(object_lock_enabled, FALSE) FROM buckets WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, tenant.ExportsBucket).Scan(&vis, &versioning, &lock), "the system bucket has a registry row")
	assert.Equal(t, "private", vis)
	assert.False(t, lock, "never lock-enabled: the retention delete must not be refused")
	assert.NotEqual(t, "Enabled", versioning.String, "never versioned: a delete leaves nothing behind")

	var size int64
	var etag, backend, ctype, floor string
	require.NoError(t, f.db.QueryRow(`SELECT size_bytes, etag, backend_name, content_type, COALESCE(floor,'') FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, tenant.ExportsBucket, st.ObjectKey).Scan(&size, &etag, &backend, &ctype, &floor), "the export is a head row like any object")
	assert.Equal(t, st.SizeBytes, size)
	assert.Equal(t, st.ETag, etag, "the row's etag is the proof of a whole object")
	assert.Equal(t, "local", backend)
	assert.Equal(t, "application/json", ctype)
	assert.Equal(t, "standard", floor, "billed on the standard floor")
	require.Len(t, f.exportFiles(), 1, "exactly one blob")

	body := f.body(st)
	for _, section := range []string{"user", "tenant", "quota", "buckets", "objects", "object_versions", "object_locks",
		"api_keys", "bandwidth_usage", "events", "oauth_accounts", "sessions", "webhooks", "signup", "exports"} {
		assert.Contains(t, body, section, "section %s", section)
	}
	user := body["user"].(map[string]any)
	assert.Equal(t, f.email, user["email"])
	assert.NotContains(t, user, "password_hash")
	assert.Equal(t, "https://ref.example", user["signup_referrer"])
	tn := body["tenant"].(map[string]any)
	assert.Equal(t, "house", tn["plan"])
	assert.Equal(t, "annual", tn["house_period"])
	assert.NotContains(t, tn, "secret_key")
	quota := body["quota"].(map[string]any)
	assert.Len(t, quota["floors"], 2)
	house := quota["house"].(map[string]any)
	assert.EqualValues(t, 2, house["standard_tb"])
	assert.EqualValues(t, 1, house["vault_tb"])

	objects := body["objects"].([]any)
	require.Len(t, objects, 5, "all five objects, through pages of 2 that cross the bucket boundary")
	var keys []string
	for _, o := range objects {
		m := o.(map[string]any)
		keys = append(keys, m["bucket"].(string)+"/"+m["key"].(string))
		assert.NotContains(t, m, "backend", "placement is internal")
	}
	assert.Equal(t, []string{f.buckets[0] + "/a/1.txt", f.buckets[0] + "/a/2.txt", f.buckets[0] + "/z/3.txt", f.buckets[1] + "/b/4.txt", f.buckets[1] + "/b/5.txt"}, keys)
	first := objects[0].(map[string]any)
	assert.Equal(t, "STANDARD", first["storage_class"], "the class the customer sees")
	assert.Equal(t, engine.CustomerStorageClass("vault", "geyser"), objects[2].(map[string]any)["storage_class"], "an attic object reports its backend's class")
	assert.Equal(t, "GLACIER", objects[2].(map[string]any)["storage_class"])
	assert.Equal(t, map[string]any{"x-amz-meta-owner": "me"}, first["metadata"])
	assert.Equal(t, map[string]any{"env": "prod"}, first["tags"])

	buckets := body["buckets"].([]any)
	require.Len(t, buckets, 2, "the system bucket is not listed")
	b0 := buckets[0].(map[string]any)
	assert.Equal(t, "us-central-1", b0["region"])
	assert.Equal(t, "Enabled", b0["versioning"])
	assert.Equal(t, true, b0["object_lock_enabled"])
	assert.Equal(t, "https://app.example", b0["cors_origins"])

	keysSec := body["api_keys"].([]any)
	require.Len(t, keysSec, 1)
	k := keysSec[0].(map[string]any)
	assert.Equal(t, "ci key", k["name"])
	assert.Equal(t, "VLT_EXP"+f.suffix, k["key_id"])
	assert.NotContains(t, k, "secret_hash")
	assert.NotContains(t, k, "secret_key")
	assert.NotNil(t, k["last_used"])

	oauth := body["oauth_accounts"].([]any)
	require.Len(t, oauth, 1)
	assert.Equal(t, map[string]any{"provider": "github", "linked_at": oauth[0].(map[string]any)["linked_at"]}, oauth[0], "provider only — never the provider's id, e-mail or name")

	sessions := body["sessions"].([]any)
	require.Len(t, sessions, 1)
	assert.Equal(t, "ExportBrowser/1.0", sessions[0].(map[string]any)["user_agent"])
	assert.Equal(t, "203.0.113.9", sessions[0].(map[string]any)["ip"])

	hooks := body["webhooks"].([]any)
	require.Len(t, hooks, 1)
	assert.Equal(t, "https://hooks.example/in", hooks[0].(map[string]any)["url"])
	assert.NotContains(t, hooks[0].(map[string]any), "secret")

	assert.Len(t, body["events"], 3)
	assert.Len(t, body["object_versions"], 1)
	assert.Len(t, body["object_locks"], 1)
	assert.Len(t, body["bandwidth_usage"], 1)
	security := body["security"].(map[string]any)
	assert.Equal(t, true, security["mfa_enabled"])
	assert.Len(t, body["exports"], 1, "the export records themselves, this one included")
	assert.Len(t, body["audit_trail"], 1)
}

func TestAccountExport_ATenantWithNothingStillGetsAnExport(t *testing.T) {
	f := setupExportFixture(t)
	f.exec(`INSERT INTO users (id, email, password_hash, status, created_at, updated_at) VALUES ($1, $2, 'x', 'active', NOW(), NOW())`, f.userID, f.email)
	f.exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, 'Empty', $2, $3, $4)`, f.tenantID, f.email, f.accessKey, f.secretKey)
	f.exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier) VALUES ($1, 1000000, 0, 'free')`, f.tenantID)

	id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
	f.run()

	st := f.status(id)
	require.Equal(t, "completed", st.Status, st.Error)
	body := f.body(st)
	assert.Equal(t, []any{}, body["objects"], "an empty array, not null")
	assert.Equal(t, []any{}, body["buckets"])
	assert.Equal(t, []any{}, body["quota"].(map[string]any)["floors"], "no floor rows: an empty array")
}

// --- one in flight ----------------------------------------------------------

func TestAccountExport_OneInFlightPerUserIsEnforcedByTheDatabase(t *testing.T) {
	f := setupExportFixture(t)
	f.seed()

	first, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
	_, err = f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.ErrorIs(t, err, ErrExportInFlight, "a second request while one is pending")

	f.run()
	require.Equal(t, "completed", f.status(first).Status)
	second, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err, "a new export may be requested once the last one is done")
	assert.NotEqual(t, first, second)
}

// --- the download -----------------------------------------------------------

func TestAccountExport_DownloadIsAPresignedGETOnThePrimaryPair(t *testing.T) {
	// Arrange
	f := setupExportFixture(t)
	f.seed()
	id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
	f.run()
	st := f.status(id)
	require.Equal(t, "completed", st.Status, st.Error)

	// Act
	link, expires, err := f.svc.DownloadURL(context.Background(), id, f.userID)
	require.NoError(t, err)

	// Assert: the URL's shape — the public endpoint, the system bucket, the
	// primary key id, one hour — and that it serves the object through the
	// real presigned verifier (not test mode).
	u, err := url.Parse(link)
	require.NoError(t, err)
	assert.Equal(t, "s3.test.local", u.Host)
	assert.Equal(t, "/"+tenant.ExportsBucket+"/"+st.ObjectKey, u.Path)
	assert.True(t, strings.HasPrefix(u.Query().Get("X-Amz-Credential"), f.accessKey+"/"), u.Query().Get("X-Amz-Credential"))
	assert.Equal(t, "3600", u.Query().Get("X-Amz-Expires"))
	assert.WithinDuration(t, time.Now().Add(time.Hour), expires, time.Minute)
	assert.NotContains(t, link, f.secretKey)

	live := &Server{logger: zap.NewNop(), router: chi.NewRouter(), engine: f.eng, db: f.db, testMode: false}
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", u.RequestURI(), nil)
		req.Host = u.Host
		rr := httptest.NewRecorder()
		live.handleS3Request(rr, req)
		return rr
	}
	rr := get()
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assert.Equal(t, st.SizeBytes, int64(rr.Body.Len()))

	// Adversarial: rotate the primary pair — the URL must stop working.
	f.exec(`UPDATE tenants SET access_key = $1, secret_key = $2 WHERE id = $3`, f.accessKey+"NEW", f.secretKey+"NEW", f.tenantID)
	rr = get()
	assert.Equal(t, 403, rr.Code, "a presigned URL of a rotated key is dead: %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), "AccessDenied")
}

func TestAccountExport_AnotherUsersExportIsNotFound(t *testing.T) {
	f := setupExportFixture(t)
	f.seed()
	id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
	f.run()

	other := uuid.New().String()
	_, err = f.svc.Get(context.Background(), id, other)
	assert.ErrorIs(t, err, ErrExportNotFound, "never 403: existence is not confirmed")
	_, _, err = f.svc.DownloadURL(context.Background(), id, other)
	assert.ErrorIs(t, err, ErrExportNotFound)
	_, err = f.svc.Get(context.Background(), "not-a-uuid", f.userID)
	assert.ErrorIs(t, err, ErrExportNotFound)
}

// --- expiry -----------------------------------------------------------------

func TestAccountExport_ExpiredIsGoneAndRetentionDeletesTheObject(t *testing.T) {
	// Arrange: a completed export whose 7 days have passed.
	f := setupExportFixture(t)
	f.seed()
	id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
	f.run()
	st := f.status(id)
	require.Equal(t, "completed", st.Status)
	var usedBefore int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.tenantID).Scan(&usedBefore))
	assert.Equal(t, int64(4096)+st.SizeBytes, usedBefore, "the export counted against the quota")
	f.exec(`UPDATE account_exports SET expires_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, id)

	// The status says expired; no URL is minted.
	st = f.status(id)
	assert.True(t, st.Expired)
	_, _, err = f.svc.DownloadURL(context.Background(), id, f.userID)
	assert.ErrorIs(t, err, ErrExportExpired)

	// Act: the retention step.
	n, err := f.svc.PurgeExpired(context.Background())
	require.NoError(t, err)

	// Assert: object gone through the customer delete path, row kept as 'expired'.
	assert.Equal(t, int64(1), n)
	var cnt int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`, f.tenantID, tenant.ExportsBucket).Scan(&cnt))
	assert.Zero(t, cnt, "head row gone")
	assert.Empty(t, f.exportFiles(), "blob gone")
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM account_exports WHERE id = $1`, id).Scan(&status))
	assert.Equal(t, "expired", status, "the row stays for the audit trail")
	var usedAfter int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.tenantID).Scan(&usedAfter))
	assert.Equal(t, int64(4096), usedAfter, "quota released")

	// A second pass finds nothing; a request after expiry works again.
	n, err = f.svc.PurgeExpired(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
	_, err = f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
}

func TestAccountExport_RetentionOnAnExportWhoseHeadRowIsAlreadyGone(t *testing.T) {
	f := setupExportFixture(t)
	f.seed()
	id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
	f.run()
	f.exec(`UPDATE account_exports SET expires_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, id)
	f.exec(`DELETE FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`, f.tenantID, tenant.ExportsBucket)

	n, err := f.svc.PurgeExpired(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM account_exports WHERE id = $1`, id).Scan(&status))
	assert.Equal(t, "expired", status)
	assert.Empty(t, f.exportFiles(), "the blob is removed even without its row")
}

// --- failures ---------------------------------------------------------------

func TestAccountExport_AFailedRenderStaysPendingAndIsRetried(t *testing.T) {
	// Arrange: the first write fails half-way (the backend refuses).
	f := setupExportFixture(t)
	f.seed()
	id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
	calls := 0
	f.svc.beforeWrite = func() error {
		calls++
		if calls == 1 {
			return errors.New("backend refused the write")
		}
		return nil
	}

	// Act: run 1 fails the export, run 2 completes it.
	rep := f.run()
	assert.Zero(t, rep.Rows)
	assert.Contains(t, rep.Note, "backend refused the write")
	st := f.status(id)
	assert.Equal(t, "pending", st.Status, "never 'completed' without a whole object")
	assert.Equal(t, 1, st.Attempts)
	assert.Empty(t, f.exportFiles(), "no object")
	var cnt int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`, f.tenantID, tenant.ExportsBucket).Scan(&cnt))
	assert.Zero(t, cnt)

	rep = f.run()
	assert.Equal(t, int64(1), rep.Rows)
	st = f.status(id)
	assert.Equal(t, "completed", st.Status, st.Error)
	assert.Equal(t, 2, st.Attempts)
}

func TestAccountExport_ThreeFailuresMarkTheExportFailed(t *testing.T) {
	f := setupExportFixture(t)
	f.seed()
	id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
	f.svc.beforeWrite = func() error { return errors.New("still refused") }

	for i := 0; i < exportMaxAttempts; i++ {
		f.run()
	}
	st := f.status(id)
	assert.Equal(t, "failed", st.Status)
	assert.Equal(t, exportMaxAttempts, st.Attempts)
	assert.Contains(t, st.Error, "still refused")
	rep := f.run()
	assert.Zero(t, rep.Rows, "a failed row is not picked up again")
	_, err = f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err, "the user may ask again")
}

// --- quota ------------------------------------------------------------------

func TestAccountExport_OutOfQuotaStillSucceedsAndIsAudited(t *testing.T) {
	// Arrange: the tenant is full on the standard floor and on the total.
	f := setupExportFixture(t)
	f.seed()
	f.exec(`UPDATE tenant_quotas SET storage_limit_bytes = 4096, storage_used_bytes = 4096 WHERE tenant_id = $1`, f.tenantID)
	f.exec(`UPDATE tenant_floor_quotas SET storage_limit_bytes = 4096, storage_used_bytes = 4096 WHERE tenant_id = $1 AND floor = 'standard'`, f.tenantID)
	id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)

	// Act
	f.run()

	// Assert: completed; the bytes are accounted (not hidden); the audit
	// row says the allowance was used.
	st := f.status(id)
	require.Equal(t, "completed", st.Status, st.Error)
	var used int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.tenantID).Scan(&used))
	assert.Equal(t, int64(4096)+st.SizeBytes, used, "the export is accounted even past the limit")
	var meta []byte
	require.NoError(t, f.db.QueryRow(`SELECT metadata FROM audit_logs WHERE tenant_id = $1 AND action = 'account.export_completed'`, f.tenantID).Scan(&meta))
	var m map[string]any
	require.NoError(t, json.Unmarshal(meta, &m))
	assert.Equal(t, true, m["over_quota"], string(meta))
}

// --- the system bucket ------------------------------------------------------

func TestAccountExport_SystemBucketDoesNotExistToS3ExceptForItsObjects(t *testing.T) {
	f := setupExportFixture(t)
	f.seed()
	id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
	f.run()
	st := f.status(id)
	require.Equal(t, "completed", st.Status, st.Error)

	s3 := func(method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader("x")).WithContext(s3Ctx(context.Background(), f.tenant()))
		req.ContentLength = 1
		rr := httptest.NewRecorder()
		f.server.handleS3Request(rr, req)
		return rr
	}
	rr := s3("GET", "/")
	require.Equal(t, 200, rr.Code)
	assert.NotContains(t, rr.Body.String(), tenant.ExportsBucket, "ListBuckets omits the system bucket")
	assert.Contains(t, rr.Body.String(), f.buckets[0])

	for _, c := range []struct{ method, target string }{
		{"GET", "/" + tenant.ExportsBucket + "?list-type=2"},
		{"GET", "/" + tenant.ExportsBucket + "/"},
		{"HEAD", "/" + tenant.ExportsBucket + "/"},
		{"DELETE", "/" + tenant.ExportsBucket + "/"},
		{"PUT", "/" + tenant.ExportsBucket + "/"},
		{"PUT", "/" + tenant.ExportsBucket + "/new.txt"},
		{"DELETE", "/" + tenant.ExportsBucket + "/" + st.ObjectKey},
		{"GET", "/" + tenant.ExportsBucket + "?versions"},
	} {
		rr := s3(c.method, c.target)
		assert.Equal(t, 404, rr.Code, "%s %s: %s", c.method, c.target, rr.Body.String())
		if c.method != "HEAD" {
			assert.Contains(t, rr.Body.String(), "NoSuchBucket", "%s %s", c.method, c.target)
		}
	}
	rr = s3("GET", "/"+tenant.ExportsBucket+"/"+st.ObjectKey)
	assert.Equal(t, 200, rr.Code, "GetObject of an export works")
	rr = s3("HEAD", "/"+tenant.ExportsBucket+"/"+st.ObjectKey)
	assert.Equal(t, 200, rr.Code, "HeadObject of an export works")
	var cnt int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`, f.tenantID, tenant.ExportsBucket).Scan(&cnt))
	assert.Equal(t, 1, cnt, "nothing was written or deleted")
}

func TestAccountExport_SystemBucketDoesNotCountAgainstTheBucketCap(t *testing.T) {
	f := setupExportFixture(t)
	f.seed()
	id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
	f.run()
	require.Equal(t, "completed", f.status(id).Status)
	n, err := countCustomerBuckets(context.Background(), f.db, f.tenantID)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "two customer buckets; the system bucket is not one of them")
}

// --- erasure ----------------------------------------------------------------

// D-16: a user whose deletion is scheduled may still export; and on the date
// the runner's walk removes the export object with every other object, the
// row with every other row.
func TestAccountExport_ErasureRemovesTheExportObjectAndRow(t *testing.T) {
	d := setupDeletionFixture(t)
	d.seed(time.Now().Add(-time.Hour))
	svc := NewAccountExportService(d.db, d.eng, usage.NewQuotaManager(d.db), crypto.NewGlobalContentIndex(d.db), "http://s3.test.local", zap.NewNop())
	svc.JobName = "test_account_export_del_" + d.suffix
	svc.onlyUserID = d.userID
	t.Cleanup(func() { _, _ = d.db.Exec(`DELETE FROM job_runs WHERE job = $1`, svc.JobName) })
	sj := newJobScheduler(d.db, zap.NewNop()).Register(svc.spec())

	id, err := svc.Request(context.Background(), d.userID, d.tenantID)
	require.NoError(t, err, "pending_deletion blocks nothing (D-16)")
	_, err = sj.RunNow(context.Background())
	require.NoError(t, err)
	st, err := svc.Get(context.Background(), id, d.userID)
	require.NoError(t, err)
	require.Equal(t, "completed", st.Status, st.Error)
	exportBlob := filepath.Join(d.primary, d.tenantID+"_"+tenant.ExportsBucket, st.ObjectKey)
	_, statErr := os.Stat(exportBlob)
	require.NoError(t, statErr, "the export blob exists before the erasure")

	// Act: the erasure, as the scheduler runs it.
	var res AccountDeletionResult
	_, err = d.sj.runWith(context.Background(), func(ctx context.Context) (jobReport, error) {
		r, runErr := d.runner.RunOnce(ctx)
		res = r
		return jobReport{Rows: int64(r.Erased)}, runErr
	})
	require.NoError(t, err, "run: %+v", res)
	require.Len(t, res.Tenants, 1)
	te := res.Tenants[0]

	// Assert
	require.Equal(t, outcomeErased, te.Outcome, "%+v", te)
	_, statErr = os.Stat(exportBlob)
	assert.True(t, os.IsNotExist(statErr), "the export blob is erased with the account")
	assert.Zero(t, d.count(`SELECT COUNT(*) FROM account_exports WHERE tenant_id = $1`, d.tenantID), "no export row survives")
	assert.Zero(t, d.count(`SELECT COUNT(*) FROM buckets WHERE tenant_id = $1`, d.tenantID), "the system bucket row is gone too")
}
