package drivers

import (
	"errors"
	"net/http"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// s3IsNotFound reports whether an aws-sdk-go-v2 error chain means "the OBJECT
// is not there": the NoSuchKey / NotFound API codes (types.NoSuchKey,
// types.NotFound, or the GenericAPIError the deserializer builds from the
// status text when a vendor answers a 404 with no body — every HeadObject
// miss looks like that), or any HTTP 404 from the endpoint.
//
// NoSuchBucket is also a 404 but means the BACKEND is misconfigured; it is
// decided before the status check so it never reads as an absent object.
// Anything else — 403 for a key the credential cannot see, 5xx, transport
// errors — is an error the caller must surface, never a miss.
//
// This is the driver-side twin of engine.isSDKNotFound / api.isObjectMissingErr
// (Review R6-01); it replaced the `strings.Contains(err.Error(), "404")`
// checks in every Exists (Review R7-05), where a request id or message
// containing "404" was a false miss.
func s3IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		case "NoSuchBucket":
			return false
		}
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return false
}
