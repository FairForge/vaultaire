package tenant

import "strings"

// System buckets (WP-R10-3b). A system bucket is a `buckets` row in the
// tenant's own namespace that the service writes into and the customer only
// reads from: the GDPR export lands in ExportsBucket. Its name starts with
// SystemBucketPrefix, which the S3 bucket-name grammar (api.s3BucketNameRe)
// never allows, so a customer cannot create one, and every listing of `buckets`
// that a customer sees carries `name NOT LIKE '\_%'` (the literal, in each
// query — gosec refuses a concatenated fragment) and never shows one. To the S3 API a system
// bucket does not exist (NoSuchBucket) except for GetObject / HeadObject of
// its keys — that is how a presigned export URL is served. The dashboard's
// bucket pages answer 404 for it.
const (
	// SystemBucketPrefix starts every system bucket's name.
	SystemBucketPrefix = "_"
	// ExportsBucket holds the account's GDPR exports (one object per export,
	// 7-day expiry). Never versioned, never lock-enabled: the retention job
	// deletes the objects through the customer delete path.
	ExportsBucket = "_exports"
)

// IsSystemBucket reports whether name is a system bucket's.
func IsSystemBucket(name string) bool {
	return strings.HasPrefix(name, SystemBucketPrefix)
}
