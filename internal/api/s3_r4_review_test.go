package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review R4 (docs/reviews/R4-bucket-features.md): bucket-level features must
// honour the S3 contracts the rest of the object path relies on — Object Lock
// retention is monotonic, notification targets never point into the private
// network, the CDN never renders scriptable types inline, DeleteBucket looks
// at the objects, listings are complete and in byte order, tags belong to the
// object that was written, conditionals and ranges follow RFC 9110, and a
// presigned URL's lifetime is bounded by the clock, not the signer.

// ---- R4-01 (P0): Object Lock retention rules ---------------------------------

func (f *lockFixture) putRetention(t *testing.T, key, mode string, until time.Time, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`<Retention xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Mode>%s</Mode><RetainUntilDate>%s</RetainUntilDate></Retention>`,
		mode, until.UTC().Format(time.RFC3339))
	req := httptest.NewRequest("PUT", "/"+f.bucket+"/"+key+"?retention", bytes.NewReader([]byte(body)))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req = req.WithContext(f.asPrimaryKey(req.Context()))
	w := httptest.NewRecorder()
	f.server.handlePutObjectRetention(w, req, &S3Request{Bucket: f.bucket, Object: key, TenantID: f.tenantID})
	return w
}

func (f *lockFixture) retentionRow(t *testing.T, key string) (mode string, until time.Time) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`SELECT retention_mode, retain_until_date FROM object_locks WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
		f.tenantID, f.bucket, key).Scan(&mode, &until))
	return mode, until
}

func TestObjectRetention_ComplianceCannotBecomeGovernanceOrShorten(t *testing.T) {
	f := setupLockFixture(t)
	key := "compliance.bin"
	f.putObject(t, key, "RETAINED")
	d2 := time.Now().Add(48 * time.Hour)
	require.Equal(t, http.StatusOK, f.putRetention(t, key, "COMPLIANCE", d2, nil).Code)

	w := f.putRetention(t, key, "GOVERNANCE", d2.Add(-24*time.Hour), nil)
	assert.Equal(t, http.StatusForbidden, w.Code, "COMPLIANCE can never be downgraded to GOVERNANCE")
	w = f.putRetention(t, key, "GOVERNANCE", d2.Add(24*time.Hour), map[string]string{"x-amz-bypass-governance-retention": "true"})
	assert.Equal(t, http.StatusForbidden, w.Code, "not even with a longer date or the bypass header")
	w = f.putRetention(t, key, "COMPLIANCE", d2.Add(-time.Hour), nil)
	assert.Equal(t, http.StatusForbidden, w.Code, "COMPLIANCE cannot be shortened")
	w = f.putRetention(t, key, "COMPLIANCE", d2.Add(24*time.Hour), nil)
	assert.Equal(t, http.StatusOK, w.Code, "extending is the only change allowed")

	mode, until := f.retentionRow(t, key)
	assert.Equal(t, "COMPLIANCE", mode)
	assert.WithinDuration(t, d2.Add(24*time.Hour), until, 2*time.Second)

	w = f.deleteObject(t, key, map[string]string{"x-amz-bypass-governance-retention": "true"})
	assert.Equal(t, http.StatusForbidden, w.Code, "the bypass header means nothing to COMPLIANCE")
}

func TestObjectRetention_GovernanceShortenNeedsBypass(t *testing.T) {
	f := setupLockFixture(t)
	key := "governance.bin"
	f.putObject(t, key, "RETAINED")
	d2 := time.Now().Add(48 * time.Hour)
	require.Equal(t, http.StatusOK, f.putRetention(t, key, "GOVERNANCE", d2, nil).Code)

	assert.Equal(t, http.StatusForbidden, f.putRetention(t, key, "GOVERNANCE", d2.Add(-24*time.Hour), nil).Code,
		"shortening GOVERNANCE without the bypass header is refused")
	assert.Equal(t, http.StatusOK, f.putRetention(t, key, "GOVERNANCE", d2.Add(24*time.Hour), nil).Code,
		"extending never needs the bypass")
	assert.Equal(t, http.StatusOK, f.putRetention(t, key, "GOVERNANCE", d2.Add(-24*time.Hour),
		map[string]string{"x-amz-bypass-governance-retention": "true"}).Code, "shortening with the bypass is allowed")
	assert.Equal(t, http.StatusOK, f.putRetention(t, key, "COMPLIANCE", d2.Add(48*time.Hour), nil).Code,
		"GOVERNANCE may be upgraded to COMPLIANCE without the bypass when the period is not shortened")
	mode, _ := f.retentionRow(t, key)
	assert.Equal(t, "COMPLIANCE", mode)
}

func TestObjectRetention_DateMustBeFutureAndKeyMustExist(t *testing.T) {
	f := setupLockFixture(t)
	f.putObject(t, "real.bin", "x")
	w := f.putRetention(t, "real.bin", "GOVERNANCE", time.Now().Add(-time.Hour), nil)
	assert.Equal(t, http.StatusBadRequest, w.Code, "a retain-until date in the past")
	assert.Contains(t, w.Body.String(), ErrInvalidArgument)

	w = f.putRetention(t, "ghost.bin", "COMPLIANCE", time.Now().Add(time.Hour), nil)
	assert.Equal(t, http.StatusNotFound, w.Code, "retention on a key that does not exist")
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_locks WHERE tenant_id=$1 AND object_key='ghost.bin'`, f.tenantID).Scan(&n))
	assert.Equal(t, 0, n)
}

func TestObjectLockConfig_CannotBeDisabledOnceEnabled(t *testing.T) {
	f := setupLockFixture(t)
	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/"+f.bucket+"?object-lock", strings.NewReader(body))
		req = req.WithContext(s3Ctx(req.Context(), f.tenant))
		w := httptest.NewRecorder()
		f.server.handlePutObjectLockConfiguration(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
		return w
	}
	require.Equal(t, http.StatusOK, put(`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>30</Days></DefaultRetention></Rule></ObjectLockConfiguration>`).Code)

	w := put(`<ObjectLockConfiguration></ObjectLockConfiguration>`)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrInvalidBucketState)

	var enabled bool
	var mode string
	require.NoError(t, f.db.QueryRow(`SELECT object_lock_enabled, default_retention_mode FROM buckets WHERE tenant_id=$1 AND name=$2`, f.tenantID, f.bucket).Scan(&enabled, &mode))
	assert.True(t, enabled)
	assert.Equal(t, "COMPLIANCE", mode, "the defaults survive too")

	assert.Equal(t, http.StatusOK, put(`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`).Code,
		"the default rule may be removed while lock stays enabled")
}

// ---- R4-02 (P1): notification targets never point into the private network --

func TestNotificationTarget_RefusesPrivateLoopbackAndNonHTTP(t *testing.T) {
	f := setupNotificationFixture(t)
	webhookAllowPrivateTargets.Store(false) // the production policy
	put := func(topic string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`<NotificationConfiguration><TopicConfiguration><Topic>%s</Topic><Event>s3:ObjectCreated:*</Event></TopicConfiguration></NotificationConfiguration>`, topic)
		req := httptest.NewRequest("PUT", "/"+f.bucket+"?notification", strings.NewReader(body))
		req = req.WithContext(s3Ctx(req.Context(), f.tenant))
		w := httptest.NewRecorder()
		f.server.handlePutBucketNotification(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
		return w
	}
	for _, bad := range []string{
		"http://127.0.0.1:8000/admin", "http://localhost/x", "http://[::1]/x", "http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/", "http://192.168.1.1/", "http://172.16.0.1/", "http://100.64.0.1/", "http://0.0.0.0/",
		"http://224.0.0.1/", "ftp://example.com/", "file:///etc/passwd", "http://user:pw@example.com/", "https://",
	} {
		w := put(bad)
		assert.Equal(t, http.StatusBadRequest, w.Code, "target %q must be refused: %s", bad, w.Body.String())
	}
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM bucket_notifications WHERE tenant_id=$1`, f.tenantID).Scan(&n))
	assert.Equal(t, 0, n, "nothing stored for a refused target")

	w := put("https://hooks.example.com/s3")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestWebhookClient_RefusesResolvedPrivateAddressAndRedirects(t *testing.T) {
	// A hostname that resolves to loopback at dial time (DNS rebinding) must
	// be refused by the dialer even though the stored URL looked public.
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "http://127.0.0.1:1/never", http.StatusFound)
	}))
	defer srv.Close()

	c := webhookClient(5 * time.Second)
	req, _ := http.NewRequestWithContext(context.Background(), "POST", srv.URL+"/hook", strings.NewReader("{}"))
	_, err := c.Do(req)
	require.Error(t, err, "loopback address refused at dial time")
	assert.Equal(t, 0, hits)

	webhookAllowPrivateTargets.Store(true)
	t.Cleanup(func() { webhookAllowPrivateTargets.Store(false) })
	c = webhookClient(5 * time.Second)
	req, _ = http.NewRequestWithContext(context.Background(), "POST", srv.URL+"/hook", strings.NewReader("{}"))
	resp, err := c.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusFound, resp.StatusCode, "redirects are never followed")
	assert.Equal(t, 1, hits)
}

func TestIsPrivateOrSpecialIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "172.31.255.255": true, "192.168.0.1": true, "169.254.169.254": true,
		"100.64.0.1": true, "0.0.0.0": true, "224.0.0.1": true, "255.255.255.255": true, "::1": true, "fe80::1": true,
		"fc00::1": true, "::ffff:127.0.0.1": true, "8.8.8.8": false, "104.16.0.1": false, "2606:4700::1111": false,
	} {
		assert.Equal(t, want, isPrivateOrSpecialIP(net.ParseIP(ip)), ip)
	}
}

// ---- R4-03 (P1): the CDN never renders scriptable types inline ---------------

func TestCDN_InlineDispositionNeverRendersScriptableTypes(t *testing.T) {
	cases := []struct {
		stored, contentType, want string
	}{
		{"inline", "text/html", `attachment; filename="x.html"`},
		{"inline", "image/svg+xml", `attachment; filename="x.html"`},
		{"inline", "text/xml", `attachment; filename="x.html"`},
		{"", "application/xml", `attachment; filename="x.html"`},
		{"", "application/rss+xml", `attachment; filename="x.html"`},
		{"inline; filename=\"a.html\"", "text/html", `attachment; filename="x.html"`},
		{`attachment; filename="report.html"`, "text/html", `attachment; filename="report.html"`},
		{"inline", "image/png", "inline"},
		{"", "image/png", "inline"},
		{"", "text/plain", "inline"},
		{"", "application/octet-stream", `attachment; filename="x.html"`},
	}
	for _, c := range cases {
		got := cdnContentDisposition(false, c.stored, c.contentType, "dir/x.html")
		assert.Equal(t, c.want, got, "stored=%q type=%q", c.stored, c.contentType)
	}
	assert.False(t, isInlineRenderable("text/xml"))
	assert.False(t, isInlineRenderable("application/xhtml+xml; charset=utf-8"))
	assert.True(t, isInlineRenderable("image/jpeg"))
}

// ---- R4-04 (P1): DeleteBucket is decided by the database ---------------------

func (f *versioningFixture) deleteBucket(t *testing.T, bucket string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("DELETE", "/"+bucket, nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.DeleteBucket(w, req)
	return w
}

func TestDeleteBucket_NonEmptyIs409AndMissingIs404(t *testing.T) {
	f := setupVersioningFixture(t)
	f.putObject(t, "obj.bin", "BYTES")

	w := f.deleteBucket(t, f.bucket)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrBucketNotEmpty)
	var rows int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM buckets WHERE tenant_id=$1 AND name=$2`, f.tenantID, f.bucket).Scan(&rows))
	assert.Equal(t, 1, rows, "the bucket row survives a refused delete")

	// Only a version row left (marker after the object was deleted): still not empty.
	f.setVersioning(t, "Enabled")
	require.Equal(t, http.StatusNoContent, f.deleteObject(t, "obj.bin").Code)
	w = f.deleteBucket(t, f.bucket)
	assert.Equal(t, http.StatusConflict, w.Code, "delete markers / versions count as contents")
	_, err := f.db.Exec(`DELETE FROM object_versions WHERE tenant_id=$1`, f.tenantID)
	require.NoError(t, err)

	// An in-progress multipart upload keeps the bucket too.
	_, err = f.db.Exec(`INSERT INTO multipart_uploads (upload_id, tenant_id, bucket, object_key, status) VALUES ('upload-'||repeat('a',32), $1, $2, 'mp', 'active')`, f.tenantID, f.bucket)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM multipart_uploads WHERE tenant_id=$1`, f.tenantID) })
	w = f.deleteBucket(t, f.bucket)
	assert.Equal(t, http.StatusConflict, w.Code, "an active multipart upload counts as contents")
	_, err = f.db.Exec(`UPDATE multipart_uploads SET status='aborted' WHERE tenant_id=$1`, f.tenantID)
	require.NoError(t, err)

	// Empty: gone from the registry, no marker directory needed.
	w = f.deleteBucket(t, f.bucket)
	assert.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM buckets WHERE tenant_id=$1 AND name=$2`, f.tenantID, f.bucket).Scan(&rows))
	assert.Equal(t, 0, rows)

	w = f.deleteBucket(t, f.bucket)
	assert.Equal(t, http.StatusNotFound, w.Code, "a bucket with no row is NoSuchBucket")
	assert.Contains(t, w.Body.String(), ErrNoSuchBucket)
}

func TestCreateBucket_ReCreateReportsStoredRegion(t *testing.T) {
	f := setupVersioningFixture(t)
	req := httptest.NewRequest("PUT", "/"+f.bucket, strings.NewReader(`<CreateBucketConfiguration><LocationConstraint>eu-west-1</LocationConstraint></CreateBucketConfiguration>`))
	req.ContentLength = int64(len(`<CreateBucketConfiguration><LocationConstraint>eu-west-1</LocationConstraint></CreateBucketConfiguration>`))
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.CreateBucket(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var region string
	require.NoError(t, f.db.QueryRow(`SELECT region FROM buckets WHERE tenant_id=$1 AND name=$2`, f.tenantID, f.bucket).Scan(&region))
	assert.Equal(t, region, w.Header().Get("x-amz-bucket-region"), "the header must describe the stored region, not the request")
	assert.NotEqual(t, "eu-west-1", region, "region is immutable after creation")
}

// ---- R4-05 (P1): listings are complete and in byte order ---------------------

func (f *versioningFixture) seedKeys(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name)
			VALUES ($1, $2, $3, 1, 'e', 'text/plain', 'local') ON CONFLICT DO NOTHING`, f.tenantID, f.bucket, k)
		require.NoError(t, err)
	}
}

func (f *versioningFixture) list(t *testing.T, query string) ListBucketV2Result {
	t.Helper()
	req := httptest.NewRequest("GET", "/"+f.bucket+"?"+query, nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res ListBucketV2Result
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
	return res
}

func keysOf(res ListBucketV2Result) []string {
	var out []string
	for _, c := range res.Contents {
		out = append(out, c.Key)
	}
	for _, p := range res.CommonPrefixes {
		out = append(out, p.Prefix)
	}
	return out
}

func TestListObjects_DelimiterPageBoundaryKeepsEveryKey(t *testing.T) {
	f := setupVersioningFixture(t)
	f.seedKeys(t, "c1", "c2", "c3", "d/x", "d/y", "e")
	p1 := f.list(t, "list-type=2&delimiter=/&max-keys=2")
	assert.Equal(t, []string{"c1", "c2"}, keysOf(p1))
	require.True(t, p1.IsTruncated)
	require.NotEmpty(t, p1.NextContinuationToken)
	p2 := f.list(t, "list-type=2&delimiter=/&max-keys=2&continuation-token="+url.QueryEscape(p1.NextContinuationToken))
	assert.Equal(t, []string{"c3", "d/"}, keysOf(p2), "the boundary key must not be lost")
	require.True(t, p2.IsTruncated)
	p3 := f.list(t, "list-type=2&delimiter=/&max-keys=2&continuation-token="+url.QueryEscape(p2.NextContinuationToken))
	assert.Equal(t, []string{"e"}, keysOf(p3))
	assert.False(t, p3.IsTruncated)
}

func TestListObjects_WidePrefixDoesNotEndTheListing(t *testing.T) {
	f := setupVersioningFixture(t)
	_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name)
		SELECT $1, $2, 'a/'||lpad(g::text, 5, '0'), 1, 'e', 'text/plain', 'local' FROM generate_series(1, 1201) g`, f.tenantID, f.bucket)
	require.NoError(t, err)
	f.seedKeys(t, "b/x", "c1")

	p1 := f.list(t, "list-type=2&delimiter=/&max-keys=1")
	assert.Equal(t, []string{"a/"}, keysOf(p1))
	require.True(t, p1.IsTruncated, "b/ and c1 still exist")
	p2 := f.list(t, "list-type=2&delimiter=/&max-keys=1&continuation-token="+url.QueryEscape(p1.NextContinuationToken))
	assert.Equal(t, []string{"b/"}, keysOf(p2))
	require.True(t, p2.IsTruncated)
	p3 := f.list(t, "list-type=2&delimiter=/&max-keys=1&continuation-token="+url.QueryEscape(p2.NextContinuationToken))
	assert.Equal(t, []string{"c1"}, keysOf(p3))
	assert.False(t, p3.IsTruncated)

	all := f.list(t, "list-type=2&delimiter=/")
	assert.Equal(t, []string{"c1", "a/", "b/"}, keysOf(all), "one page: keys then prefixes, nothing lost")
	assert.False(t, all.IsTruncated)
}

func TestListObjects_ByteOrderAndPrefixEscaping(t *testing.T) {
	f := setupVersioningFixture(t)
	f.seedKeys(t, "B", "a", "_x", "-x", "a-b", "a/b", "a_b", "ab", "a%c")
	res := f.list(t, "list-type=2")
	assert.Equal(t, []string{"-x", "B", "_x", "a", "a%c", "a-b", "a/b", "a_b", "ab"}, keysOf(res), "UTF-8 binary order regardless of the database collation")

	res = f.list(t, "list-type=2&prefix=a_")
	assert.Equal(t, []string{"a_b"}, keysOf(res), "_ in a prefix is literal")
	res = f.list(t, "list-type=2&prefix=a%25")
	assert.Equal(t, []string{"a%c"}, keysOf(res), "%% in a prefix is literal")
	res = f.list(t, "list-type=2&start-after=a-b")
	assert.Equal(t, []string{"a/b", "a_b", "ab"}, keysOf(res))
}

func TestListObjects_V1MarkerAndMissingBucket(t *testing.T) {
	f := setupVersioningFixture(t)
	f.seedKeys(t, "k1", "k2", "k3")
	res := f.list(t, "max-keys=2")
	assert.True(t, res.IsTruncated)
	assert.Equal(t, "k2", res.NextMarker, "v1 clients page on NextMarker")
	res = f.list(t, "max-keys=2&marker=k2")
	assert.Equal(t, []string{"k3"}, keysOf(res))
	assert.Equal(t, "k2", res.Marker)

	req := httptest.NewRequest("GET", "/no-such-bucket-"+f.tenantID+"?list-type=2", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), ErrNoSuchBucket)
}

func TestListObjectVersions_PrefixIsLiteralAndClassFromBackend(t *testing.T) {
	f := setupVersioningFixture(t)
	f.seedKeys(t, "a_b", "axb")
	req := httptest.NewRequest("GET", "/"+f.bucket+"?versions&prefix=a_", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res ListVersionsResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
	require.Len(t, res.Versions, 1)
	assert.Equal(t, "a_b", res.Versions[0].Key)
	assert.Equal(t, "STANDARD", res.Versions[0].StorageClass, "same backend→class mapping as List and HEAD (local is STANDARD since WP-R7-5)")
}

func TestPrefixSuccessor(t *testing.T) {
	assert.Equal(t, "a0", prefixSuccessor("a/"))
	assert.Equal(t, "b", prefixSuccessor("a"))
	assert.Equal(t, "dir/sub0", prefixSuccessor("dir/sub/"))
	assert.Equal(t, "", prefixSuccessor(""))
	assert.Equal(t, "é", prefixSuccessor("è"), "advances the last rune, stays valid UTF-8")
	assert.True(t, prefixSuccessor("x\U0010FFFF") > "x\U0010FFFF")
	assert.True(t, prefixSuccessor("퟿") > "퟿￿", "skips the surrogate gap")
}

// ---- R4-06 (P1): tags belong to the object that was written -------------------

func (f *versioningFixture) tagsOf(t *testing.T, key string) string {
	t.Helper()
	var tags string
	require.NoError(t, f.db.QueryRow(`SELECT tags::text FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`, f.tenantID, f.bucket, key).Scan(&tags))
	return tags
}

func TestObjectTags_HeaderOnPutAndResetOnOverwrite(t *testing.T) {
	f := setupVersioningFixture(t)
	key := "tagged.bin"
	req := httptest.NewRequest("PUT", "/"+f.bucket+"/"+key, bytes.NewReader([]byte("v1")))
	req.Header.Set("x-amz-tagging", "env=prod&team=core%20infra")
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, f.bucket, key)
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"env":"prod","team":"core infra"}`, f.tagsOf(t, key))

	f.putObject(t, key, "v2")
	assert.JSONEq(t, `{}`, f.tagsOf(t, key), "PutObject replaces the tag set; no header means no tags")

	req = httptest.NewRequest("PUT", "/"+f.bucket+"/"+key, bytes.NewReader([]byte("v3")))
	req.Header.Set("x-amz-tagging", strings.Repeat("k=v&", 11)+"z=1")
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w = httptest.NewRecorder()
	f.adapter.HandlePut(w, req, f.bucket, key)
	assert.Equal(t, http.StatusBadRequest, w.Code, "more than 10 tags is InvalidTag, refused before the body is read")
	assert.Contains(t, w.Body.String(), ErrInvalidTag)
	assert.Equal(t, "v2", onDisk(t, f, key))
}

func TestObjectTags_CopyDirectiveAndMultipartReset(t *testing.T) {
	f := setupVersioningFixture(t)
	req := httptest.NewRequest("PUT", "/"+f.bucket+"/src", bytes.NewReader([]byte("SRC")))
	req.Header.Set("x-amz-tagging", "a=b")
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, f.bucket, "src")
	require.Equal(t, http.StatusOK, w.Code)

	require.Equal(t, http.StatusOK, f.copyObject(t, "src", "copied", nil).Code)
	assert.JSONEq(t, `{"a":"b"}`, f.tagsOf(t, "copied"), "COPY (default) carries the source tags")
	require.Equal(t, http.StatusOK, f.copyObject(t, "src", "replaced", map[string]string{"x-amz-tagging-directive": "REPLACE", "x-amz-tagging": "x=y"}).Code)
	assert.JSONEq(t, `{"x":"y"}`, f.tagsOf(t, "replaced"))
	require.Equal(t, http.StatusOK, f.copyObject(t, "src", "replaced-empty", map[string]string{"x-amz-tagging-directive": "REPLACE"}).Code)
	assert.JSONEq(t, `{}`, f.tagsOf(t, "replaced-empty"))

	// A multipart overwrite of a tagged key starts with an empty tag set.
	require.Equal(t, http.StatusOK, f.mpUpload(t, "src", []byte("MULTIPART"), nil).Code)
	assert.JSONEq(t, `{}`, f.tagsOf(t, "src"))
}

// ---- R4-07 (P2): conditionals and ranges per RFC 9110 -------------------------

func (f *versioningFixture) head(t *testing.T, key string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("HEAD", "/"+f.bucket+"/"+key, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, req)
	return w
}

func (f *versioningFixture) get(t *testing.T, key string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/"+f.bucket+"/"+key, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, f.bucket, key)
	return w
}

func TestConditional_IfMatchAndHeadConditionals(t *testing.T) {
	f := setupVersioningFixture(t)
	w := f.putObject(t, "c.bin", "CONDITIONAL")
	etag := w.Header().Get("ETag")
	require.NotEmpty(t, etag)

	assert.Equal(t, http.StatusPreconditionFailed, f.get(t, "c.bin", map[string]string{"If-Match": `"0000"`}).Code, "GET with a wrong If-Match")
	assert.Equal(t, http.StatusOK, f.get(t, "c.bin", map[string]string{"If-Match": etag}).Code)
	assert.Equal(t, http.StatusPreconditionFailed, f.get(t, "c.bin", map[string]string{"If-Match": `"0000"`, "If-None-Match": `"0000"`}).Code,
		"If-Match is evaluated first (RFC 9110 §13.2.2)")

	assert.Equal(t, http.StatusNotModified, f.head(t, "c.bin", map[string]string{"If-None-Match": etag}).Code, "HEAD honours If-None-Match")
	assert.Equal(t, http.StatusPreconditionFailed, f.head(t, "c.bin", map[string]string{"If-Match": `"0000"`}).Code, "HEAD honours If-Match")
	assert.Equal(t, http.StatusOK, f.head(t, "c.bin", map[string]string{"If-Match": etag}).Code)
	h := f.head(t, "c.bin", map[string]string{"If-Modified-Since": time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)})
	assert.Equal(t, http.StatusNotModified, h.Code, "HEAD honours If-Modified-Since")
	assert.Equal(t, etag, h.Header().Get("ETag"), "304 carries the validators")
}

func TestRange_ZeroByteObjectMultiRangeAndCase(t *testing.T) {
	f := setupVersioningFixture(t)
	f.putObject(t, "empty.bin", "")
	f.putObject(t, "ten.bin", "0123456789")

	w := f.get(t, "empty.bin", map[string]string{"Range": "bytes=0-0"})
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, w.Code, "any Range on a 0-byte object is unsatisfiable")
	assert.Equal(t, "bytes */0", w.Header().Get("Content-Range"))

	w = f.get(t, "ten.bin", map[string]string{"Range": "bytes=0-1,5-6"})
	assert.Equal(t, http.StatusOK, w.Code, "multi-range is ignored, the full body is served")
	assert.Equal(t, "0123456789", w.Body.String())

	w = f.get(t, "ten.bin", map[string]string{"Range": "BYTES=2-3"})
	assert.Equal(t, http.StatusPartialContent, w.Code, "the range unit is case-insensitive")
	assert.Equal(t, "23", w.Body.String())

	w = f.get(t, "ten.bin", map[string]string{"Range": "bytes=50-"})
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, w.Code)
}

// ---- R4-08 (P2): presigned URLs — clock-bounded lifetime, RFC 3986 canonical query

func TestPresign_FutureDateIsRefused(t *testing.T) {
	s, mock, cleanup := newMockDB(t)
	defer cleanup()
	expectTenantLookup(mock)

	future := time.Now().UTC().Add(48 * time.Hour)
	q := signPresignedURL("GET", "/bucket/key", "localhost:8000", testAccessKey, testSecretKey, 604800, future)
	r := httptest.NewRequest("GET", "/bucket/key?"+q.Encode(), nil)
	r.Host = "localhost:8000"
	_, _, err := s.verifyPresignedURL(r)
	require.Error(t, err, "a signer must not mint URLs that outlive the 7-day cap by dating them in the future")
	assert.Contains(t, err.Error(), ErrRequestTimeTooSkewed)

	// Well inside the skew window it is still fine.
	s2, mock2, cleanup2 := newMockDB(t)
	defer cleanup2()
	expectTenantLookup(mock2)
	q = signPresignedURL("GET", "/bucket/key", "localhost:8000", testAccessKey, testSecretKey, 300, time.Now().UTC().Add(5*time.Minute))
	r = httptest.NewRequest("GET", "/bucket/key?"+q.Encode(), nil)
	r.Host = "localhost:8000"
	_, _, err = s2.verifyPresignedURL(r)
	require.NoError(t, err)
}

func TestPresign_SDKPresignedURLWithSpaceInQueryVerifies(t *testing.T) {
	s, mock, cleanup := newMockDB(t)
	defer cleanup()
	expectTenantLookup(mock)

	client := s3.New(s3.Options{
		Region:       testRegion,
		Credentials:  credentials.NewStaticCredentialsProvider(testAccessKey, testSecretKey, ""),
		BaseEndpoint: aws.String("http://localhost:8000"),
		UsePathStyle: true,
	})
	presigned, err := s3.NewPresignClient(client).PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket:                     aws.String("bucket"),
		Key:                        aws.String("key"),
		ResponseContentDisposition: aws.String(`attachment; filename="my file.txt"`),
	}, s3.WithPresignExpires(5*time.Minute))
	require.NoError(t, err)
	u, err := url.Parse(presigned.URL)
	require.NoError(t, err)
	require.Contains(t, u.RawQuery, "%20", "the SDK encodes the space as %%20")

	r := httptest.NewRequest("GET", u.RequestURI(), nil)
	r.Host = u.Host
	tenantID, _, err := s.verifyPresignedURL(r)
	require.NoError(t, err, "URL: %s", presigned.URL)
	assert.Equal(t, testPresignTenantID, tenantID)
}

func TestPresign_OwnGeneratorRoundTripsAwkwardKeys(t *testing.T) {
	for _, key := range []string{"plain.txt", "with space.txt", "a+b.txt", "ünï/日本語.txt", "q?&=x.txt"} {
		s, mock, cleanup := newMockDB(t)
		expectTenantLookup(mock)
		full, _ := generatePresignedS3URL("http://localhost:8000", testAccessKey, testSecretKey, "bucket", key, "GET", 300)
		u, err := url.Parse(full)
		require.NoError(t, err, key)
		r := httptest.NewRequest("GET", u.RequestURI(), nil)
		r.Host = u.Host
		_, _, err = s.verifyPresignedURL(r)
		assert.NoError(t, err, "key %q via %s", key, full)
		cleanup()
	}
}
