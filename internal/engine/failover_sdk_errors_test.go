package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// s3ErrorFrom drives a REAL aws-sdk-go-v2 S3 client against an httptest server
// that answers with the given status and body, and returns the error exactly
// as the S3-class drivers (idrive, lyve, r2, geyser, s3compat) receive it. The
// error chain is OperationError → s3shared.ResponseError → APIError, with the
// SDK's own formatting ("StatusCode: 404", "api error NotFound: Not Found"),
// which no hand-built fixture reproduces faithfully — the previous test
// encoded the aws-sdk-go v1 casing and passed for years (R6-01).
func s3ErrorFrom(t *testing.T, op string, status int, body string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-amz-request-id", "R6TESTREQUESTID")
		w.Header().Set("x-amz-id-2", "R6TESTHOSTID")
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return s3CallAgainst(t, srv.URL, op, context.Background())
}

func s3CallAgainst(t *testing.T, endpoint, op string, ctx context.Context) error {
	t.Helper()
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKIAR6TEST", "secret", ""),
		Retryer:      aws.NopRetryer{},
	})
	in := struct{ bucket, key *string }{aws.String("stored-test"), aws.String("t-tenant/c/k")}
	var err error
	switch op {
	case "GET":
		_, err = client.GetObject(ctx, &s3.GetObjectInput{Bucket: in.bucket, Key: in.key})
	case "HEAD":
		_, err = client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: in.bucket, Key: in.key})
	case "DELETE":
		_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: in.bucket, Key: in.key})
	default:
		t.Fatalf("unknown op %q", op)
	}
	require.Error(t, err)
	// Mirror the driver + engine wrapping (idrive.go:315, engine.go:265).
	return fmt.Errorf("get %s/%s: %w", "c", "k", fmt.Errorf("idrive get stream c/k: %w", err))
}

const (
	xmlNoSuchKey    = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message><Key>t-tenant/c/k</Key><RequestId>R6</RequestId></Error>`
	xmlNoSuchBucket = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist</Message></Error>`
	xmlInternal     = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>InternalError</Code><Message>We encountered an internal error.</Message></Error>`
	xmlSlowDown     = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>SlowDown</Code><Message>Please reduce your request rate.</Message></Error>`
	xmlAccessDenied = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`
)

// TestIsBackendFailure_RealSDKErrorShapes is the R6-01 regression: every 404
// shape aws-sdk-go-v2 can hand a driver is a MISS, never a breaker failure;
// 5xx / 403 / NoSuchBucket / connection errors ARE failures.
func TestIsBackendFailure_RealSDKErrorShapes(t *testing.T) {
	cases := []struct {
		name        string
		op          string
		status      int
		body        string
		wantFailure bool
	}{
		{"GET 404 with NoSuchKey body (AWS, iDrive, R2)", "GET", 404, xmlNoSuchKey, false},
		{"GET 404 with empty body (vendor without error XML → api error NotFound)", "GET", 404, "", false},
		{"HEAD 404 (types.NotFound — Exists / RestoreStatus)", "HEAD", 404, "", false},
		{"DELETE 404 with NoSuchKey body (vendor that is not idempotent)", "DELETE", 404, xmlNoSuchKey, false},
		{"DELETE 404 with empty body", "DELETE", 404, "", false},
		{"GET 404 NoSuchBucket — misconfigured backend IS a failure", "GET", 404, xmlNoSuchBucket, true},
		{"GET 500 InternalError", "GET", 500, xmlInternal, true},
		{"GET 503 SlowDown", "GET", 503, xmlSlowDown, true},
		{"GET 403 AccessDenied — dead key IS a failure", "GET", 403, xmlAccessDenied, true},
		{"HEAD 500 empty body", "HEAD", 500, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s3ErrorFrom(t, tc.op, tc.status, tc.body)
			t.Logf("error text: %v", err)
			assert.Equal(t, tc.wantFailure, isBackendFailure(err))
		})
	}
}

// A refused connection (backend down) must still trip the breaker.
func TestIsBackendFailure_ConnectionRefusedIsFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())

	err = s3CallAgainst(t, "http://"+addr, "GET", context.Background())
	assert.True(t, isBackendFailure(err), "connection refused must count: %v", err)
}

// R6-06: the CLIENT going away cancels the request context; the SDK returns a
// wrapped context.Canceled. That is not the backend's fault and must never
// charge its breaker (five aborted uploads a minute used to open the primary).
func TestIsBackendFailure_ClientCancellationIsNotFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s3CallAgainst(t, srv.URL, "GET", ctx)
	require.True(t, errors.Is(err, context.Canceled), "fixture must produce a wrapped context.Canceled: %v", err)

	assert.False(t, isBackendFailure(err), "client cancellation must not trip the breaker: %v", err)
	// A genuine deadline is still the stall signal.
	assert.True(t, isBackendFailure(fmt.Errorf("idrive put: %w", context.DeadlineExceeded)))
}

// A cancelled request must not be retried against every other backend either.
func TestFailoverManager_StopsWalkingWhenContextCancelled(t *testing.T) {
	fm := NewFailoverManager(nopLogger())
	for _, b := range []string{"a", "b", "c"} {
		fm.Register(b)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	_, err := fm.Execute(ctx, []string{"a", "b", "c"}, func(string) error {
		calls++
		return fmt.Errorf("sdk: %w", ctx.Err())
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	assert.LessOrEqual(t, calls, 1, "a cancelled request must not walk the remaining candidates")
	for _, b := range []string{"a", "b", "c"} {
		assert.Equal(t, "closed", fm.GetStatus(b), "breaker %s charged for a client cancellation", b)
	}
}
