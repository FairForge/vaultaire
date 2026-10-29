package api

// WP-DX-1 (Stage 1a Prompt 50): test mode. A VLT_TEST_ key resolves to the
// tenant's sandbox; every sandbox write lands on `local` (never a paid
// backend), is never billed or metered, and every response says
// x-stored-mode: test.

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/testutil"
)

// paidDriver is a paid backend that must never be called by a sandbox.
type paidDriver struct {
	t     *testing.T
	name  string
	calls atomic.Int32
}

func (d *paidDriver) hit(op string) error {
	d.calls.Add(1)
	d.t.Errorf("a sandbox request reached the paid backend %s (%s)", d.name, op)
	return fmt.Errorf("paid backend %s called", d.name)
}
func (d *paidDriver) Name() string { return d.name }
func (d *paidDriver) Get(context.Context, string, string) (io.ReadCloser, error) {
	return nil, d.hit("Get")
}
func (d *paidDriver) Put(context.Context, string, string, io.Reader, ...engine.PutOption) error {
	return d.hit("Put")
}
func (d *paidDriver) Delete(context.Context, string, string) error { return d.hit("Delete") }
func (d *paidDriver) List(context.Context, string, string) ([]string, error) {
	return nil, d.hit("List")
}
func (d *paidDriver) Exists(context.Context, string, string) (bool, error) {
	return false, d.hit("Exists")
}
func (d *paidDriver) HealthCheck(context.Context) error { return nil }

// sandboxFixture is the request-log fixture with paid backends as the
// engine's primary and its other drivers, `local` registered, and a
// sandbox of the fixture's tenant with one test key.
type sandboxFixture struct {
	*requestLogFixture
	sandboxID string
	tak, tsk  string
	paid      []*paidDriver
}

func setupSandboxFixture(t *testing.T) *sandboxFixture {
	f := &sandboxFixture{requestLogFixture: setupRequestLogFixture(t)}
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	for _, n := range []string{"idrive", "lyve", "geyser"} {
		d := &paidDriver{t: t, name: n}
		f.paid = append(f.paid, d)
		eng.AddDriver(n, d)
	}
	eng.SetPrimary("idrive")
	eng.AddDriver("local", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	eng.AddDriver("edgefake", f.drv)
	f.srv.engine = eng
	f.sandboxID = "sandbox-" + uuid.NewString()[:12]
	id := uuid.NewString()
	f.tak, f.tsk = "VLT_TEST_"+strings.ToUpper(strings.ReplaceAll(id, "-", ""))[:20], "SKT"+id
	f.exec(`INSERT INTO tenants (id, name, email, sandbox_of) VALUES ($1, 'test data', $2, $3)`, f.sandboxID, "sandbox+"+f.tenantID+"@sandbox.invalid", f.tenantID)
	f.exec(`INSERT INTO api_keys (id, user_id, tenant_id, name, key_id, secret_hash, secret_key) VALUES ($1, $2, $3, 'test', $4, 'h', $5)`,
		uuid.NewString(), f.userID, f.sandboxID, f.tak, f.tsk)
	f.exec(`INSERT INTO tenant_quotas (tenant_id) VALUES ($1)`, f.sandboxID)
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM request_log WHERE tenant_id = $1`, `DELETE FROM s3_access_log WHERE tenant_id = $1`,
			`DELETE FROM edge_admits WHERE tenant_id = $1`, `DELETE FROM retired_blobs WHERE tenant_id = $1`,
			`DELETE FROM events WHERE tenant_id = $1`, `DELETE FROM object_versions WHERE tenant_id = $1`,
			`DELETE FROM multipart_parts WHERE upload_id IN (SELECT upload_id FROM multipart_uploads WHERE tenant_id = $1)`,
			`DELETE FROM multipart_uploads WHERE tenant_id = $1`, `DELETE FROM object_head_cache WHERE tenant_id = $1`,
			`DELETE FROM bandwidth_usage_daily WHERE tenant_id = $1`, `DELETE FROM buckets WHERE tenant_id = $1`,
			`DELETE FROM quota_usage_events WHERE tenant_id = $1`, `DELETE FROM tenant_quotas WHERE tenant_id = $1`,
			`DELETE FROM api_keys WHERE tenant_id = $1`, `DELETE FROM tenants WHERE id = $1`,
		} {
			_, _ = f.db.Exec(q, f.sandboxID)
		}
	})
	return f
}

// admitAs is the edge admit of a PUT signed with (ak, sk).
func (f *sandboxFixture) admitAs(ak, sk, path string, body []byte) edgeAdmitResponse {
	f.t.Helper()
	oak, osk := f.ak, f.sk
	f.ak, f.sk = ak, sk
	defer func() { f.ak, f.sk = oak, osk }()
	return f.admit(f.signed(path, nil, body, nil, ""))
}

func (f *sandboxFixture) test(method, path string, body []byte, hdr map[string]string) *http.Response {
	f.t.Helper()
	w := f.do(method, path, body, f.tak, f.tsk, hdr)
	return w.Result()
}

func TestTestKey_NeverReachesAPaidBackend(t *testing.T) {
	f := setupSandboxFixture(t)
	b := "/sbx-" + f.sandboxID[8:16]

	r := f.test(http.MethodPut, b, nil, nil)
	require.Equal(t, http.StatusOK, r.StatusCode)
	assert.Equal(t, "test", r.Header.Get("x-stored-mode"))

	body := []byte(strings.Repeat("t", 4096))
	r = f.test(http.MethodPut, b+"/a.txt", body, nil)
	require.Equal(t, http.StatusOK, r.StatusCode)
	assert.Equal(t, "test", r.Header.Get("x-stored-mode"))

	r = f.test(http.MethodPut, b+"/big.bin", []byte(strings.Repeat("g", 1<<20)), map[string]string{"x-amz-storage-class": "GLACIER"})
	require.Equal(t, http.StatusOK, r.StatusCode)

	r = f.test(http.MethodPut, b+"/c.txt", nil, map[string]string{"x-amz-copy-source": b + "/a.txt"})
	require.Equal(t, http.StatusOK, r.StatusCode)

	// A multipart upload.
	r = f.test(http.MethodPost, b+"/m.bin?uploads", nil, nil)
	require.Equal(t, http.StatusOK, r.StatusCode)
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	raw, _ := io.ReadAll(r.Body)
	require.NoError(t, xml.Unmarshal(raw, &init))
	part := []byte(strings.Repeat("p", 5<<20))
	r = f.test(http.MethodPut, b+"/m.bin?partNumber=1&uploadId="+init.UploadID, part, nil)
	require.Equal(t, http.StatusOK, r.StatusCode)
	etag := r.Header.Get("ETag")
	complete := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, etag)
	r = f.test(http.MethodPost, b+"/m.bin?uploadId="+init.UploadID, []byte(complete), nil)
	require.Equal(t, http.StatusOK, r.StatusCode)

	r = f.test(http.MethodGet, b+"/c.txt", nil, nil)
	require.Equal(t, http.StatusOK, r.StatusCode)
	got, _ := io.ReadAll(r.Body)
	assert.Equal(t, body, got)

	// Every row the sandbox wrote names `local`.
	rows, err := f.db.Query(`SELECT object_key, backend_name FROM object_head_cache WHERE tenant_id = $1`, f.sandboxID)
	require.NoError(t, err)
	n := 0
	for rows.Next() {
		var k, be string
		require.NoError(t, rows.Scan(&k, &be))
		assert.Equal(t, "local", be, k)
		n++
	}
	require.NoError(t, rows.Err())
	_ = rows.Close()
	assert.Equal(t, 4, n)

	// The edge admit passes a sandbox through: the origin serves it.
	adm := f.admitAs(f.tak, f.tsk, b+"/edge.bin", body)
	assert.False(t, adm.Edge)
	assert.Equal(t, edgeReasonSandbox, adm.Reason)

	r = f.test(http.MethodDelete, b+"/a.txt", nil, nil)
	require.Equal(t, http.StatusNoContent, r.StatusCode)
	for _, d := range f.paid {
		assert.Zero(t, d.calls.Load(), d.name)
	}
}

// TestTestKey_NeverBilled: the parent's quota row and every bandwidth row are
// untouched by the sandbox's writes and reads. The sandbox's own quota row
// counts its bytes — the cap on the `local` disk test data may use (the
// free tier's), never a bill: a sandbox has no entitlement and no payment.
func TestTestKey_NeverBilled(t *testing.T) {
	f := setupSandboxFixture(t)
	f.wireEgress(1<<40, 0, nil)
	b := "/sbx-" + f.sandboxID[8:16]
	var before int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.tenantID).Scan(&before))
	require.Equal(t, http.StatusOK, f.test(http.MethodPut, b, nil, nil).StatusCode)
	body := []byte(strings.Repeat("q", 64<<10))
	require.Equal(t, http.StatusOK, f.test(http.MethodPut, b+"/q.bin", body, nil).StatusCode)
	r := f.test(http.MethodGet, b+"/q.bin", nil, nil)
	require.Equal(t, http.StatusOK, r.StatusCode)
	_, _ = io.Copy(io.Discard, r.Body)
	f.srv.bandwidthTracker.Flush()
	var after, sandboxUsed int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.tenantID).Scan(&after))
	require.NoError(t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.sandboxID).Scan(&sandboxUsed))
	assert.Equal(t, before, after, "the parent's quota untouched")
	assert.Equal(t, int64(len(body)), sandboxUsed, "the sandbox's own allowance counts its bytes")
	var bw int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM bandwidth_usage_daily WHERE tenant_id = ANY($1)`, "{"+f.tenantID+","+f.sandboxID+"}").Scan(&bw))
	assert.Zero(t, bw, "never metered")
}

// A sandbox's objects are never a job's to copy onto a paid backend: the
// vault parity job's unprotected rows and the mirror backfill skip them
// (smart demotion takes only rows on the hot backend; a sandbox's are on
// `local`).
func TestSandbox_NoJobCopiesItToAPaidBackend(t *testing.T) {
	f := setupSandboxFixture(t)
	f.exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, 'sbjobs', 'private')`, f.sandboxID)
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor) VALUES ($1, 'sbjobs', 'v', 10, 'e', 'local', 'vault')`, f.sandboxID)
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor) VALUES ($1, 'sbjobs', 's', 10, 'e', 'local', 'standard')`, f.sandboxID)

	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) `+unprotectedFrom, f.sandboxID).Scan(&n))
	assert.Zero(t, n, "vault parity never protects a sandbox object onto its leg")

	f.addMirrorLeg()
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeMirror, f.sandboxID, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	j := f.copiesJob()
	j.onlyTenant = f.sandboxID
	_, err := j.RunOnce(context.Background())
	require.NoError(t, err)
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM blob_copies WHERE tenant_id = $1`, f.sandboxID).Scan(&n))
	assert.Zero(t, n, "the mirror backfill never enqueues a sandbox object")
	_, _ = f.db.Exec(`DELETE FROM feature_flags WHERE tenant_id = $1`, f.sandboxID)
}

// POST /api/v1/manage/keys with "test": true mints a VLT_TEST_ key on the
// tenant's sandbox (created on the first one); the answer says so.
func TestMgmtCreateKey_TestKeyOnTheSandbox(t *testing.T) {
	db := setupSandboxDB(t)
	authSvc := auth.NewAuthService(nil, db)
	user, _, _, err := authSvc.CreateUserWithTenant(context.Background(), "mgmt-testkey-"+uuid.NewString()[:8]+"@stored.ge", "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	t.Cleanup(func() { cleanupSandboxOf(db, user.TenantID) })
	s := &Server{logger: zap.NewNop(), auth: authSvc, db: db, config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/manage/keys", strings.NewReader(`{"name":"ci","test":true}`))
	ctx := context.WithValue(req.Context(), userIDKey, user.ID)
	req = req.WithContext(context.WithValue(ctx, tenantIDKey, user.TenantID))
	w := httptest.NewRecorder()
	s.handleMgmtCreateKey(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, strings.HasPrefix(resp["key"].(string), auth.TestKeyPrefix))
	assert.Equal(t, false, resp["livemode"])
	sb, err := auth.SandboxOf(context.Background(), db, user.TenantID)
	require.NoError(t, err)
	assert.NotEmpty(t, sb)
	assert.True(t, isSandbox(context.Background(), db, sb))
	assert.False(t, isSandbox(context.Background(), db, user.TenantID))
}

func setupSandboxDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	return db
}

// cleanupSandboxOf removes a tenant's sandbox and the tenant (the tests').
func cleanupSandboxOf(db *sql.DB, parent string) {
	_, _ = db.Exec(`DELETE FROM api_keys WHERE tenant_id IN (SELECT id FROM tenants WHERE sandbox_of = $1)`, parent)
	_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id IN (SELECT id FROM tenants WHERE sandbox_of = $1)`, parent)
	_, _ = db.Exec(`DELETE FROM tenants WHERE sandbox_of = $1`, parent)
	_, _ = db.Exec(`DELETE FROM api_keys WHERE tenant_id = $1`, parent)
	_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, parent)
	_, _ = db.Exec(`DELETE FROM users WHERE email = (SELECT email FROM tenants WHERE id = $1)`, parent)
	_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, parent)
}

// POST /api/v1/manage/sandbox/reset (the parent's call) empties the
// sandbox: every object's bytes off `local`, every object, version, upload
// and bucket row, its counter back to 0; the keys stay.
func TestSandbox_ResetPurgesEverything(t *testing.T) {
	f := setupSandboxFixture(t)
	b := "/sbr-" + f.sandboxID[8:16]
	require.Equal(t, http.StatusOK, f.test(http.MethodPut, b, nil, nil).StatusCode)
	for _, k := range []string{"a", "b", "c"} {
		require.Equal(t, http.StatusOK, f.test(http.MethodPut, b+"/"+k, []byte("data-"+k), nil).StatusCode)
	}
	r := f.test(http.MethodPost, b+"/up.bin?uploads", nil, nil)
	require.Equal(t, http.StatusOK, r.StatusCode)
	local, _ := f.srv.engine.GetDriver("local")
	skA := f.storageKeyOf(b[1:], "a")
	ok, err := local.Exists(context.Background(), f.sandboxID+"_"+b[1:], skA)
	require.NoError(t, err)
	require.True(t, ok)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/manage/sandbox/reset", nil)
	req = req.WithContext(context.WithValue(req.Context(), tenantIDKey, f.tenantID))
	w := httptest.NewRecorder()
	f.srv.handleMgmtSandboxReset(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.EqualValues(t, 3, resp["objects_deleted"])
	assert.EqualValues(t, 1, resp["buckets_deleted"])

	for _, q := range []string{
		`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`,
		`SELECT COUNT(*) FROM object_versions WHERE tenant_id = $1`,
		`SELECT COUNT(*) FROM multipart_uploads WHERE tenant_id = $1 AND status = 'active'`,
		`SELECT COUNT(*) FROM buckets WHERE tenant_id = $1`,
		`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`,
	} {
		var n int
		require.NoError(t, f.db.QueryRow(q, f.sandboxID).Scan(&n))
		assert.Zero(t, n, q)
	}
	ok, err = local.Exists(context.Background(), f.sandboxID+"_"+b[1:], skA)
	require.NoError(t, err)
	assert.False(t, ok, "the bytes are gone from local")
	var keys int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1 AND revoked_at IS NULL`, f.sandboxID).Scan(&keys))
	assert.Equal(t, 1, keys, "the test key stays")

	// A tenant with no sandbox: nothing to reset, 200 with zeros.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/manage/sandbox/reset", nil)
	req = req.WithContext(context.WithValue(req.Context(), tenantIDKey, "no-such-parent"))
	w = httptest.NewRecorder()
	f.srv.handleMgmtSandboxReset(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// The nightly sandbox_purge job removes every sandbox object older than 7
// days (by its last write) and keeps the rest.
func TestSandbox_NightlyPurgeAt7Days(t *testing.T) {
	f := setupSandboxFixture(t)
	b := "/sbn-" + f.sandboxID[8:16]
	require.Equal(t, http.StatusOK, f.test(http.MethodPut, b, nil, nil).StatusCode)
	require.Equal(t, http.StatusOK, f.test(http.MethodPut, b+"/old", []byte("old!"), nil).StatusCode)
	require.Equal(t, http.StatusOK, f.test(http.MethodPut, b+"/new", []byte("new"), nil).StatusCode)
	f.exec(`UPDATE object_head_cache SET updated_at = NOW() - interval '8 days' WHERE tenant_id = $1 AND object_key = 'old'`, f.sandboxID)

	j := newSandboxPurgeJob(f.srv)
	j.onlyTenant = f.sandboxID
	res, err := j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.ObjectsDeleted)
	var keys []string
	rows, err := f.db.Query(`SELECT object_key FROM object_head_cache WHERE tenant_id = $1`, f.sandboxID)
	require.NoError(t, err)
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		keys = append(keys, k)
	}
	require.NoError(t, rows.Err())
	_ = rows.Close()
	assert.Equal(t, []string{"new"}, keys)
	var used int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.sandboxID).Scan(&used))
	assert.Equal(t, int64(3), used, "the counter is what remains")
	spec := j.spec()
	assert.Equal(t, 10, spec.Hour)
	assert.Equal(t, 30, spec.Minute)
}

func (f *sandboxFixture) storageKeyOf(bucket, key string) string {
	f.t.Helper()
	var sk string
	require.NoError(f.t, f.db.QueryRow(`SELECT COALESCE(storage_key, object_key) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.sandboxID, bucket, key).Scan(&sk))
	return sk
}
