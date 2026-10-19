package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R2-7 (R2-25, R3-27): the key length and the bucket name are checked at
// dispatch, for every object operation, before any handler. A key over
// S3's 1,024 bytes walked into the handlers (R2-25: a 1,100-byte GET 500'd
// ×3 retries); a malformed bucket name died wherever the DB lookup took it
// (R3-27). Everything here goes through handleS3Request with real SigV4.

// s3Status is the HTTP status and S3 error code of an SDK call's failure
// (a HEAD has no body: its code is the SDK's name for the status).
func s3Status(t *testing.T, err error) (int, string) {
	t.Helper()
	require.Error(t, err)
	var re *smithyhttp.ResponseError
	require.True(t, errors.As(err, &re), "%v", err)
	code := ""
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code = ae.ErrorCode()
	}
	return re.HTTPStatusCode(), code
}

func TestKeyLength_AnObjectOpWithAKeyOver1024BytesIs400(t *testing.T) {
	// Arrange
	f := setupBypassFixture(t)
	c, ctx := f.root(), context.Background()
	key := strings.Repeat("k", 1100)
	b := aws.String(f.bucket)

	ops := map[string]func() error{
		"GetObject": func() error {
			_, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: b, Key: aws.String(key)})
			return err
		},
		"HeadObject": func() error {
			_, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: b, Key: aws.String(key)})
			return err
		},
		"DeleteObject": func() error {
			_, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: b, Key: aws.String(key)})
			return err
		},
		"GetObjectTagging": func() error {
			_, err := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: b, Key: aws.String(key)})
			return err
		},
		"GetObjectRetention": func() error {
			_, err := c.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{Bucket: b, Key: aws.String(key)})
			return err
		},
		"PutObject": func() error {
			_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: b, Key: aws.String(key), Body: strings.NewReader("x")})
			return err
		},
	}

	for name, op := range ops {
		// Act
		status, code := s3Status(t, op())

		// Assert
		assert.Equal(t, http.StatusBadRequest, status, name)
		if name != "HeadObject" {
			assert.Equal(t, "KeyTooLongError", code, name)
		}
	}
}

func TestKeyLength_A1024ByteKeyIsNotRefusedByTheGate(t *testing.T) {
	f := setupBypassFixture(t)
	key := strings.Repeat("k", 1024)

	_, err := f.root().GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: aws.String(key)})

	status, code := s3Status(t, err)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "NoSuchKey", code)
}

func TestKeyLength_WritesKeepTheStricter857Refusal(t *testing.T) {
	// A 900-byte key is under the gate's 1,024 and over the write paths'
	// 857 (blob_address.go: the address suffix needs the room).
	f := setupBypassFixture(t)
	key := strings.Repeat("w", 900)

	_, err := f.root().PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(f.bucket), Key: aws.String(key), Body: strings.NewReader("x")})

	status, code := s3Status(t, err)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "KeyTooLongError", code)
	assert.Contains(t, err.Error(), "limited to 857 bytes", "the write path's own message, not the gate's")
}

func TestBucketName_AnObjectOpOnAMalformedNameIs400(t *testing.T) {
	f := setupBypassFixture(t)
	c, ctx := f.root(), context.Background()
	names := map[string]string{
		"uppercase": "Locked",
		"2 chars":   "ab",
		"64 chars":  strings.Repeat("a", 64),
		"dot dot":   "a..b",
	}

	for label, name := range names {
		b := aws.String(name)
		_, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: b, Key: aws.String("k")})
		status, code := s3Status(t, err)
		assert.Equal(t, http.StatusBadRequest, status, "GET %s", label)
		assert.Equal(t, "InvalidBucketName", code, "GET %s", label)

		_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: b, Key: aws.String("k"), Body: strings.NewReader("x")})
		status, code = s3Status(t, err)
		assert.Equal(t, http.StatusBadRequest, status, "PUT %s", label)
		assert.Equal(t, "InvalidBucketName", code, "PUT %s", label)

		_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: b, Key: aws.String("k")})
		status, code = s3Status(t, err)
		assert.Equal(t, http.StatusBadRequest, status, "DELETE %s", label)
		assert.Equal(t, "InvalidBucketName", code, "DELETE %s", label)

		_, err = c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: b})
		status, code = s3Status(t, err)
		assert.Equal(t, http.StatusBadRequest, status, "LIST %s", label)
		assert.Equal(t, "InvalidBucketName", code, "LIST %s", label)
	}
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM object_head_cache WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	assert.Zero(t, n, "nothing stored under a malformed name")
}

func TestBucketName_ASystemBucketNameIsStillNoSuchBucket(t *testing.T) {
	// `_` names are not valid bucket names, but answering InvalidBucketName
	// would say they are special: anything but a read of an object stays
	// NoSuchBucket (systemBucketRefused).
	f := setupBypassFixture(t)
	c, ctx := f.root(), context.Background()

	_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(tenant.ExportsBucket), Key: aws.String("k"), Body: strings.NewReader("x")})
	status, code := s3Status(t, err)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "NoSuchBucket", code)

	_, err = c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(tenant.ExportsBucket)})
	status, code = s3Status(t, err)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "NoSuchBucket", code)
}

func TestBucketName_SystemBucketReadsStillWork(t *testing.T) {
	// The account-export download: a presigned GET (and HEAD) of an
	// `_exports` key minted by DownloadURL passes both gates.
	f := setupExportFixture(t)
	f.seed()
	id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
	f.run()
	st := f.status(id)
	require.Equal(t, "completed", st.Status, st.Error)
	link, _, err := f.svc.DownloadURL(context.Background(), id, f.userID)
	require.NoError(t, err)
	u, err := url.Parse(link)
	require.NoError(t, err)

	live := &Server{logger: zap.NewNop(), router: chi.NewRouter(), engine: f.eng, db: f.db, testMode: false}
	req := httptest.NewRequest("GET", u.RequestURI(), nil)
	req.Host = u.Host
	rr := httptest.NewRecorder()
	live.handleS3Request(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, st.SizeBytes, int64(rr.Body.Len()))
}

func TestBucketName_AValidBucketIsUntouched(t *testing.T) {
	f := setupBypassFixture(t)
	c, ctx := f.root(), context.Background()
	b, k := aws.String(f.bucket), aws.String("dir/round-trip.txt")

	_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: b, Key: k, Body: strings.NewReader("hello")})
	require.NoError(t, err)
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: b, Key: k})
	require.NoError(t, err)
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: b, Key: k})
	require.NoError(t, err)
	got, err := io.ReadAll(out.Body)
	_ = out.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
	list, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: b})
	require.NoError(t, err)
	assert.Len(t, list.Contents, 1)
	_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: b, Key: k})
	require.NoError(t, err)
	assert.True(t, f.gone(f.bucket, "dir/round-trip.txt"))
}

func TestDeleteObjects_AKeyOver1024BytesIsAPerKeyError(t *testing.T) {
	// Batch keys arrive in the body, not the path: the dispatch gate cannot
	// see them, so the batch refuses the key itself — a per-key error, the
	// other keys deleted.
	f := setupBypassFixture(t)
	c, ctx := f.root(), context.Background()
	b := aws.String(f.bucket)
	for _, k := range []string{"one.txt", "two.txt"} {
		_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: b, Key: aws.String(k), Body: strings.NewReader(k)})
		require.NoError(t, err)
	}
	long := strings.Repeat("d", 1100)

	out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: b, Delete: &s3types.Delete{Objects: []s3types.ObjectIdentifier{
		{Key: aws.String("one.txt")}, {Key: aws.String(long)}, {Key: aws.String("two.txt")}}}})

	require.NoError(t, err)
	require.Len(t, out.Errors, 1)
	assert.Equal(t, long, aws.ToString(out.Errors[0].Key))
	assert.Equal(t, "KeyTooLongError", aws.ToString(out.Errors[0].Code))
	var deleted []string
	for _, d := range out.Deleted {
		deleted = append(deleted, aws.ToString(d.Key))
	}
	assert.ElementsMatch(t, []string{"one.txt", "two.txt"}, deleted)
	assert.True(t, f.gone(f.bucket, "one.txt"))
	assert.True(t, f.gone(f.bucket, "two.txt"))
}
