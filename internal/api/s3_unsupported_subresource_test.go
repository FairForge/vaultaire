package api

// Sub-resources this server does not implement (WP-R4-7, found 2026-09-30
// while auditing the plan's "not started" phases against the router): the
// parser knew a fixed set of sub-resources and let every other one fall
// through to the plain operation of the method —
//
//	DELETE /{bucket}?lifecycle  → DeleteBucket   (an empty bucket was deleted)
//	PUT    /{bucket}?policy     → CreateBucket   (200: the client believes a policy applies)
//	GET    /{bucket}?lifecycle  → ListObjects    (a listing parsed as an empty configuration)
//
// the class A1 closed for ?acl only. AWS and every S3-compatible answer 501
// NotImplemented for a sub-resource they do not serve.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestDetermineOperation_UnsupportedSubresource(t *testing.T) {
	p := NewS3Parser(zap.NewNop())

	bucketLevel := []string{
		"lifecycle", "policy", "policyStatus", "cors", "encryption", "tagging",
		"website", "replication", "publicAccessBlock", "ownershipControls",
		"analytics", "metrics", "intelligent-tiering", "accelerate",
		"requestPayment",
	}
	for _, sub := range bucketLevel {
		for _, method := range []string{"GET", "PUT", "DELETE"} {
			r := httptest.NewRequest(method, "/bkt?"+sub, nil)
			req, err := p.ParseRequest(r)
			require.NoError(t, err, "%s ?%s", method, sub)
			assert.Equal(t, opUnsupportedSubresource, req.Operation, "%s /bkt?%s", method, sub)
		}
	}

	otherCases := []struct{ method, path string }{
		{"GET", "/bkt/obj?attributes"},
		{"GET", "/bkt/obj?torrent"},
		{"POST", "/bkt/obj?select&select-type=2"},
		{"PUT", "/bkt/obj?renameObject"},
		// not S3 operations, but each used to run the destructive plain one
		{"DELETE", "/bkt/obj?retention"},
		{"DELETE", "/bkt/obj?legal-hold"},
		{"DELETE", "/bkt/obj?acl"},
		{"PUT", "/bkt/obj?uploadId=upload-0123"},
		{"PUT", "/bkt?location"},
		{"DELETE", "/bkt?versioning"},
		{"DELETE", "/bkt?notification"},
	}
	for _, tc := range otherCases {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		req, err := p.ParseRequest(r)
		require.NoError(t, err, "%s %s", tc.method, tc.path)
		assert.Equal(t, opUnsupportedSubresource, req.Operation, "%s %s", tc.method, tc.path)
	}
}

// The parameters real clients put on ordinary requests must keep routing to
// the ordinary operation: listing parameters, the SDKs' x-id hint, presign
// parameters, response-* overrides, versionId.
func TestDetermineOperation_OrdinaryQueriesStillRoute(t *testing.T) {
	p := NewS3Parser(zap.NewNop())

	cases := []struct{ method, path, want string }{
		{"GET", "/bkt?list-type=2&prefix=a/&delimiter=/&max-keys=10&encoding-type=url&fetch-owner=true", "ListObjects"},
		{"GET", "/bkt?x-id=ListObjectsV2&list-type=2", "ListObjects"},
		{"PUT", "/bkt?x-id=CreateBucket", "CreateBucket"},
		{"DELETE", "/bkt?x-id=DeleteBucket", "DeleteBucket"},
		{"GET", "/bkt/obj?x-id=GetObject&versionId=abc&response-content-disposition=attachment", "GetObject"},
		{"GET", "/bkt/obj?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=00", "GetObject"},
		{"PUT", "/bkt/obj?x-id=PutObject", "PutObject"},
		{"DELETE", "/bkt/obj?versionId=abc", "DeleteObject"},
		{"GET", "/bkt?versioning", "GetBucketVersioning"},
		{"DELETE", "/bkt?inventory", "DeleteBucketInventory"},
		{"GET", "/bkt/obj?tagging", "GetObjectTagging"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		req, err := p.ParseRequest(r)
		require.NoError(t, err, "%s %s", tc.method, tc.path)
		assert.Equal(t, tc.want, req.Operation, "%s %s", tc.method, tc.path)
	}
}

// DELETE of a configuration this server does not have must never delete the
// bucket: before the fix an EMPTY bucket was removed by
// `aws s3api delete-bucket-lifecycle` / `delete-bucket-policy` / `delete-bucket-cors`.
func TestUnsupportedSubresource_DeleteDoesNotDeleteTheBucket(t *testing.T) {
	f := setupA1Fixture(t)

	const empty = "a1-empty-bkt"
	_, err := f.db.Exec(
		`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, $2, 'private')
		 ON CONFLICT (tenant_id, name) DO NOTHING`, f.tenantID, empty)
	require.NoError(t, err)

	for _, sub := range []string{"lifecycle", "policy", "cors", "tagging", "encryption"} {
		r := httptest.NewRequest("DELETE", "/"+empty+"?"+sub, nil).WithContext(f.ctx())
		w := httptest.NewRecorder()
		f.server.handleS3Request(w, r)

		assert.Equal(t, http.StatusNotImplemented, w.Code, "DELETE ?%s: %s", sub, w.Body.String())
		assert.Contains(t, w.Body.String(), "<Code>NotImplemented</Code>", "DELETE ?%s", sub)

		var n int
		require.NoError(t, f.db.QueryRow(
			`SELECT COUNT(*) FROM buckets WHERE tenant_id = $1 AND name = $2`,
			f.tenantID, empty).Scan(&n))
		require.Equal(t, 1, n, "DELETE /%s?%s removed the bucket", empty, sub)
	}
}

// PUT of a configuration this server does not apply must not answer 200, and
// GET must not answer a listing.
func TestUnsupportedSubresource_PutAndGetAreNotImplemented(t *testing.T) {
	f := setupA1Fixture(t)

	lifecycle := `<LifecycleConfiguration><Rule><ID>expire</ID><Status>Enabled</Status>` +
		`<Filter><Prefix></Prefix></Filter><Expiration><Days>30</Days></Expiration></Rule></LifecycleConfiguration>`
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"*"}]}`

	cases := []struct{ method, sub, body string }{
		{"PUT", "lifecycle", lifecycle},
		{"PUT", "policy", policy},
		{"PUT", "encryption", `<ServerSideEncryptionConfiguration/>`},
		{"GET", "lifecycle", ""},
		{"GET", "policy", ""},
		{"GET", "encryption", ""},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, "/"+f.bucket+"?"+tc.sub, strings.NewReader(tc.body)).WithContext(f.ctx())
		w := httptest.NewRecorder()
		f.server.handleS3Request(w, r)

		assert.Equal(t, http.StatusNotImplemented, w.Code, "%s ?%s: %s", tc.method, tc.sub, w.Body.String())
		assert.NotContains(t, w.Body.String(), "ListBucketResult", "%s ?%s answered a listing", tc.method, tc.sub)
	}
}
