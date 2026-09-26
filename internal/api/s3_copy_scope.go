package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/FairForge/vaultaire/internal/auth"
)

// copySourceScopeDenied applies the scoped-key rules to the SOURCE of a
// CopyObject. The dispatcher treats a PUT with x-amz-copy-source as
// PutObject, so the generic checks only see the destination bucket; without
// this a key scoped to one bucket could pull any object of the tenant into it
// and a PutObject-only key could read (review R5-02).
//
// The header is parsed with the same parseCopySource the copy handler uses
// (first header value, leading slash optional, ?versionId stripped, bucket
// NOT percent-decoded) so both sides agree on the bucket name; a source the
// handler would reject as malformed is left to it (400), not denied here.
func copySourceScopeDenied(scope *auth.KeyScope, r *http.Request) (hint string, denied bool) {
	if scope == nil {
		return "", false
	}
	source := r.Header.Get("x-amz-copy-source")
	if source == "" {
		return "", false
	}
	srcBucket, _, err := parseCopySource(source)
	if err != nil {
		return "", false
	}
	if !auth.CheckPermission(scope.Permissions, "GetObject") {
		return "Copying reads the source object: this key does not have GetObject access.", true
	}
	if !auth.CheckBucketScope(scope.BucketScope, srcBucket) {
		return fmt.Sprintf("Copy source bucket %q is outside this key's bucket scope: %s",
			srcBucket, strings.Join(scope.BucketScope, ", ")), true
	}
	return "", false
}
