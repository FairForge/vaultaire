package api

// WP-LOG-1 (Stage 1a Prompt 49): the developers' request log. Every S3
// request of a tenant — a refusal after authentication included — is one
// request_log row written by the access-log tracker's second sink, with the
// error code and the request id the client was answered; the code's
// suggestion is stored once and joined on read; nothing else is held.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/requestlog"
)

// requestLogFixture is the edge fixture with the tracker wired as NewServer
// wires it, and a second key that may only GetObject.
type requestLogFixture struct {
	*edgeFixture
	tracker  *S3AccessLogTracker
	scopedAK string
	scopedSK string
}

func setupRequestLogFixture(t *testing.T) *requestLogFixture {
	f := &requestLogFixture{edgeFixture: setupEdgeFixture(t)}
	f.tracker = NewS3AccessLogTracker(f.db)
	f.tracker.SetRequestLog(func(tenantID string) bool { return f.fl.Enabled(flagRequestLog, tenantID) })
	f.srv.accessLogTracker = f.tracker
	id := uuid.NewString()
	f.scopedAK, f.scopedSK = "VKRO"+strings.ToUpper(id[:8]), "SKRO"+id
	f.exec(`INSERT INTO api_keys (id, user_id, tenant_id, is_primary, name, key_id, secret_hash, secret_key, permissions)
		VALUES ($1, $2, $3, FALSE, 'read-only', $4, 'h', $5, '["GetObject"]')`, uuid.NewString(), f.userID, f.tenantID, f.scopedAK, f.scopedSK)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM request_log WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM s3_access_log WHERE tenant_id = $1`, f.tenantID)
	})
	return f
}

// do sends one signed S3 request with (ak, sk) through the origin's
// dispatcher and flushes the tracker.
func (f *requestLogFixture) do(method, path string, body []byte, ak, sk string, hdr map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	r, err := http.NewRequest(method, "https://"+f.host+path, bytes.NewReader(body))
	require.NoError(f.t, err)
	r.ContentLength = int64(len(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	sum := sha256.Sum256(body)
	r.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	require.NoError(f.t, v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: ak, SecretAccessKey: sk},
		r, hex.EncodeToString(sum[:]), "s3", "us-east-1", time.Now()))
	r.RemoteAddr = "203.0.113.7:5555"
	w := httptest.NewRecorder()
	f.srv.handleS3Request(w, r)
	f.tracker.Flush()
	return w
}

// manage calls GET /api/v1/manage/requests as the fixture's tenant.
func (f *requestLogFixture) manage(query string) (int, listResponse, []requestlog.Row) {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/manage/requests?"+query, nil)
	r = r.WithContext(context.WithValue(r.Context(), tenantIDKey, f.tenantID))
	w := httptest.NewRecorder()
	f.srv.handleMgmtRequests(w, r)
	var resp listResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	raw, _ := json.Marshal(resp.Data)
	var rows []requestlog.Row
	_ = json.Unmarshal(raw, &rows)
	return w.Code, resp, rows
}

func TestRequestLog_EveryErrorCarriesItsSuggestion(t *testing.T) {
	// Every S3 error code the API answers has its suggestion.
	for code, status := range errorStatusCodes {
		if status >= 400 {
			assert.NotEmpty(t, requestSuggestion(code), "error code %s has no suggestion", code)
		}
	}
	for code := range requestSuggestions {
		_, known := errorStatusCodes[code]
		assert.True(t, known, "suggestion for %s, which the API never answers", code)
	}

	// A deliberate 403: the read-only key PUTs.
	f := setupRequestLogFixture(t)
	w := f.do(http.MethodPut, "/"+f.bucket+"/denied.txt", []byte("no"), f.scopedAK, f.scopedSK, nil)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var answered S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &answered))
	require.Equal(t, ErrAccessDenied, answered.Code)

	code, _, rows := f.manage("")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, rows, 1, "one row for the refused request")
	got := rows[0]
	assert.Equal(t, ErrAccessDenied, got.ErrorCode)
	assert.Equal(t, http.StatusForbidden, got.Status)
	assert.Equal(t, answered.RequestID, got.RequestID, "the request id the client was answered")
	assert.Equal(t, f.scopedAK, got.KeyID)
	assert.Equal(t, "PutObject", got.Op)
	assert.Equal(t, f.bucket, got.Bucket)
	assert.Equal(t, "denied.txt", got.Key)
	assert.Equal(t, "origin", got.Host)
	assert.Equal(t, requestSuggestion(ErrAccessDenied), got.Suggestion, "the code's suggestion, joined")
	assert.NotContains(t, got.Suggestion, "PutObject", "never the response's own suggestion text")
}

func TestRequestLog_NeverHoldsAnSSECKeyOrASecret(t *testing.T) {
	f := setupRequestLogFixture(t)
	// The schema: exactly these columns, none of them a key, a header or a body.
	rows, err := f.db.Query(`SELECT column_name FROM information_schema.columns WHERE table_name = 'request_log' ORDER BY column_name`)
	require.NoError(t, err)
	var cols []string
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		cols = append(cols, c)
	}
	require.NoError(t, rows.Err())
	_ = rows.Close()
	assert.ElementsMatch(t, []string{"id", "tenant_id", "key_id", "logged_at", "op", "bucket", "object_key", "status",
		"error_code", "request_id", "bytes_in", "bytes_out", "host"}, cols, "a new column must be argued here first")
	for _, c := range cols {
		for _, bad := range []string{"secret", "sse", "signature", "authorization", "header", "body", "ip", "agent", "password", "token"} {
			if c == "key_id" || c == "object_key" {
				continue
			}
			assert.NotContains(t, c, bad, "column %s", c)
		}
	}
	// The writer names those columns and nothing else.
	for _, c := range []string{"tenant_id", "key_id", "logged_at", "op", "bucket", "object_key", "status", "error_code", "request_id", "bytes_in", "bytes_out", "host"} {
		assert.Contains(t, requestLogInsertSQL, c)
	}

	// An SSE-C request: the key, its MD5 and the signature appear nowhere in the row.
	sseKey := strings.Repeat("k", 32)
	keyB64 := "a2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2s="
	w := f.do(http.MethodPut, "/"+f.bucket+"/ssec.bin", []byte("secret body"), f.ak, f.sk, map[string]string{
		"x-amz-server-side-encryption-customer-algorithm": "AES256",
		"x-amz-server-side-encryption-customer-key":       keyB64,
		"x-amz-server-side-encryption-customer-key-MD5":   "bogus-md5",
	})
	require.NotZero(t, w.Code)
	var row string
	require.NoError(t, f.db.QueryRow(`SELECT row_to_json(l)::text FROM request_log l WHERE tenant_id = $1 AND object_key = 'ssec.bin'`, f.tenantID).Scan(&row))
	for _, s := range []string{sseKey, keyB64, "bogus-md5", f.sk, "secret body", "203.0.113.7"} {
		assert.NotContains(t, row, s)
	}
}

func TestRequestLog_RetainedSevenDays(t *testing.T) {
	f := setupRequestLogFixture(t)
	var policy *retentionPolicy
	for _, p := range defaultRetentionPolicies() {
		if p.Table == "request_log" {
			p := p
			policy = &p
		}
	}
	require.NotNil(t, policy)
	assert.Equal(t, 7*24*time.Hour, policy.MaxAge)
	assert.Equal(t, "logged_at", policy.Column)
	assert.Equal(t, retentionDelete, policy.Action)
	// The retention job's statement on two rows: the 8-day-old one goes.
	f.exec(`INSERT INTO request_log (tenant_id, logged_at, op, request_id) VALUES ($1, NOW() - interval '8 days', 'GetObject', 'old'), ($1, NOW() - interval '6 days', 'GetObject', 'young')`, f.tenantID)
	_, err := f.db.Exec(`DELETE FROM request_log WHERE tenant_id = $1 AND `+policy.Column+` < NOW() - make_interval(secs => $2)`, f.tenantID, policy.MaxAge.Seconds())
	require.NoError(t, err)
	_, _, rows := f.manage("")
	require.Len(t, rows, 1)
	assert.Equal(t, "young", rows[0].RequestID)
}

func TestRequestLog_SearchByRequestID(t *testing.T) {
	f := setupRequestLogFixture(t)
	for i := range 3 {
		f.do(http.MethodGet, fmt.Sprintf("/%s/missing-%d", f.bucket, i), nil, f.ak, f.sk, nil)
	}
	_, _, all := f.manage("")
	require.Len(t, all, 3)
	want := all[1].RequestID
	require.NotEmpty(t, want)
	code, _, rows := f.manage("request_id=" + want)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, rows, 1)
	assert.Equal(t, want, rows[0].RequestID)
	assert.Equal(t, ErrNoSuchKey, rows[0].ErrorCode)
	assert.Equal(t, requestSuggestion(ErrNoSuchKey), rows[0].Suggestion)

	// The filters: by key and by status.
	f.do(http.MethodPut, "/"+f.bucket+"/x", []byte("x"), f.scopedAK, f.scopedSK, nil)
	_, _, byKey := f.manage("key=" + f.scopedAK)
	require.Len(t, byKey, 1)
	_, _, by404 := f.manage("status=404")
	assert.Len(t, by404, 3)
	_, _, by4xx := f.manage("status=4xx")
	assert.Len(t, by4xx, 4)
	code, _, _ = f.manage("status=teapot")
	assert.Equal(t, http.StatusBadRequest, code)
}

func TestRequestLog_KeysetPaged(t *testing.T) {
	f := setupRequestLogFixture(t)
	for i := range 5 {
		f.exec(`INSERT INTO request_log (tenant_id, logged_at, op, request_id) VALUES ($1, NOW() - make_interval(secs => $2), 'GetObject', $3)`,
			f.tenantID, i, fmt.Sprintf("r%d", i))
	}
	// Two rows share one second: the id breaks the tie.
	f.exec(`INSERT INTO request_log (tenant_id, logged_at, op, request_id) SELECT tenant_id, logged_at, op, 'r2b' FROM request_log WHERE tenant_id = $1 AND request_id = 'r2'`, f.tenantID)
	var seen []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		_, resp, rows := f.manage("limit=2&cursor=" + cursor)
		for _, r := range rows {
			seen = append(seen, r.RequestID)
		}
		if !resp.HasMore {
			break
		}
		cursor = resp.NextCursor
	}
	assert.Equal(t, []string{"r0", "r1", "r2b", "r2", "r3", "r4"}, seen, "newest first, every row once")
}

func TestRequestLog_ErasedWithTheAccount(t *testing.T) {
	f := setupRequestLogFixture(t)
	f.do(http.MethodGet, "/"+f.bucket+"/nope", nil, f.ak, f.sk, nil)
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	require.Equal(t, 1, n)
	var stmt string
	for _, d := range account.Deleted {
		if d.Table == "request_log" {
			require.Equal(t, account.ByTenant, d.Key)
			stmt = d.SQL
		}
	}
	require.NotEmpty(t, stmt, "account.EraseRows deletes request_log")
	_, err := f.db.Exec(stmt, f.tenantID)
	require.NoError(t, err)
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	assert.Zero(t, n)
	_, excluded := exportExcludedTables["request_log"]
	assert.True(t, excluded, "excluded from the export with its reason")
}

func TestRequestLog_FlagOffWritesNothing(t *testing.T) {
	f := setupRequestLogFixture(t)
	require.NoError(t, f.fl.Set(context.Background(), flagRequestLog, f.tenantID, false, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	f.do(http.MethodGet, "/"+f.bucket+"/nope", nil, f.ak, f.sk, nil)
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	assert.Zero(t, n)
	// The bucket log's error rows are written as before.
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM s3_access_log WHERE tenant_id = $1 AND error_code = $2`, f.tenantID, ErrNoSuchKey).Scan(&n))
	assert.Equal(t, 1, n, "the bucket log's error row, now with its code")
}

// RP78-3: one request id per S3 request — the dispatcher's — on every
// response (success or error, every op) and in its request_log row. AWS
// answers x-amz-request-id on every response, and that id is what a
// support ticket quotes; prod had 28 of 44 origin rows with none.
func TestS3_EveryResponseCarriesTheLoggedRequestID(t *testing.T) {
	f := setupRequestLogFixture(t)
	b := "/" + f.bucket
	type call struct {
		name, method, path string
		body               []byte
		hdr                map[string]string
		ro                 bool // the read-only key
	}
	calls := []call{
		{name: "PutObject", method: http.MethodPut, path: b + "/a.txt", body: []byte("hello")},
		{name: "PutObject denied", method: http.MethodPut, path: b + "/d.txt", body: []byte("no"), ro: true},
		{name: "HeadObject", method: http.MethodHead, path: b + "/a.txt"},
		{name: "HeadObject missing", method: http.MethodHead, path: b + "/missing.txt"},
		{name: "GetObject", method: http.MethodGet, path: b + "/a.txt"},
		{name: "GetObject missing", method: http.MethodGet, path: b + "/missing.txt"},
		{name: "CopyObject", method: http.MethodPut, path: b + "/c.txt", hdr: map[string]string{"x-amz-copy-source": b + "/a.txt"}},
		{name: "CopyObject missing", method: http.MethodPut, path: b + "/c2.txt", hdr: map[string]string{"x-amz-copy-source": b + "/missing.txt"}},
		{name: "ListObjectsV2", method: http.MethodGet, path: b + "?list-type=2"},
		{name: "ListObjectsV2 no bucket", method: http.MethodGet, path: "/nosuchbucket-rp783?list-type=2"},
		{name: "HeadBucket", method: http.MethodHead, path: b},
		{name: "GetBucketVersioning", method: http.MethodGet, path: b + "?versioning"},
		{name: "CreateMultipartUpload", method: http.MethodPost, path: b + "/m.bin?uploads"},
		{name: "AbortMultipartUpload missing", method: http.MethodDelete, path: b + "/m.bin?uploadId=nosuchupload"},
		{name: "DeleteObject", method: http.MethodDelete, path: b + "/c.txt"},
		{name: "DeleteObject denied", method: http.MethodDelete, path: b + "/a.txt", ro: true},
		{name: "ListBuckets", method: http.MethodGet, path: "/"},
	}
	var answered []string
	for _, c := range calls {
		ak, sk := f.ak, f.sk
		if c.ro {
			ak, sk = f.scopedAK, f.scopedSK
		}
		w := f.do(c.method, c.path, c.body, ak, sk, c.hdr)
		id := w.Header().Get("x-amz-request-id")
		require.NotEmpty(t, id, "%s (%d) answered no x-amz-request-id", c.name, w.Code)
		if w.Code >= 400 && c.method != http.MethodHead {
			var e S3Error
			require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &e), "%s: %s", c.name, w.Body.String())
			assert.Equal(t, id, e.RequestID, "%s: the error body's RequestId is the header's", c.name)
		}
		answered = append(answered, id)
	}
	var logged []string
	rows, err := f.db.Query(`SELECT request_id FROM request_log WHERE tenant_id = $1 ORDER BY logged_at, id`, f.tenantID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		logged = append(logged, id)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, answered, logged, "each response's id is its row's, never empty")
}
