package api

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 — the SSE-C key MD5 header
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-SSEC-1 B.5 (Stage 1a Prompt 17 Part B, migration 096): a bucket that
// requires a customer key. An unkeyed PUT, CreateMultipartUpload or copy
// destination into it is 400 InvalidRequest with AWS's bucket-policy
// wording; a generated object (access log, inventory, export) is refused and
// counted, never plaintext; a flagged bucket cannot be a log or inventory
// target (409); a public bucket cannot require a key (409). Set through the
// management API and the dashboard. Objects written before the flag stay as
// they were.

func (f *versioningFixture) requireKeys(t *testing.T, bucket string, on bool) {
	t.Helper()
	_, err := f.db.Exec(`UPDATE buckets SET ssec_required = $3 WHERE tenant_id = $1 AND name = $2`, f.tenantID, bucket, on)
	require.NoError(t, err)
}

// testSSECKey is one customer key for these tests; its base64 and MD5 are
// what the S3 headers carry.
var (
	testSSECKey    = bytes.Repeat([]byte{0x42}, 32)
	testSSECKeyB64 = base64.StdEncoding.EncodeToString(testSSECKey)
	testSSECKeyMD5 = func() string { s := md5.Sum(testSSECKey); return base64.StdEncoding.EncodeToString(s[:]) }() // #nosec G401
)

func ssecRequiredKeyHeaders() map[string]string {
	return map[string]string{
		"x-amz-server-side-encryption-customer-algorithm": "AES256",
		"x-amz-server-side-encryption-customer-key":       testSSECKeyB64,
		"x-amz-server-side-encryption-customer-key-MD5":   testSSECKeyMD5,
	}
}

func assertSSECRequired(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var e S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &e))
	assert.Equal(t, "InvalidRequest", e.Code)
	assert.True(t, strings.HasPrefix(e.Message, ssecRequiredRefusal), "AWS's sentence first: %q", e.Message)
}

func TestSSECRequired_AnUnkeyedPutIs400AKeyedOneWrites(t *testing.T) {
	f := setupVersioningFixture(t)
	f.requireKeys(t, f.bucket, true)

	assertSSECRequired(t, f.s3(t, "PUT", "/"+f.bucket+"/plain.txt", strings.NewReader("plaintext"), nil))
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`, f.tenantID, f.bucket).Scan(&n))
	assert.Zero(t, n, "nothing written")

	w := f.s3(t, "PUT", "/"+f.bucket+"/keyed.txt", strings.NewReader("sealed"), ssecRequiredKeyHeaders())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestSSECRequired_AnUnkeyedCreateMultipartUploadIs400(t *testing.T) {
	f := setupVersioningFixture(t)
	f.requireKeys(t, f.bucket, true)

	assertSSECRequired(t, f.s3(t, "POST", "/"+f.bucket+"/big.bin?uploads", nil, nil))
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM multipart_uploads WHERE tenant_id = $1 AND bucket = $2`, f.tenantID, f.bucket).Scan(&n))
	assert.Zero(t, n, "no upload row")
}

func TestSSECRequired_AnUnkeyedCopyDestinationIs400(t *testing.T) {
	f := setupVersioningFixture(t)
	require.Equal(t, http.StatusOK, f.s3(t, "PUT", "/"+f.bucket+"/src.txt", strings.NewReader("source bytes"), nil).Code)
	f.requireKeys(t, f.bucket, true)

	assertSSECRequired(t, f.copyObject(t, "src.txt", "dst.txt", nil))
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'dst.txt'`, f.tenantID, f.bucket).Scan(&n))
	assert.Zero(t, n)
	// The object written before the flag is still read as it was.
	assert.Equal(t, http.StatusOK, f.s3(t, "GET", "/"+f.bucket+"/src.txt", nil, nil).Code)
}

func TestSSECRequired_AGeneratedObjectIsRefusedAndCounted(t *testing.T) {
	f := setupLoggingFixture(t)
	w := newGeneratedObjectWriter(f.db, f.eng, nil, nil, zap.NewNop())
	f.server.accessLogTracker.SetWriter(w)
	_, err := f.db.Exec(`UPDATE buckets SET logging_enabled = TRUE, logging_target_bucket = $3, logging_prefix = 'logs/' WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, f.bucket, f.logBucket)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE buckets SET ssec_required = TRUE WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.logBucket)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO s3_access_log (tenant_id, bucket, object_key, operation, status_code, bytes_sent, bytes_received, source_ip, user_agent, request_id, error_code, logged_at)
		VALUES ($1,$2,'k','GetObject',200,1,0,'127.0.0.1','ua','rid','',NOW())`, f.tenantID, f.bucket)
	require.NoError(t, err)
	before := refusedCount(generatedKindAccessLog, refusedSSECRequired)

	_, dErr := f.server.accessLogTracker.deliverLogs(context.Background())
	require.NoError(t, dErr)

	var pending, heads int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM s3_access_log WHERE tenant_id=$1`, f.tenantID).Scan(&pending))
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2`, f.tenantID, f.logBucket).Scan(&heads))
	assert.Equal(t, 1, pending, "rows kept")
	assert.Zero(t, heads)
	entries, _ := os.ReadDir(filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.logBucket)))
	assert.Empty(t, entries, "nothing on the backend")
	assert.Equal(t, before+1, refusedCount(generatedKindAccessLog, refusedSSECRequired))
}

func TestSSECRequired_AFlaggedBucketCannotBeALogOrInventoryTarget(t *testing.T) {
	lf := setupLoggingFixture(t)
	_, err := lf.db.Exec(`UPDATE buckets SET ssec_required = TRUE WHERE tenant_id = $1 AND name = $2`, lf.tenantID, lf.logBucket)
	require.NoError(t, err)
	logXML := fmt.Sprintf(`<BucketLoggingStatus xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><LoggingEnabled><TargetBucket>%s</TargetBucket><TargetPrefix>logs/</TargetPrefix></LoggingEnabled></BucketLoggingStatus>`, lf.logBucket)
	r := httptest.NewRequest("PUT", "/"+lf.bucket+"?logging", bytes.NewReader([]byte(logXML))).WithContext(s3Ctx(context.Background(), lf.tenant))
	w := httptest.NewRecorder()
	lf.server.handlePutBucketLogging(w, r, &S3Request{Bucket: lf.bucket, TenantID: lf.tenantID})
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrInvalidBucketState)
	var enabled bool
	require.NoError(t, lf.db.QueryRow(`SELECT logging_enabled FROM buckets WHERE tenant_id = $1 AND name = $2`, lf.tenantID, lf.bucket).Scan(&enabled))
	assert.False(t, enabled)

	inf := setupInventoryFixture(t)
	_, err = inf.db.Exec(`UPDATE buckets SET ssec_required = TRUE WHERE tenant_id = $1 AND name = $2`, inf.tenantID, inf.invBucket)
	require.NoError(t, err)
	invXML := fmt.Sprintf(`<InventoryConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Id>daily</Id><IsEnabled>true</IsEnabled><Destination><S3BucketDestination><Bucket>%s</Bucket><Prefix>reports/</Prefix><Format>CSV</Format></S3BucketDestination></Destination><Schedule><Frequency>Daily</Frequency></Schedule></InventoryConfiguration>`, inf.invBucket)
	r = httptest.NewRequest("PUT", "/"+inf.bucket+"?inventory&id=daily", bytes.NewReader([]byte(invXML))).WithContext(s3Ctx(context.Background(), inf.tenant))
	w = httptest.NewRecorder()
	inf.server.handlePutBucketInventory(w, r, &S3Request{Bucket: inf.bucket, TenantID: inf.tenantID})
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	require.NoError(t, inf.db.QueryRow(`SELECT inventory_enabled FROM buckets WHERE tenant_id = $1 AND name = $2`, inf.tenantID, inf.bucket).Scan(&enabled))
	assert.False(t, enabled)
}

// The management API: PATCH sets the flag, GET and PATCH report it, a public
// bucket cannot require a key.
func (f *versioningFixture) mgmt(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := f.server
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Request-Id", "req-1")
			ctx := context.WithValue(req.Context(), tenantIDKey, f.tenantID)
			ctx = context.WithValue(ctx, userIDKey, "user-1")
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Get("/api/v1/manage/buckets/{name}", s.handleMgmtGetBucket)
	r.Patch("/api/v1/manage/buckets/{name}", s.handleMgmtPatchBucket)
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	var req *http.Request
	if rd != nil {
		req = httptest.NewRequest(method, path, rd)
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestSSECRequired_ManagementAPISetsAndReportsTheFlag(t *testing.T) {
	f := setupVersioningFixture(t)
	path := "/api/v1/manage/buckets/" + f.bucket

	w := f.mgmt(t, "GET", path, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, false, got["ssec_required"], "reported, off by default")

	w = f.mgmt(t, "PATCH", path, `{"ssec_required": true}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, true, got["ssec_required"])
	var on bool
	require.NoError(t, f.db.QueryRow(`SELECT ssec_required FROM buckets WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket).Scan(&on))
	assert.True(t, on)

	// Metadata alone still works (the body no longer needs both).
	w = f.mgmt(t, "PATCH", path, `{"metadata": {"env": "prod"}}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, true, got["ssec_required"], "untouched by a metadata patch")

	w = f.mgmt(t, "PATCH", path, `{"ssec_required": false}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, f.db.QueryRow(`SELECT ssec_required FROM buckets WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket).Scan(&on))
	assert.False(t, on)
}

func TestSSECRequired_APublicBucketCannotRequireAKey(t *testing.T) {
	f := setupVersioningFixture(t)
	_, err := f.db.Exec(`UPDATE buckets SET visibility = 'public-read' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	require.NoError(t, err)

	w := f.mgmt(t, "PATCH", "/api/v1/manage/buckets/"+f.bucket, `{"ssec_required": true}`)

	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "bucket_public")
	var on bool
	require.NoError(t, f.db.QueryRow(`SELECT ssec_required FROM buckets WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket).Scan(&on))
	assert.False(t, on)
}
