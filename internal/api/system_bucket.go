package api

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/FairForge/vaultaire/internal/tenant"
)

// System buckets on the S3 surface (WP-R10-3b; tenant.ExportsBucket). A
// system bucket is a registry row in the tenant's namespace that the service
// writes: to the S3 API it does not exist — ListBuckets omits it; ListObjects,
// HeadBucket, DeleteBucket, every bucket sub-resource and every object write
// answer NoSuchBucket — except GetObject and HeadObject of its keys, which is
// how a presigned export URL is served. Its name cannot be created through
// CreateBucket (the S3 name grammar refuses a leading '_'), so no customer
// bucket can ever be mistaken for one. The dashboard's bucket pages 404 it
// (internal/dashboard/router.go) and no bucket count includes it.

// systemBucketRefused answers NoSuchBucket for a request that addresses a
// system bucket in any way other than reading one of its objects. It reports
// whether it wrote the response.
func systemBucketRefused(w http.ResponseWriter, r *http.Request, req *S3Request) bool {
	if req == nil || !tenant.IsSystemBucket(req.Bucket) {
		return false
	}
	if req.Object != "" && (req.Operation == "GetObject" || req.Operation == "HeadObject") {
		return false
	}
	WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, generateRequestID())
	return true
}

// countCustomerBuckets counts the tenant's buckets without the system ones
// (the bucket caps are the customer's).
func countCustomerBuckets(ctx context.Context, db *sql.DB, tenantID string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM buckets WHERE tenant_id = $1 AND name NOT LIKE '\_%'`, tenantID).Scan(&n)
	return n, err
}

// mgmtSystemBucketNotFound answers the management API's bucket_not_found for
// a per-bucket route that names a system bucket — the same rule as the S3
// surface (NoSuchBucket) and the dashboard (404): the export bucket is the
// service's, not the customer's to patch, re-tier, pin or delete (a tier or
// a pin would move the export objects; a lock or versioning change would
// make the expiry delete refuse). It reports whether it wrote the response.
func mgmtSystemBucketNotFound(w http.ResponseWriter, name string) bool {
	if !tenant.IsSystemBucket(name) {
		return false
	}
	writeManagementError(w, ErrTypeNotFound, "bucket_not_found", "bucket not found", "name")
	return true
}
