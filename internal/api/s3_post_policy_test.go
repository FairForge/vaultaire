package api

// WP-POST-1 PR A (Stage 1a Prompt 57): browser uploads through S3 presigned
// POST policies. A form POST to /{bucket} whose fields precede `file`
// carries a base64 policy and its SigV4 signature; the policy is verified
// by the one verifier's fourth entrance (auth.ValidatePostPolicy: the one
// credential lookup, the verifier's own key derivation), every condition is
// held against the form, and the file part streams through the PUT's own
// write path — the same placement, quota, ETag and head row.

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5" // #nosec G501 -- the S3 ETag
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// formField is one field of a POST form, in order.
type formField struct{ name, value string }

// postForm is a multipart/form-data body: the fields in order, then the
// file part (S3's order rule), as a browser sends it.
func buildPostForm(t *testing.T, fields []formField, filename string, content []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		require.NoError(t, mw.WriteField(f.name, f.value))
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	h.Set("Content-Type", "application/octet-stream")
	pw, err := mw.CreatePart(h)
	require.NoError(t, err)
	_, err = pw.Write(content)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	return buf.Bytes(), mw.FormDataContentType()
}

// signPolicy is a test's own SigV4 of a policy (the client's side).
func signPolicy(t *testing.T, ak, sk, date, region string, policy map[string]any) []formField {
	t.Helper()
	conds, _ := policy["conditions"].([]any)
	cred := ak + "/" + date[:8] + "/" + region + "/s3/aws4_request"
	conds = append(conds, map[string]any{"x-amz-algorithm": "AWS4-HMAC-SHA256"}, map[string]any{"x-amz-credential": cred},
		map[string]any{"x-amz-date": date})
	policy["conditions"] = conds
	raw, err := json.Marshal(policy)
	require.NoError(t, err)
	b64 := base64.StdEncoding.EncodeToString(raw)
	h := func(k []byte, s string) []byte { m := hmac.New(sha256.New, k); m.Write([]byte(s)); return m.Sum(nil) }
	key := h(h(h(h([]byte("AWS4"+sk), date[:8]), region), "s3"), "aws4_request")
	return []formField{{"x-amz-algorithm", "AWS4-HMAC-SHA256"}, {"x-amz-credential", cred}, {"x-amz-date", date},
		{"policy", b64}, {"x-amz-signature", hex.EncodeToString(h(key, b64))}}
}

func (f *edgeFixture) post(bucket string, body []byte, contentType string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "https://"+f.host+"/"+bucket, bytes.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Origin", "https://upload.example")
	r.ContentLength = int64(len(body))
	r.RemoteAddr = "203.0.113.9:4444"
	return f.serve(r)
}

func (f *edgeFixture) headRow(key string) (etag, contentType string, meta map[string]string, ok bool) {
	f.t.Helper()
	var raw []byte
	err := f.db.QueryRow(`SELECT etag, COALESCE(content_type, ''), COALESCE(metadata, '{}'::jsonb) FROM object_head_cache
		WHERE tenant_id = $1 AND object_key = $2`, f.tenantID, key).Scan(&etag, &contentType, &raw)
	if err != nil {
		return "", "", nil, false
	}
	_ = json.Unmarshal(raw, &meta)
	return etag, contentType, meta, true
}

func (f *edgeFixture) storageUsed() int64 {
	f.t.Helper()
	var n int64
	require.NoError(f.t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	return n
}

func postMD5(b []byte) string {
	sum := md5.Sum(b) // #nosec G401 -- the S3 ETag
	return hex.EncodeToString(sum[:])
}

func postErrCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct{ Code, Message string }
	_ = xml.Unmarshal(w.Body.Bytes(), &e)
	return e.Code
}

type boto3Fixture struct {
	AccessKey string            `json:"access_key"`
	SecretKey string            `json:"secret_key"`
	Bucket    string            `json:"bucket"`
	Fields    map[string]string `json:"fields"`
}

func TestPostPolicy_AcceptsBoto3sGeneratePresignedPost(t *testing.T) {
	f := setupEdgeFixture(t)
	raw, err := os.ReadFile("testdata/post_policy_boto3.json")
	require.NoError(t, err)
	var fx boto3Fixture
	require.NoError(t, json.Unmarshal(raw, &fx))
	// The fixture's own credential and bucket, on the fixture's tenant.
	f.exec(`DELETE FROM api_keys WHERE key_id = $1`, fx.AccessKey)
	f.exec(`INSERT INTO api_keys (id, user_id, tenant_id, is_primary, name, key_id, secret_hash, secret_key) VALUES ($1, $2, $3, FALSE, 'boto3 fixture', $4, 'h', $5)`,
		uuid.NewString(), f.userID, f.tenantID, fx.AccessKey, fx.SecretKey)
	f.exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, $2, 'private')`, f.tenantID, fx.Bucket)
	tr := NewS3AccessLogTracker(f.db)
	tr.SetRequestLog(func(string) bool { return true })
	f.srv.accessLogTracker = tr
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM request_log WHERE tenant_id = $1`, f.tenantID) })

	var fields []formField
	for k, v := range fx.Fields {
		fields = append(fields, formField{k, v})
	}
	content := []byte("\x89PNG\r\n\x1a\n — a cat")
	body, ct := buildPostForm(t, fields, "cat.png", content)
	w := f.post(fx.Bucket, body, ct)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var pr struct {
		XMLName                     xml.Name `xml:"PostResponse"`
		Location, Bucket, Key, ETag string
	}
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &pr), w.Body.String())
	assert.Equal(t, fx.Bucket, pr.Bucket)
	assert.Equal(t, "uploads/cat.png", pr.Key, "${filename} is the file part's name")
	assert.Equal(t, `"`+postMD5(content)+`"`, pr.ETag, "the ETag is the file's MD5")
	assert.Equal(t, `"`+postMD5(content)+`"`, w.Header().Get("ETag"))
	assert.Equal(t, "form", w.Header().Get("x-amz-meta-origin"), "x-amz-meta-* echoed")
	etag, ctype, meta, ok := f.headRow("uploads/cat.png")
	require.True(t, ok, "the head row")
	assert.Equal(t, postMD5(content), strings.Trim(etag, `"`))
	assert.Equal(t, "image/png", ctype, "the form's Content-Type field")
	assert.Equal(t, "form", meta["origin"])
	assert.EqualValues(t, len(content), f.storageUsed(), "billed as a PUT")
	tr.Flush()
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM request_log WHERE tenant_id = $1 AND op = 'PostObject'`, f.tenantID),
		"the access log names the POST")

	// The same form tampered: another key prefix is outside the policy.
	for i := range fields {
		if fields[i].name == "key" {
			fields[i].value = "elsewhere/${filename}"
		}
	}
	body, ct = buildPostForm(t, fields, "cat.png", content)
	w = f.post(fx.Bucket, body, ct)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, "AccessDenied", postErrCode(t, w))
}

func (f *edgeFixture) policyFields(expires time.Time, conds []any, extra ...formField) []formField {
	f.t.Helper()
	pol := map[string]any{"expiration": expires.UTC().Format("2006-01-02T15:04:05.000Z"), "conditions": conds}
	fields := append([]formField{}, extra...)
	return append(fields, signPolicy(f.t, f.ak, f.sk, time.Now().UTC().Format("20060102T150405Z"), "us-east-1", pol)...)
}

func TestPostPolicy_SizeRangeEnforced(t *testing.T) {
	f := setupEdgeFixture(t)
	conds := []any{map[string]any{"bucket": f.bucket}, []any{"starts-with", "$key", "in/"}, []any{"content-length-range", 1, 100}}
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "in/${filename}"})
	before := f.storageUsed()

	body, ct := buildPostForm(t, fields, "big.bin", bytes.Repeat([]byte("x"), 101))
	w := f.post(f.bucket, body, ct)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, "EntityTooLarge", postErrCode(t, w))
	_, _, _, ok := f.headRow("in/big.bin")
	assert.False(t, ok, "nothing written")
	assert.Equal(t, before, f.storageUsed(), "nothing reserved, nothing kept")
	assert.Empty(t, f.drv.objects, "no backend write")

	body, ct = buildPostForm(t, fields, "empty.bin", nil)
	w = f.post(f.bucket, body, ct)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, "EntityTooSmall", postErrCode(t, w))

	body, ct = buildPostForm(t, fields, "ok.bin", bytes.Repeat([]byte("y"), 100))
	w = f.post(f.bucket, body, ct)
	require.Equal(t, http.StatusNoContent, w.Code, "the default success_action_status: %s", w.Body.String())
	assert.Equal(t, before+100, f.storageUsed())

	// A body whose file part is not what its Content-Length says (a tail
	// after the closing delimiter): refused, nothing kept.
	body, ct = buildPostForm(t, fields, "tail.bin", bytes.Repeat([]byte("z"), 50))
	body = append(body, []byte("epilogue")...)
	w = f.post(f.bucket, body, ct)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	_, _, _, ok = f.headRow("in/tail.bin")
	assert.False(t, ok, "a malformed tail never commits")
	assert.Equal(t, before+100, f.storageUsed(), "its reservation released")
}

func TestPostPolicy_ExpiredIs403(t *testing.T) {
	f := setupEdgeFixture(t)
	conds := []any{map[string]any{"bucket": f.bucket}, map[string]any{"key": "late.txt"}}
	fields := f.policyFields(time.Now().Add(-time.Minute), conds, formField{"key", "late.txt"})
	body, ct := buildPostForm(t, fields, "late.txt", []byte("late"))
	w := f.post(f.bucket, body, ct)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, "AccessDenied", postErrCode(t, w))
	assert.Contains(t, w.Body.String(), "Policy expired")
	_, _, _, ok := f.headRow("late.txt")
	assert.False(t, ok)
}

func TestPostPolicy_ContentTypeStartsWith(t *testing.T) {
	f := setupEdgeFixture(t)
	conds := []any{map[string]any{"bucket": f.bucket}, []any{"starts-with", "$key", "pics/"}, []any{"starts-with", "$Content-Type", "image/"},
		map[string]any{"success_action_status": "200"}}
	for _, tc := range []struct {
		ctype string
		code  int
	}{{"image/jpeg", http.StatusOK}, {"text/html", http.StatusForbidden}} {
		fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "pics/${filename}"}, formField{"Content-Type", tc.ctype},
			formField{"success_action_status", "200"})
		body, ct := buildPostForm(t, fields, "a.jpg", []byte("jpeg"))
		w := f.post(f.bucket, body, ct)
		assert.Equal(t, tc.code, w.Code, "%s: %s", tc.ctype, w.Body.String())
	}
	_, ctype, _, ok := f.headRow("pics/a.jpg")
	require.True(t, ok)
	assert.Equal(t, "image/jpeg", ctype)

	// A field the policy does not name is refused (every field but the
	// signature, the policy, file and x-ignore-* must be in the conditions).
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "pics/${filename}"}, formField{"Content-Type", "image/png"},
		formField{"success_action_status", "200"}, formField{"x-amz-meta-sneaky", "1"}, formField{"x-ignore-this", "fine"})
	body, ct := buildPostForm(t, fields, "b.png", []byte("png"))
	w := f.post(f.bucket, body, ct)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	// ${filename} is the file part's base name: a path in it is refused.
	fields = f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "pics/${filename}"}, formField{"Content-Type", "image/png"},
		formField{"success_action_status", "200"})
	for _, bad := range []string{"../escape.png", "a/b.png", `c:\x\d.png`, ".."} {
		body, ct = buildPostForm(t, fields, bad, []byte("png"))
		w = f.post(f.bucket, body, ct)
		assert.Equal(t, http.StatusBadRequest, w.Code, "%q: %s", bad, w.Body.String())
	}

	// success_action_redirect is refused (an open redirect — D-POST-1).
	redirect := append([]any{}, conds...)
	redirect = append(redirect, []any{"starts-with", "$success_action_redirect", ""})
	fields = f.policyFields(time.Now().Add(time.Hour), redirect, formField{"key", "pics/${filename}"}, formField{"Content-Type", "image/png"},
		formField{"success_action_status", "200"}, formField{"success_action_redirect", "https://evil.example/"})
	body, ct = buildPostForm(t, fields, "r.png", []byte("png"))
	w = f.post(f.bucket, body, ct)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	_, _, _, ok = f.headRow("pics/r.png")
	assert.False(t, ok)
}

func TestPostPolicy_ARevokedKeysPolicyIsRefused(t *testing.T) {
	f := setupEdgeFixture(t)
	conds := []any{map[string]any{"bucket": f.bucket}, map[string]any{"key": "k.txt"}}
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "k.txt"})
	f.exec(`UPDATE api_keys SET revoked_at = NOW() WHERE key_id = $1`, f.ak)
	body, ct := buildPostForm(t, fields, "k.txt", []byte("k"))
	w := f.post(f.bucket, body, ct)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, "InvalidAccessKeyId", postErrCode(t, w))
	_, _, _, ok := f.headRow("k.txt")
	assert.False(t, ok)

	// A signature over another policy, of a live key: SignatureDoesNotMatch.
	f.exec(`UPDATE api_keys SET revoked_at = NULL WHERE key_id = $1`, f.ak)
	for i := range fields {
		if fields[i].name == "x-amz-signature" {
			fields[i].value = strings.Repeat("0", 64)
		}
	}
	body, ct = buildPostForm(t, fields, "k.txt", []byte("k"))
	w = f.post(f.bucket, body, ct)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, "SignatureDoesNotMatch", postErrCode(t, w))
}

// The key's IP allow-list holds for a policy as for a PUT.
func TestPostPolicy_AnIPRestrictedKeyFromElsewhereIsRefused(t *testing.T) {
	f := setupEdgeFixture(t)
	f.exec(`UPDATE api_keys SET ip_allowlist = '{198.51.100.1/32}' WHERE key_id = $1`, f.ak)
	conds := []any{map[string]any{"bucket": f.bucket}, map[string]any{"key": "ip.txt"}}
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "ip.txt"})
	body, ct := buildPostForm(t, fields, "ip.txt", []byte("ip"))
	w := f.post(f.bucket, body, ct) // from 203.0.113.9
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, "AccessDenied", postErrCode(t, w))
	_, _, _, ok := f.headRow("ip.txt")
	assert.False(t, ok)
}

// Found by the prod proof (2026-11-03): behind HAProxy and Cloudflare the
// origin sees plain HTTP, and the answer's Location said http://; and the
// request log's row named no key (a form has no Authorization header).
func TestPostPolicy_LocationIsHTTPSAndTheLogNamesTheKey(t *testing.T) {
	f := setupEdgeFixture(t)
	tr := NewS3AccessLogTracker(f.db)
	tr.SetRequestLog(func(string) bool { return true })
	f.srv.accessLogTracker = tr
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM request_log WHERE tenant_id = $1`, f.tenantID) })
	conds := []any{map[string]any{"bucket": f.bucket}, map[string]any{"key": "loc.txt"}, map[string]any{"success_action_status": "201"}}
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "loc.txt"}, formField{"success_action_status", "201"})
	body, ct := buildPostForm(t, fields, "loc.txt", []byte("loc"))
	r := httptest.NewRequest(http.MethodPost, "http://"+f.host+"/"+f.bucket, bytes.NewReader(body)) // as the proxy hands it on
	r.Header.Set("Content-Type", ct)
	r.ContentLength = int64(len(body))
	r.RemoteAddr = "203.0.113.9:4444"
	w := f.serve(r)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, "https://"+f.host+"/"+f.bucket+"/loc.txt", w.Header().Get("Location"))
	assert.Contains(t, w.Body.String(), "<Location>https://")
	tr.Flush()
	var keyID string
	require.NoError(t, f.db.QueryRow(`SELECT key_id FROM request_log WHERE tenant_id = $1 AND op = 'PostObject'`, f.tenantID).Scan(&keyID))
	assert.Equal(t, f.ak, keyID, "the policy's access key")
}
