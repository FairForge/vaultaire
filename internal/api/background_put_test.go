package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Review R13-02 (R6-21 / WP-R6-7): access-log delivery and inventory reports
// go through the customer write path. The proof is the one R6 asked for:
// enable logging → make requests → deliver → GET the report through the S3
// handler. Before the fix the object landed under "tenant/<id>/<bucket>"
// with no head row: GET was 404 and the bytes were orphaned.

func (f *loggingFixture) s3(t *testing.T, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, rd)
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	rr := httptest.NewRecorder()
	f.server.handleS3Request(rr, req)
	return rr
}

func TestAccessLogDelivery_ReportReadableThroughS3(t *testing.T) {
	f := setupLoggingFixture(t)
	f.server.accessLogTracker.SetWriter(newGeneratedObjectWriter(f.db, f.eng, nil, nil, zap.NewNop()))

	// Enable logging on the source bucket through the S3 handler.
	configXML := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<BucketLoggingStatus xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><LoggingEnabled><TargetBucket>%s</TargetBucket><TargetPrefix>logs/</TargetPrefix></LoggingEnabled></BucketLoggingStatus>`, f.logBucket)
	s3Req := &S3Request{Bucket: f.bucket, TenantID: f.tenantID}
	r := httptest.NewRequest("PUT", "/"+f.bucket+"?logging", strings.NewReader(configXML)).
		WithContext(s3Ctx(context.Background(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handlePutBucketLogging(w, r, s3Req)
	require.Equal(t, 200, w.Code, w.Body.String())

	// Ten customer requests against the source bucket.
	for i := 0; i < 10; i++ {
		rr := f.s3(t, "PUT", fmt.Sprintf("/%s/obj-%d", f.bucket, i), []byte("payload"))
		require.Equal(t, 200, rr.Code, rr.Body.String())
	}
	f.server.accessLogTracker.Flush()
	var pending int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM s3_access_log WHERE tenant_id=$1 AND bucket=$2`, f.tenantID, f.bucket).Scan(&pending))
	require.Equal(t, 10, pending, "ten rows wait for delivery")

	// Deliver.
	_, dErr := f.server.accessLogTracker.deliverLogs(context.Background())
	require.NoError(t, dErr)

	// The rows are gone and exactly one log object exists in the target
	// bucket, addressed like any customer object.
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM s3_access_log WHERE tenant_id=$1 AND bucket=$2`, f.tenantID, f.bucket).Scan(&pending))
	assert.Equal(t, 0, pending, "delivered rows deleted")
	var key, backend, contentType string
	var size int64
	require.NoError(t, f.db.QueryRow(`SELECT object_key, COALESCE(backend_name,''), content_type, size_bytes FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2`,
		f.tenantID, f.logBucket).Scan(&key, &backend, &contentType, &size), "head row written for the report")
	assert.True(t, strings.HasPrefix(key, "logs/"), key)
	assert.Equal(t, "local", backend)
	assert.Equal(t, "text/plain", contentType)
	assert.Greater(t, size, int64(0))

	// GET it through the S3 handler (was 404 before the fix).
	rr := f.s3(t, "GET", "/"+f.logBucket+"/"+key, nil)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	lines := strings.Split(strings.TrimRight(rr.Body.String(), "\n"), "\n")
	assert.Len(t, lines, 10, rr.Body.String())
	assert.Contains(t, lines[0], f.tenantID+" "+f.bucket+" [")
	assert.Contains(t, rr.Body.String(), "PutObject obj-0 200")

	// HEAD works from the head row and the size matches the body.
	rr = f.s3(t, "HEAD", "/"+f.logBucket+"/"+key, nil)
	assert.Equal(t, 200, rr.Code)
	assert.Equal(t, fmt.Sprint(size), rr.Header().Get("Content-Length"))

	// And nothing was written under the old, unaddressable container.
	_, err := os.Stat(filepath.Join(f.tempDir, "tenant", f.tenantID))
	assert.True(t, os.IsNotExist(err), "no bytes under tenant/<id>/… (R6-21)")
}

func TestAccessLogDelivery_TargetBucketGoneKeepsRows(t *testing.T) {
	f := setupLoggingFixture(t)
	f.server.accessLogTracker.SetWriter(newGeneratedObjectWriter(f.db, f.eng, nil, nil, zap.NewNop()))
	_, err := f.db.Exec(`UPDATE buckets SET logging_enabled = TRUE, logging_target_bucket = $3, logging_prefix = 'logs/' WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, f.bucket, f.logBucket)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO s3_access_log (tenant_id, bucket, object_key, operation, status_code, bytes_sent, bytes_received, source_ip, user_agent, request_id, error_code, logged_at)
		VALUES ($1,$2,'k','GetObject',200,1,0,'127.0.0.1','ua','rid','',NOW())`, f.tenantID, f.bucket)
	require.NoError(t, err)
	// The target bucket was deleted after the config was written.
	_, err = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.logBucket)
	require.NoError(t, err)

	_, dErr := f.server.accessLogTracker.deliverLogs(context.Background())
	require.NoError(t, dErr)

	var pending int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM s3_access_log WHERE tenant_id=$1`, f.tenantID).Scan(&pending))
	assert.Equal(t, 1, pending, "rows kept when the target bucket is gone")
	entries, _ := os.ReadDir(filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.logBucket)))
	assert.Empty(t, entries, "nothing written into a bucket the tenant no longer owns")
}

// quotaDenier is a QuotaManager that refuses every reservation.
type quotaDenier struct{ stubQuotaManager }

func (*quotaDenier) CheckAndReserve(context.Context, string, int64) (bool, error) { return false, nil }

func TestAccessLogDelivery_QuotaExceededKeepsRows(t *testing.T) {
	f := setupLoggingFixture(t)
	f.server.accessLogTracker.SetWriter(newGeneratedObjectWriter(f.db, f.eng, &quotaDenier{}, nil, zap.NewNop()))
	_, err := f.db.Exec(`UPDATE buckets SET logging_enabled = TRUE, logging_target_bucket = $3, logging_prefix = 'logs/' WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, f.bucket, f.logBucket)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO s3_access_log (tenant_id, bucket, object_key, operation, status_code, bytes_sent, bytes_received, source_ip, user_agent, request_id, error_code, logged_at)
		VALUES ($1,$2,'k','GetObject',200,1,0,'127.0.0.1','ua','rid','',NOW())`, f.tenantID, f.bucket)
	require.NoError(t, err)

	_, dErr := f.server.accessLogTracker.deliverLogs(context.Background())
	require.NoError(t, dErr)

	var pending, heads int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM s3_access_log WHERE tenant_id=$1`, f.tenantID).Scan(&pending))
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id=$1`, f.tenantID).Scan(&heads))
	assert.Equal(t, 1, pending, "rows kept for the next pass")
	assert.Equal(t, 0, heads, "no report written over quota")
}

func TestAccessLogTracker_GateRecordsOnlyEnabledBuckets(t *testing.T) {
	at := NewS3AccessLogTracker(nil)
	at.SetLoggingEnabled("t1", "logged", true)
	at.enabledMu.Lock()
	at.enabledLoaded = true
	at.enabledMu.Unlock()

	at.Record(context.Background(), s3AccessEvent{tenantID: "t1", bucket: "logged", statusCode: 200})
	at.Record(context.Background(), s3AccessEvent{tenantID: "t1", bucket: "silent", statusCode: 200})
	at.Record(context.Background(), s3AccessEvent{tenantID: "t1", bucket: "silent", statusCode: 403}) // errors always (support page)
	at.SetLoggingEnabled("t1", "logged", false)
	at.Record(context.Background(), s3AccessEvent{tenantID: "t1", bucket: "logged", statusCode: 200})

	at.mu.Lock()
	defer at.mu.Unlock()
	require.Len(t, at.buffer, 2)
	assert.Equal(t, "logged", at.buffer[0].bucket)
	assert.Equal(t, 403, at.buffer[1].statusCode)
}

func TestInventoryDelivery_ReportReadableThroughS3(t *testing.T) {
	f := setupInventoryFixture(t)
	f.server.accessLogTracker = nil
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("PUT", fmt.Sprintf("/%s/file-%d.txt", f.bucket, i), strings.NewReader("hello"))
		req.ContentLength = 5
		req = req.WithContext(s3Ctx(req.Context(), f.tenant))
		rr := httptest.NewRecorder()
		f.server.handleS3Request(rr, req)
		require.Equal(t, 200, rr.Code, rr.Body.String())
	}

	runner := NewInventoryRunner(f.db, f.eng, zap.NewNop())
	runner.SetWriter(newGeneratedObjectWriter(f.db, f.eng, nil, nil, zap.NewNop()))
	runner.GenerateReportNow(context.Background(), f.tenantID, f.bucket, f.invBucket, "inv/", "CSV")

	key := fmt.Sprintf("inv/%s/%sT00-00Z/manifest.csv", f.bucket, time.Now().UTC().Format("2006-01-02"))
	var contentType string
	require.NoError(t, f.db.QueryRow(`SELECT content_type FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3`,
		f.tenantID, f.invBucket, key).Scan(&contentType), "head row for the manifest")
	assert.Equal(t, "text/csv", contentType)

	req := httptest.NewRequest("GET", "/"+f.invBucket+"/"+key, nil).
		WithContext(s3Ctx(context.Background(), f.tenant))
	rr := httptest.NewRecorder()
	f.server.handleS3Request(rr, req)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	lines := strings.Split(strings.TrimRight(rr.Body.String(), "\n"), "\n")
	require.Len(t, lines, 4, rr.Body.String())
	assert.Equal(t, "Key,SizeBytes,ETag,ContentType,LastModified,EncryptionAlgorithm,StorageClass", lines[0])
	assert.True(t, strings.HasPrefix(lines[1], "file-0.txt,5,"), lines[1])

	// Re-running the same day overwrites the same key (one manifest per day).
	runner.GenerateReportNow(context.Background(), f.tenantID, f.bucket, f.invBucket, "inv/", "CSV")
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id=$1 AND bucket=$2`, f.tenantID, f.invBucket).Scan(&n))
	assert.Equal(t, 1, n)
}
