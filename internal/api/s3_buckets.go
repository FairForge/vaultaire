package api

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/usage"
	"go.uber.org/zap"
)

var s3BucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]{1,61}[a-z0-9]$`)

func validateBucketName(name string) bool {
	return s3BucketNameRe.MatchString(name) && !strings.Contains(name, "..")
}

func safeBucketPath(base, tenantID, bucket string) (string, bool) {
	p := filepath.Join(base, tenantID, bucket)
	if !strings.HasPrefix(filepath.Clean(p), filepath.Clean(base)+string(filepath.Separator)) {
		return "", false
	}
	return p, true
}

// ListBucketsResponse for S3 API
type ListBucketsResponse struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Owner   struct {
		ID          string `xml:"ID"`
		DisplayName string `xml:"DisplayName"`
	} `xml:"Owner"`
	Buckets struct {
		Bucket []BucketInfo `xml:"Bucket"`
	} `xml:"Buckets"`
}

type BucketInfo struct {
	Name         string    `xml:"Name"`
	CreationDate time.Time `xml:"CreationDate"`
}

// ListBuckets handles S3 ListBuckets operation
func (s *Server) ListBuckets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get tenant from context
	t, err := tenant.FromContext(ctx)
	tenantID := "default"
	if err == nil && t != nil {
		tenantID = t.ID
	}

	response := ListBucketsResponse{}
	response.Owner.ID = tenantID
	response.Owner.DisplayName = tenantID

	if s.db != nil {
		rows, dbErr := s.db.QueryContext(ctx,
			`SELECT name, created_at FROM buckets WHERE tenant_id = $1 ORDER BY name`, tenantID)
		if dbErr != nil {
			s.logger.Error("list buckets from DB", zap.Error(dbErr))
		} else {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var bi BucketInfo
				if err := rows.Scan(&bi.Name, &bi.CreationDate); err != nil {
					s.logger.Error("scan bucket row", zap.Error(err))
					continue
				}
				response.Buckets.Bucket = append(response.Buckets.Bucket, bi)
			}
		}
	} else {
		basePath := filepath.Join("/tmp/vaultaire", tenantID)
		entries, err := os.ReadDir(basePath)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
					response.Buckets.Bucket = append(response.Buckets.Bucket, BucketInfo{
						Name:         entry.Name(),
						CreationDate: time.Now(),
					})
				}
			}
		} else if !os.IsNotExist(err) {
			s.logger.Error("Failed to list containers", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/xml")
	if err := xml.NewEncoder(w).Encode(response); err != nil {
		s.logger.Error("Failed to encode response", zap.Error(err))
	}
}

// freeTierBucketCapBlocksWrite reports whether an object write into bucket
// must be refused because the tenant is on the free tier, already holds its
// FreeTierLimits.MaxBuckets bucket rows, and bucket is not one of them.
// CreateBucket enforces the cap, but PUT / CopyObject / multipart auto-create
// the container and never looked (R2-19 = R5-27, Review R10-12). Paid tenants
// and tenants under the cap are untouched; existing phantom containers keep
// working (the wider "PUT needs a bucket row" rule is R4's, WP-R5-13).
func (s *Server) freeTierBucketCapBlocksWrite(ctx context.Context, tenantID, bucket string) bool {
	if s.db == nil || s.quotaManager == nil || tenantID == "" || bucket == "" {
		return false
	}
	tier, err := s.quotaManager.GetTier(ctx, tenantID)
	if err != nil || !usage.IsFreeTier(tier) {
		return false
	}
	var owned bool
	if err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)",
		tenantID, bucket).Scan(&owned); err != nil || owned {
		return false
	}
	var count int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM buckets WHERE tenant_id = $1", tenantID).Scan(&count); err != nil {
		return false
	}
	return count >= usage.FreeTierLimits.MaxBuckets
}

// writeFreeTierBucketCap answers the refusal the way CreateBucket does.
func writeFreeTierBucketCap(w http.ResponseWriter, r *http.Request) {
	WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
		WithSuggestion(fmt.Sprintf("Free tier allows %d bucket. Upgrade at https://stored.ge/dashboard/billing", usage.FreeTierLimits.MaxBuckets)))
}

// CreateBucket handles S3 CreateBucket operation
func (s *Server) CreateBucket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse bucket name from the path
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket := strings.SplitN(path, "/", 2)[0]

	if !validateBucketName(bucket) {
		WriteS3Error(w, ErrInvalidBucketName, r.URL.Path, generateRequestID())
		return
	}

	// Get tenant from context
	t, err := tenant.FromContext(ctx)
	tenantID := "default"
	if err == nil && t != nil {
		tenantID = t.ID
	}

	s.logger.Info("Creating bucket",
		zap.String("bucket", bucket),
		zap.String("tenant", tenantID))

	if s.db != nil && tenantID != "default" {
		// Idempotent re-creation: if the tenant already owns this bucket, skip
		// the quota gate entirely. CreateBucket on a bucket you own is a no-op
		// (BucketAlreadyOwnedByYou) in S3 — it must not 403 just because you're
		// at the bucket limit, or `aws s3 mb`/terraform/rclone ensure-bucket
		// calls break once a free-tier tenant has their one bucket.
		var alreadyOwned bool
		_ = s.db.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)",
			tenantID, bucket).Scan(&alreadyOwned)

		if !alreadyOwned {
			var count int
			_ = s.db.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM buckets WHERE tenant_id = $1", tenantID).Scan(&count)

			const maxBucketsPerTenant = 1000
			if count >= maxBucketsPerTenant {
				WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
					WithSuggestion(fmt.Sprintf("Maximum %d buckets per account", maxBucketsPerTenant)))
				return
			}

			if s.quotaManager != nil {
				tier, _ := s.quotaManager.GetTier(ctx, tenantID)
				if usage.IsFreeTier(tier) && count >= usage.FreeTierLimits.MaxBuckets {
					WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
						WithSuggestion(fmt.Sprintf("Free tier allows %d bucket. Upgrade at https://stored.ge/dashboard/billing", usage.FreeTierLimits.MaxBuckets)))
					return
				}
			}
		}
	}

	// Parse region: header takes precedence, then XML body, then default.
	region := r.Header.Get("X-Stored-Region")
	if region == "" {
		region = r.Header.Get("x-amz-bucket-region")
	}
	if region == "" && r.ContentLength > 0 {
		body, readErr := io.ReadAll(io.LimitReader(r.Body, 4096))
		if readErr != nil && errors.Is(readErr, auth.ErrContentSHA256Mismatch) {
			WriteS3Error(w, ErrXAmzContentSHA256Mismatch, r.URL.Path, generateRequestID())
			return
		}
		if readErr == nil && len(body) > 0 {
			var cfg struct {
				XMLName            xml.Name `xml:"CreateBucketConfiguration"`
				LocationConstraint string   `xml:"LocationConstraint"`
			}
			if xml.Unmarshal(body, &cfg) == nil && cfg.LocationConstraint != "" {
				region = cfg.LocationConstraint
			}
		}
	}
	if region == "" {
		region = drivers.IDriveDefaultRegion(os.Getenv)
	}
	if !drivers.IsValidRegion(region) {
		WriteS3Error(w, ErrInvalidLocationConstraint, r.URL.Path, generateRequestID())
		return
	}
	// A region the account has but this deployment has no driver for must be
	// refused: accepting the bucket would silently store its objects on the
	// primary — a data-residency breach with a truthful-looking label
	// (Review R7-01 / WP-R7-1).
	if !drivers.IDriveRegionAvailable(region) {
		WriteS3ErrorWithContext(w, ErrInvalidLocationConstraint, r.URL.Path, generateRequestID(),
			WithSuggestion(fmt.Sprintf("Region %s is not enabled on this deployment.", region)))
		return
	}

	// Create container directory
	dirPath, safe := safeBucketPath("/tmp/vaultaire", tenantID, bucket)
	if !safe {
		WriteS3Error(w, ErrInvalidBucketName, r.URL.Path, generateRequestID())
		return
	}
	if err := os.MkdirAll(dirPath, 0755); err != nil { // #nosec G301 -- bucket dirs need read access
		s.logger.Error("Failed to create container", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	if s.db != nil {
		sseDefault := s.sseService != nil
		_, dbErr := s.db.ExecContext(ctx, `
			INSERT INTO buckets (tenant_id, name, visibility, sse_enabled, region)
			VALUES ($1, $2, 'private', $3, $4)
			ON CONFLICT (tenant_id, name) DO NOTHING
		`, tenantID, bucket, sseDefault, region)
		if dbErr != nil {
			s.logger.Error("failed to persist bucket",
				zap.Error(dbErr), zap.String("bucket", bucket))
		}

		auth.EnsureTenantSlug(ctx, s.db, tenantID, s.logger)
	}

	// Re-creating an owned bucket is a no-op (ON CONFLICT DO NOTHING): the
	// header describes the region the bucket HAS, not the one requested.
	if s.db != nil {
		var stored string
		if err := s.db.QueryRowContext(ctx, `SELECT region FROM buckets WHERE tenant_id = $1 AND name = $2`,
			tenantID, bucket).Scan(&stored); err == nil && stored != "" {
			region = stored
		}
	}
	w.Header().Set("x-amz-bucket-region", region)
	w.Header().Set("Location", "/"+bucket)
	emitEvent(ctx, s.db, s.logger, "bucket.created", tenantID, map[string]interface{}{
		"bucket": bucket,
		"region": region,
	})
	w.WriteHeader(http.StatusOK)
}

// LocationConstraintResponse is the S3 GetBucketLocation response.
type LocationConstraintResponse struct {
	XMLName  xml.Name `xml:"LocationConstraint"`
	Location string   `xml:",chardata"`
}

// handleGetBucketLocation returns the region constraint for a bucket.
func (s *Server) handleGetBucketLocation(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	var region string
	if s.db != nil {
		err := s.db.QueryRowContext(r.Context(),
			`SELECT region FROM buckets WHERE tenant_id = $1 AND name = $2`,
			t.ID, req.Bucket).Scan(&region)
		if err != nil {
			WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, generateRequestID())
			return
		}
	} else {
		region = drivers.IDriveDefaultRegion(os.Getenv)
	}

	resp := LocationConstraintResponse{Location: region}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-bucket-region", region)
	if err := xml.NewEncoder(w).Encode(resp); err != nil {
		s.logger.Error("encode location response", zap.Error(err))
	}
}

// handleHeadBucket handles S3 HEAD bucket — returns 200 if bucket exists.
func (s *Server) handleHeadBucket(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	if s.db != nil {
		var region string
		err := s.db.QueryRowContext(r.Context(),
			`SELECT region FROM buckets WHERE tenant_id = $1 AND name = $2`,
			t.ID, req.Bucket).Scan(&region)
		if err != nil {
			WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, generateRequestID())
			return
		}
		w.Header().Set("x-amz-bucket-region", region)
	}
	w.WriteHeader(http.StatusOK)
}

// bucketDeleteState is the outcome of deleteBucketRegistry.
type bucketDeleteState int

const (
	bucketDeleteDone bucketDeleteState = iota
	bucketDeleteMissing
	bucketDeleteNotEmpty
)

type bucketDeleteOutcome struct {
	state                      bucketDeleteState
	objects, versions, uploads int
}

func (o bucketDeleteOutcome) contents() string {
	return fmt.Sprintf("The bucket still holds %d object(s), %d version(s) and %d in-progress multipart upload(s).",
		o.objects, o.versions, o.uploads)
}

// deleteBucketRegistry is the one bucket-delete decision for every entry
// point (S3 DeleteBucket, the management API): the registry row must exist;
// objects, versions/delete markers and in-progress multipart uploads keep
// the bucket (AWS 409 BucketNotEmpty); otherwise the row goes together with
// the bucket's own configuration rows (notification targets — a re-created
// bucket must not inherit the previous one's webhooks). No filesystem is
// consulted: the /tmp or DATA_PATH marker directory said nothing about the
// bucket's objects (R4-04 on the S3 path, post-merge R4-22 on the
// management API).
func (s *Server) deleteBucketRegistry(ctx context.Context, tenantID, bucket string) (bucketDeleteOutcome, error) {
	var out bucketDeleteOutcome
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)`,
		tenantID, bucket).Scan(&exists); err != nil {
		return out, fmt.Errorf("delete bucket %s: registry lookup: %w", bucket, err)
	}
	if !exists {
		out.state = bucketDeleteMissing
		return out, nil
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2),
			(SELECT COUNT(*) FROM object_versions WHERE tenant_id = $1 AND bucket = $2),
			(SELECT COUNT(*) FROM multipart_uploads WHERE tenant_id = $1 AND bucket = $2 AND status = 'active')`,
		tenantID, bucket).Scan(&out.objects, &out.versions, &out.uploads); err != nil {
		return out, fmt.Errorf("delete bucket %s: contents lookup: %w", bucket, err)
	}
	if out.objects > 0 || out.versions > 0 || out.uploads > 0 {
		out.state = bucketDeleteNotEmpty
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("delete bucket %s: begin: %w", bucket, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM bucket_notifications WHERE tenant_id = $1 AND bucket = $2`, tenantID, bucket); err != nil {
		return out, fmt.Errorf("delete bucket %s: notification targets: %w", bucket, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM buckets WHERE tenant_id = $1 AND name = $2`, tenantID, bucket); err != nil {
		return out, fmt.Errorf("delete bucket %s: registry row: %w", bucket, err)
	}
	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("delete bucket %s: commit: %w", bucket, err)
	}
	out.state = bucketDeleteDone
	return out, nil
}

// DeleteBucket handles S3 DeleteBucket operation
func (s *Server) DeleteBucket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse bucket name from the path
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket := strings.SplitN(path, "/", 2)[0]

	if !validateBucketName(bucket) {
		WriteS3Error(w, ErrInvalidBucketName, r.URL.Path, generateRequestID())
		return
	}

	// Get tenant from context
	t, err := tenant.FromContext(ctx)
	tenantID := "default"
	if err == nil && t != nil {
		tenantID = t.ID
	}

	s.logger.Info("Deleting bucket",
		zap.String("bucket", bucket),
		zap.String("tenant", tenantID))

	dirPath, safe := safeBucketPath("/tmp/vaultaire", tenantID, bucket)
	if !safe {
		WriteS3Error(w, ErrInvalidBucketName, r.URL.Path, generateRequestID())
		return
	}

	// With a database the registry decides (R4-04): the marker directory
	// CreateBucket leaves under /tmp said nothing about the bucket's objects
	// (a non-empty bucket was deleted; a bucket whose marker was wiped at
	// boot could not be). Objects, versions/delete markers and in-progress
	// multipart uploads all keep the bucket (AWS 409 BucketNotEmpty).
	if s.db != nil && tenantID != "default" {
		outcome, err := s.deleteBucketRegistry(ctx, tenantID, bucket)
		if err != nil {
			s.logger.Error("delete bucket: registry delete failed", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		switch outcome.state {
		case bucketDeleteMissing:
			reqID := generateRequestID()
			if suggestion := bucketSuggestion(ctx, s.db, tenantID, bucket); suggestion != "" {
				WriteS3ErrorWithContext(w, ErrNoSuchBucket, r.URL.Path, reqID, WithSuggestion(suggestion))
			} else {
				WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, reqID)
			}
			return
		case bucketDeleteNotEmpty:
			WriteS3ErrorWithContext(w, ErrBucketNotEmpty, r.URL.Path, generateRequestID(),
				WithSuggestion(outcome.contents()))
			return
		}
		_ = os.RemoveAll(dirPath) // the marker directory, if any
		emitEvent(ctx, s.db, s.logger, "bucket.deleted", tenantID, map[string]interface{}{
			"bucket": bucket,
		})
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// No database (dev): the marker directory is all there is.
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		reqID := generateRequestID()
		if suggestion := bucketSuggestion(ctx, s.db, tenantID, bucket); suggestion != "" {
			WriteS3ErrorWithContext(w, ErrNoSuchBucket, r.URL.Path, reqID, WithSuggestion(suggestion))
		} else {
			WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, reqID)
		}
		return
	}

	// Check if empty (ignoring .meta files)
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		s.logger.Error("Failed to read bucket", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Count non-meta files
	nonMetaCount := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".meta") && !strings.HasPrefix(name, ".") {
			nonMetaCount++
		}
	}

	if nonMetaCount > 0 {
		WriteS3Error(w, ErrBucketNotEmpty, r.URL.Path, generateRequestID())
		return
	}

	// Delete the bucket
	if err := os.RemoveAll(dirPath); err != nil {
		s.logger.Error("Failed to delete bucket", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	if s.db != nil {
		_, _ = s.db.ExecContext(ctx, `
			DELETE FROM buckets WHERE tenant_id = $1 AND name = $2
		`, tenantID, bucket)
	}

	emitEvent(ctx, s.db, s.logger, "bucket.deleted", tenantID, map[string]interface{}{
		"bucket": bucket,
	})
	w.WriteHeader(http.StatusNoContent)
}
