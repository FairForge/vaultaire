package api

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/FairForge/vaultaire/internal/engine"
)

// isObjectMissingErr reports whether an engine operation error means the
// object does not exist, as opposed to the backend failing. Misses arrive in
// different shapes depending on which driver served the request: the engine's
// typed NotFoundError, os.ErrNotExist from the local driver, or an
// aws-sdk-go-v2 error chain from the S3-class drivers (iDrive, Lyve, R2,
// Geyser, s3) — types.NoSuchKey, types.NotFound, or the GenericAPIError the
// deserializer builds from the status text ("api error NotFound: Not Found")
// when a vendor sends a 404 with no error body. Handlers must map every shape
// to 404 NoSuchKey: matching only the local driver's strings turned each miss
// on an S3-class backend into a 500 in prod (found via the LET demo,
// 2026-07-31); matching the SDK by text alone missed the empty-body 404s
// (R6-01), so the SDK shapes are now checked by type.
//
// Precedence (R6-02): an error wrapping engine.ErrAllBackendsUnavailable is
// NEVER a miss, whatever a fallback backend said inside it — the backend that
// holds the object was unreachable, and "does not exist" would be a lie the
// client acts on (sync tools delete or re-upload on 404). Handlers answer 503.
//
// Mirrors the taxonomy in engine.isBackendFailure — keep the two in sync.
func isObjectMissingErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, engine.ErrAllBackendsUnavailable) {
		return false
	}
	var nf engine.NotFoundError
	var nfp *engine.NotFoundError
	if errors.As(err, &nf) || errors.As(err, &nfp) {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		case "NoSuchBucket":
			return false // the backend is misconfigured, not the object absent
		}
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	msg := err.Error()
	for _, s := range []string{
		"no such file or directory",
		"not found",
		"NoSuchKey",
		"NotFound",
		"itemNotFound", // Graph API (permafrost)
		"StatusCode: 404",
		"status code: 404",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
