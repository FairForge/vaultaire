package api

// WP-KEY-1 PR B (Stage 1a Prompt 54; F-39, decided by the owner 2026-10-31;
// D-RP88-1/2): per-key and per-bucket budgets.
//
//   - a row names the key that wrote it (written_by_key; an STS token: its
//     parent's; the primary pair: none — it is never capped, F-39 (c));
//   - a key's storage cap is the bytes its rows still HOLD
//     (api_keys.storage_used_bytes), a bucket's the bytes in it
//     (buckets.storage_used_bytes, counted from its first cap), moved by the
//     one ledger beside the tenant's; the reservation checks min(key,
//     bucket, tenant, pool) and a refusal is 403 QuotaExceeded with
//     x-stored-cap naming the first cap that failed (F-39 (b));
//   - an egress cap on a key or a bucket REFUSES when this UTC month's
//     bytes reach it (403, x-stored-cap, Retry-After = the next month's
//     start), with key.cap_warning events at 80 and 95 % (F-39 (a)); the
//     tenant's allowance alone paces (D-25).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/usage"
)

type keyCapFixture struct {
	*requestLogFixture
	keyID, kak, ksk string // a full-access scoped key of the tenant
	primaryID       string
}

func setupKeyCapFixture(t *testing.T) *keyCapFixture {
	f := &keyCapFixture{requestLogFixture: setupRequestLogFixture(t)}
	id := uuid.NewString()
	f.keyID, f.kak, f.ksk = id, "VKCAP"+strings.ToUpper(id[:8]), "SKCAP"+id
	f.exec(`INSERT INTO api_keys (id, user_id, tenant_id, is_primary, name, key_id, secret_hash, secret_key, permissions)
		VALUES ($1, $2, $3, FALSE, 'capped', $4, 'h', $5, '["*"]')`, f.keyID, f.userID, f.tenantID, f.kak, f.ksk)
	require.NoError(t, f.db.QueryRow(`SELECT id::text FROM api_keys WHERE tenant_id = $1 AND is_primary`, f.tenantID).Scan(&f.primaryID))
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM sts_tokens WHERE tenant_id = $1`, f.tenantID) })
	return f
}

// put / get / del as the scoped key (k) or the primary (p).
func (f *keyCapFixture) put(ak, sk, key string, n int) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(http.MethodPut, "/"+f.bucket+"/"+key, []byte(strings.Repeat("x", n)), ak, sk, nil)
}

// mgmt calls a management handler as the tenant (a person: no scope).
func (f *keyCapFixture) mgmt(h http.HandlerFunc, method, path string, params map[string]string, body any) (int, map[string]any) {
	f.t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	ctx := context.WithValue(req.Context(), tenantIDKey, f.tenantID)
	ctx = context.WithValue(ctx, userIDKey, f.userID)
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	req = req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rc))
	w := httptest.NewRecorder()
	h(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (f *keyCapFixture) keyCaps(keyID string, storage, egress any) (int, map[string]any) {
	f.t.Helper()
	return f.mgmt(f.srv.handleMgmtSetKeyCaps, http.MethodPut, "/api/v1/manage/keys/"+keyID+"/caps", map[string]string{"id": keyID},
		map[string]any{"storage_limit_bytes": storage, "egress_limit_bytes": egress})
}

func (f *keyCapFixture) bucketCaps(storage, egress any) (int, map[string]any) {
	f.t.Helper()
	return f.mgmt(f.srv.handleMgmtSetBucketCaps, http.MethodPut, "/api/v1/manage/buckets/"+f.bucket+"/caps", map[string]string{"name": f.bucket},
		map[string]any{"storage_limit_bytes": storage, "egress_limit_bytes": egress})
}

func (f *keyCapFixture) writer(table, key string) string {
	f.t.Helper()
	var w *string
	require.NoError(f.t, f.db.QueryRow(`SELECT written_by_key::text FROM `+table+` WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		ORDER BY 1 NULLS LAST LIMIT 1`, f.tenantID, f.bucket, key).Scan(&w))
	if w == nil {
		return ""
	}
	return *w
}

func (f *keyCapFixture) keyUsed(id string) int64 {
	var n int64
	require.NoError(f.t, f.db.QueryRow(`SELECT storage_used_bytes FROM api_keys WHERE id = $1`, id).Scan(&n))
	return n
}

func (f *keyCapFixture) bucketUsed() *int64 {
	var n *int64
	require.NoError(f.t, f.db.QueryRow(`SELECT storage_used_bytes FROM buckets WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket).Scan(&n))
	return n
}

// truth: the counters agree with the rows (usage's reconcile rule).
func (f *keyCapFixture) assertCountersAreTheRows() {
	f.t.Helper()
	qm := usage.NewQuotaManager(f.db)
	held, err := qm.KeyHeldBytes(context.Background(), f.keyID)
	require.NoError(f.t, err)
	assert.Equal(f.t, held, f.keyUsed(f.keyID), "the key's counter is what its rows hold")
	if b := f.bucketUsed(); b != nil {
		in, err := qm.BucketHeldBytes(context.Background(), f.tenantID, f.bucket)
		require.NoError(f.t, err)
		assert.Equal(f.t, in, *b, "the bucket's counter is what it holds")
	}
}

func capOf(w *httptest.ResponseRecorder) string { return w.Header().Get("x-stored-cap") }

func TestKeyCap_RowsNameTheirWriter(t *testing.T) {
	f := setupKeyCapFixture(t)
	require.Equal(t, http.StatusOK, f.put(f.kak, f.ksk, "a", 10).Code)
	assert.Equal(t, f.keyID, f.writer("object_head_cache", "a"), "the scoped key wrote it")
	require.Equal(t, http.StatusOK, f.put(f.ak, f.sk, "b", 10).Code)
	assert.Empty(t, f.writer("object_head_cache", "b"), "the primary is counted to no key (F-39 (c))")

	// Versioned: the version row names its writer, and keeps it when
	// another key overwrites the object.
	f.exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	require.Equal(t, http.StatusOK, f.put(f.kak, f.ksk, "c", 10).Code)
	assert.Equal(t, f.keyID, f.writer("object_versions", "c"))
	require.Equal(t, http.StatusOK, f.put(f.ak, f.sk, "c", 12).Code)
	var retained int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_versions WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'c'
		AND NOT is_latest AND written_by_key = $3`, f.tenantID, f.bucket, f.keyID).Scan(&retained))
	assert.Equal(t, 1, retained, "the retained version is still the key's")
	assert.EqualValues(t, 20, f.keyUsed(f.keyID), "a + the retained c")

	// Multipart: the upload names the key that created it, and the object
	// is the upload's whoever completes it.
	w := f.do(http.MethodPost, "/"+f.bucket+"/m?uploads", nil, f.kak, f.ksk, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	uploadID := between(w.Body.String(), "<UploadId>", "</UploadId>")
	require.NotEmpty(t, uploadID)
	var up string
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(written_by_key::text, '') FROM multipart_uploads WHERE upload_id = $1`, uploadID).Scan(&up))
	assert.Equal(t, f.keyID, up)
	w = f.do(http.MethodPut, "/"+f.bucket+"/m?partNumber=1&uploadId="+uploadID, []byte("part-one"), f.kak, f.ksk, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.EqualValues(t, 28, f.keyUsed(f.keyID), "the part is held on the upload's key")
	etag := w.Header().Get("ETag")
	body := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + etag + `</ETag></Part></CompleteMultipartUpload>`
	w = f.do(http.MethodPost, "/"+f.bucket+"/m?uploadId="+uploadID, []byte(body), f.ak, f.sk, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, f.keyID, f.writer("object_head_cache", "m"), "completed by the primary, the upload's key wrote it")
	assert.EqualValues(t, 28, f.keyUsed(f.keyID))
	f.assertCountersAreTheRows()
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return ""
}

func TestKeyCap_PutRefusedPastTheKeysStorageCap(t *testing.T) {
	f := setupKeyCapFixture(t)
	code, out := f.keyCaps(f.keyID, 100, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.EqualValues(t, 100, out["storage_limit_bytes"])

	require.Equal(t, http.StatusOK, f.put(f.kak, f.ksk, "one", 60).Code)
	w := f.put(f.kak, f.ksk, "two", 60)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "<Code>QuotaExceeded</Code>")
	assert.Equal(t, "key", capOf(w))
	assert.EqualValues(t, 60, f.keyUsed(f.keyID), "the refusal reserved nothing")
	_, ok := f.head("two")
	assert.False(t, ok)

	// The same key overwriting its own object at the edge of its cap: net
	// of what the commit releases (R10-20's rule, per key too).
	require.Equal(t, http.StatusOK, f.put(f.kak, f.ksk, "one", 90).Code)
	assert.EqualValues(t, 90, f.keyUsed(f.keyID))
	// The primary is never capped, and writes the tenant's bytes beside it.
	require.Equal(t, http.StatusOK, f.put(f.ak, f.sk, "big", 500).Code)
	f.assertCountersAreTheRows()

	// Lifting the cap lifts the refusal.
	code, out = f.keyCaps(f.keyID, nil, nil)
	require.Equal(t, http.StatusOK, code, out)
	require.Equal(t, http.StatusOK, f.put(f.kak, f.ksk, "two", 60).Code)
	assert.EqualValues(t, 150, f.keyUsed(f.keyID))
}

func TestKeyCap_403NamesTheCap(t *testing.T) {
	f := setupKeyCapFixture(t)
	require.Equal(t, http.StatusOK, f.put(f.ak, f.sk, "seed", 25).Code)
	code, out := f.bucketCaps(50, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.EqualValues(t, 25, *f.bucketUsed(), "the bucket's counter is seeded at its first cap")

	w := f.put(f.ak, f.sk, "b", 30)
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "bucket", capOf(w), "the primary is never capped, the bucket is")

	// Both fail: the key is named first (key, bucket, tenant, pool).
	code, out = f.keyCaps(f.keyID, 10, nil)
	require.Equal(t, http.StatusOK, code, out)
	w = f.put(f.kak, f.ksk, "k", 30)
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "key", capOf(w))

	// The tenant's own limit.
	_, _ = f.bucketCaps(nil, nil)
	f.exec(`UPDATE tenant_quotas SET storage_limit_bytes = storage_used_bytes + 5 WHERE tenant_id = $1`, f.tenantID)
	w = f.put(f.ak, f.sk, "t", 30)
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "tenant", capOf(w))

	// A platform reserves in its pool.
	f.exec(`UPDATE tenants SET platform = TRUE WHERE id = $1`, f.tenantID)
	w = f.put(f.ak, f.sk, "p", 30)
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "pool", capOf(w))
	f.exec(`UPDATE tenants SET platform = FALSE WHERE id = $1`, f.tenantID)
}

func TestKeyCap_DeleteByAnotherKeyReleasesTheWriters(t *testing.T) {
	f := setupKeyCapFixture(t)
	_, _ = f.bucketCaps(1<<20, nil)
	require.Equal(t, http.StatusOK, f.put(f.kak, f.ksk, "x", 40).Code)
	require.Equal(t, http.StatusOK, f.put(f.kak, f.ksk, "y", 30).Code)
	assert.EqualValues(t, 70, f.keyUsed(f.keyID))
	assert.EqualValues(t, 70, *f.bucketUsed())

	w := f.do(http.MethodDelete, "/"+f.bucket+"/x", nil, f.ak, f.sk, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.EqualValues(t, 30, f.keyUsed(f.keyID), "deleted by the primary, released on the key that wrote it")
	assert.EqualValues(t, 30, *f.bucketUsed())

	require.Equal(t, http.StatusOK, f.put(f.ak, f.sk, "y", 10).Code)
	assert.EqualValues(t, 0, f.keyUsed(f.keyID), "overwritten by another key: released on its writer")
	assert.EqualValues(t, 10, *f.bucketUsed())
	f.assertCountersAreTheRows()

	// An abort gives a part back to the upload's key.
	w = f.do(http.MethodPost, "/"+f.bucket+"/m?uploads", nil, f.kak, f.ksk, nil)
	uploadID := between(w.Body.String(), "<UploadId>", "</UploadId>")
	require.Equal(t, http.StatusOK, f.do(http.MethodPut, "/"+f.bucket+"/m?partNumber=1&uploadId="+uploadID, []byte("12345"), f.kak, f.ksk, nil).Code)
	assert.EqualValues(t, 5, f.keyUsed(f.keyID))
	require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, "/"+f.bucket+"/m?uploadId="+uploadID, nil, f.ak, f.sk, nil).Code)
	assert.EqualValues(t, 0, f.keyUsed(f.keyID))
	f.assertCountersAreTheRows()
}

// F-39 (b): the minimum always applies; a larger number is accepted with a
// note. F-39 (c): the primary is never capped.
func TestKeyCap_BucketCapAboveTheTenantsIsANote(t *testing.T) {
	f := setupKeyCapFixture(t)
	code, out := f.bucketCaps(int64(2)<<30, nil)
	require.Equal(t, http.StatusOK, code, out)
	notes, _ := out["notes"].([]any)
	assert.Contains(t, notes, "storage_cap_above_tenant", "1 GiB tenant, 2 GiB bucket: the tenant's decides")

	code, out = f.keyCaps(f.keyID, int64(3)<<30, int64(1)<<40)
	require.Equal(t, http.StatusOK, code, out)
	notes, _ = out["notes"].([]any)
	assert.Contains(t, notes, "storage_cap_above_tenant")

	code, out = f.keyCaps(f.primaryID, 100, nil)
	assert.Equal(t, http.StatusConflict, code, out)
	assert.Equal(t, "primary_never_capped", errCode(out))

	code, out = f.keyCaps(f.keyID, -1, nil)
	assert.Equal(t, http.StatusBadRequest, code, out)
	code, out = f.keyCaps(uuid.NewString(), 100, nil)
	assert.Equal(t, http.StatusNotFound, code, out)
}

func TestKeyCap_EgressCapRefusesAtTheKey(t *testing.T) {
	f := setupKeyCapFixture(t)
	require.Equal(t, http.StatusOK, f.put(f.ak, f.sk, "obj", 60).Code)
	code, out := f.keyCaps(f.keyID, nil, 100)
	require.Equal(t, http.StatusOK, code, out)

	get := func(ak, sk string) *httptest.ResponseRecorder {
		return f.do(http.MethodGet, "/"+f.bucket+"/obj", nil, ak, sk, nil)
	}
	require.Equal(t, http.StatusOK, get(f.kak, f.ksk).Code)
	assert.Zero(t, f.capWarnings(), "60 % warns nothing")
	require.Equal(t, http.StatusOK, get(f.kak, f.ksk).Code, "under the cap when admitted: served whole")
	w := get(f.kak, f.ksk)
	require.Equal(t, http.StatusForbidden, w.Code, "spent: refused")
	assert.Contains(t, w.Body.String(), "<Code>QuotaExceeded</Code>")
	assert.Equal(t, "key", capOf(w))
	ra, err := strconv.Atoi(w.Header().Get("Retry-After"))
	require.NoError(t, err)
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	assert.InDelta(t, next.Sub(now).Seconds(), float64(ra), 5, "Retry-After is the next UTC month's start")

	assert.Equal(t, []int{80, 95}, f.capWarningSteps(), "80 and 95 %, once each")
	_ = get(f.kak, f.ksk)
	assert.Equal(t, []int{80, 95}, f.capWarningSteps(), "never twice in a month")

	// The primary is never capped; the tenant's allowance alone paces it.
	require.Equal(t, http.StatusOK, get(f.ak, f.sk).Code)

	// A bucket's egress cap applies to every key, the primary's too.
	code, out = f.bucketCaps(nil, 100)
	require.Equal(t, http.StatusOK, code, out)
	require.Equal(t, http.StatusOK, get(f.ak, f.sk).Code)
	require.Equal(t, http.StatusOK, get(f.ak, f.sk).Code)
	w = get(f.ak, f.sk)
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "bucket", capOf(w))

	// A new month is a new budget.
	f.exec(`UPDATE api_keys SET egress_month = DATE '2001-01-01' WHERE id = $1`, f.keyID)
	f.exec(`UPDATE buckets SET egress_month = DATE '2001-01-01' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	require.Equal(t, http.StatusOK, get(f.kak, f.ksk).Code)
}

func (f *keyCapFixture) capWarnings() int {
	return len(f.capWarningSteps())
}

func (f *keyCapFixture) capWarningSteps() []int {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT (data->>'threshold_pct')::int FROM events WHERE tenant_id = $1 AND type = 'key.cap_warning'
		ORDER BY 1`, f.tenantID)
	require.NoError(f.t, err)
	defer func() { _ = rows.Close() }()
	out := []int{}
	for rows.Next() {
		var n int
		require.NoError(f.t, rows.Scan(&n))
		out = append(out, n)
	}
	require.NoError(f.t, rows.Err())
	return out
}

// An STS token acts as its parent key: its writes are the key's, its caps
// the key's — it never escapes them.
func TestKeyCap_STSNarrowsNeverWidens(t *testing.T) {
	f := setupKeyCapFixture(t)
	code, out := f.keyCaps(f.keyID, 100, 50)
	require.Equal(t, http.StatusOK, code, out)
	tok, err := auth.GenerateSTSToken(context.Background(), f.db, nil, f.tenantID, f.kak,
		&auth.KeyScope{Permissions: []string{"*"}}, auth.STSRequest{Permissions: []string{"GetObject", "PutObject"}, TTL: 600})
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, f.put(tok.AccessKey, tok.SecretKey, "s1", 60).Code)
	assert.Equal(t, f.keyID, f.writer("object_head_cache", "s1"), "the token's write is its parent's")
	assert.EqualValues(t, 60, f.keyUsed(f.keyID))
	w := f.put(tok.AccessKey, tok.SecretKey, "s2", 60)
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "key", capOf(w), "the parent's storage cap")

	require.Equal(t, http.StatusOK, f.do(http.MethodGet, "/"+f.bucket+"/s1", nil, tok.AccessKey, tok.SecretKey, nil).Code)
	w = f.do(http.MethodGet, "/"+f.bucket+"/s1", nil, f.kak, f.ksk, nil)
	require.Equal(t, http.StatusForbidden, w.Code, "the token's download spent the key's month")
	assert.Equal(t, "key", capOf(w))
}

func TestKeyCap_EdgeAdmitEnforcesIt(t *testing.T) {
	f := setupKeyCapFixture(t)
	code, out := f.keyCaps(f.keyID, 100, nil)
	require.Equal(t, http.StatusOK, code, out)
	f.ak, f.sk = f.kak, f.ksk // the edge fixture signs with the scoped key

	body := []byte(strings.Repeat("e", 60))
	resp := f.admit(f.signed("/"+f.bucket+"/e1", nil, body, nil, ""))
	require.True(t, resp.Edge, "%+v", resp)
	etag := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	_, st := f.commit(okCommit(resp.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, st)
	assert.Equal(t, f.keyID, f.writer("object_head_cache", "e1"), "the edge's write names its key")
	assert.EqualValues(t, 60, f.keyUsed(f.keyID))

	resp = f.admit(f.signed("/"+f.bucket+"/e2", nil, body, nil, ""))
	require.False(t, resp.Edge)
	require.NotNil(t, resp.Refusal)
	assert.Equal(t, http.StatusForbidden, resp.Refusal.Status)
	assert.Equal(t, "key", resp.Refusal.Headers["X-Stored-Cap"], "%+v", resp.Refusal)
	assert.Zero(t, f.pending(), "a refused admit leaves nothing pending")
	assert.EqualValues(t, 60, f.keyUsed(f.keyID))
}
