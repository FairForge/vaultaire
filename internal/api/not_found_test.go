package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/stretchr/testify/assert"

	"github.com/FairForge/vaultaire/internal/engine"
)

// sdkErr builds the error chain the aws-sdk-go-v2 S3 client returns for a
// failed operation — OperationError → ResponseError (HTTP status) → the API
// error — wrapped the way the drivers and the engine wrap it. The engine
// package's failover_sdk_errors_test.go produces the same chain from a real
// client; here it is hand-built so the API classifier is tested in isolation.
func sdkErr(op string, status int, apiErr error) error {
	chain := &smithy.OperationError{
		ServiceID:     "S3",
		OperationName: op,
		Err: &awshttp.ResponseError{
			ResponseError: &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
				Err:      apiErr,
			},
			RequestID: "R6TEST",
		},
	}
	return fmt.Errorf("get t1_b/k: %w", fmt.Errorf("idrive get stream t1_b/k: %w", chain))
}

func TestIsObjectMissingErr(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		missing bool
	}{
		{"nil", nil, false},
		{
			"typed engine NotFoundError, wrapped",
			fmt.Errorf("get t1_b/k: %w", engine.ErrNotFound("t1_b", "k")),
			true,
		},
		{
			"os.ErrNotExist, wrapped",
			fmt.Errorf("get local: %w", os.ErrNotExist),
			true,
		},
		{
			"local driver path error text",
			errors.New("open /tmp/vaultaire-data/t1_b/k: no such file or directory"),
			true,
		},
		{
			"aws-sdk GetObject miss (iDrive/Lyve/S3 drivers)",
			errors.New("get t1_b/k: idrive get t1/b/k: operation error S3: GetObject, https response error StatusCode: 404, RequestID: X, HostID: Y, api error NoSuchKey: The specified key does not exist."),
			true,
		},
		{
			"aws-sdk HeadObject miss",
			errors.New("get t1_b/k: api error NotFound: Not Found"),
			true,
		},
		{
			"bare 404 status text",
			errors.New("unexpected status code: 404"),
			true,
		},
		{
			"connection refused is a backend failure",
			errors.New("get t1_b/k: dial tcp 1.2.3.4:443: connect: connection refused"),
			false,
		},
		{
			"timeout is a backend failure",
			errors.New("get t1_b/k: dial tcp: i/o timeout"),
			false,
		},
		{
			"all backends unavailable is a backend failure",
			fmt.Errorf("get t1_b/k: %w", engine.ErrAllBackendsUnavailable),
			false,
		},
		{
			"5xx from backend is a backend failure",
			errors.New("api error InternalServerError: Internal Server Error, StatusCode: 500"),
			false,
		},
		{
			"quota is not a miss",
			fmt.Errorf("put t1_b/k: %w", engine.ErrQuotaExceeded),
			false,
		},
		// R6-01: typed aws-sdk-go-v2 shapes, as the S3 client builds them
		// (OperationError → ResponseError → APIError). The empty-body 404 is
		// a GenericAPIError whose code the deserializer derives from the
		// status text; HeadObject misses are the modelled types.NotFound.
		{
			"sdk GET 404 NoSuchKey (typed)",
			sdkErr("GetObject", 404, &types.NoSuchKey{Message: aws.String("The specified key does not exist.")}),
			true,
		},
		{
			"sdk GET 404 empty body → GenericAPIError NotFound",
			sdkErr("GetObject", 404, &smithy.GenericAPIError{Code: "NotFound", Message: "Not Found"}),
			true,
		},
		{
			"sdk HEAD 404 → types.NotFound",
			sdkErr("HeadObject", 404, &types.NotFound{}),
			true,
		},
		{
			"sdk DELETE 404 empty body",
			sdkErr("DeleteObject", 404, &smithy.GenericAPIError{Code: "NotFound", Message: "Not Found"}),
			true,
		},
		{
			"sdk 404 NoSuchBucket is a backend failure",
			sdkErr("GetObject", 404, &types.NoSuchBucket{}),
			false,
		},
		{
			"sdk 500 InternalError is a backend failure",
			sdkErr("GetObject", 500, &smithy.GenericAPIError{Code: "InternalError", Message: "We encountered an internal error."}),
			false,
		},
		{
			"sdk 403 AccessDenied (dead key) is a backend failure",
			sdkErr("GetObject", 403, &smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"}),
			false,
		},
		// R6-02: the recorded backend was unreachable and a fallback's
		// not-found rode along inside the aggregate. Never a miss.
		{
			"unavailable wrapping a fallback's not-found is NOT a miss",
			fmt.Errorf("get t1_b/k: %w: all backends failed: %w", engine.ErrAllBackendsUnavailable,
				errors.New("open /tmp/vaultaire-data/t1_b/k: no such file or directory")),
			false,
		},
		{
			"unavailable wrapping a typed NotFoundError is NOT a miss",
			fmt.Errorf("get t1_b/k: %w: all backends failed: %w", engine.ErrAllBackendsUnavailable,
				engine.ErrNotFound("t1_b", "k")),
			false,
		},
		{
			"client cancellation is not a miss",
			fmt.Errorf("get t1_b/k: operation error S3: GetObject, %w", context.Canceled),
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.missing, isObjectMissingErr(tt.err))
		})
	}
}
