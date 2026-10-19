package api

import (
	"fmt"
	"net/http"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/tenant"
)

// maxS3KeyBytes is S3's object key limit: 1,024 bytes of UTF-8. It is the
// dispatch gate's, for every operation. It is NOT maxObjectKeyBytes
// (blob_address.go, 857): a write must also fit the fixed-bucket prefix and
// the blob address suffix, so the write paths keep their stricter refusal
// (blobKeyTooLong) — the two limits are different on purpose, never unify
// them.
const maxS3KeyBytes = 1024

// requestNameRefused is the dispatch gate on the names a request carries
// (WP-R2-7: R2-25, R3-27), run in handleS3Request after the scope checks
// and systemBucketRefused, before any handler, quota or body byte:
//
//   - a bucket name that is not a valid S3 name → 400 InvalidBucketName. A
//     system bucket (`_` prefix) is exempt: systemBucketRefused has already
//     answered NoSuchBucket to anything but a read of its objects, and
//     InvalidBucketName would tell a caller that `_` names are special.
//     CreateBucket and DeleteBucket keep their own check (s3_buckets.go).
//   - a key over maxS3KeyBytes → 400 KeyTooLongError, whatever the
//     operation (a GET of a 1,100-byte key used to walk into the handlers
//     and 500 three times on a backend that cannot name it).
//
// It reports whether it wrote the response. Batch keys arrive in the body
// and are refused per key by deleteObjects.
func requestNameRefused(w http.ResponseWriter, r *http.Request, req *S3Request) bool {
	if req.Bucket != "" && !tenant.IsSystemBucket(req.Bucket) &&
		req.Operation != auth.OpCreateBucket && req.Operation != auth.OpDeleteBucket &&
		!validateBucketName(req.Bucket) {
		WriteS3Error(w, ErrInvalidBucketName, r.URL.Path, generateRequestID())
		return true
	}
	if len(req.Object) > maxS3KeyBytes {
		WriteS3ErrorWithContext(w, ErrKeyTooLong, r.URL.Path, generateRequestID(),
			WithSuggestion(keyTooLongHint(req.Object)))
		return true
	}
	return false
}

func keyTooLongHint(key string) string {
	return fmt.Sprintf("Object keys are limited to %d bytes (this key is %d).", maxS3KeyBytes, len(key))
}
