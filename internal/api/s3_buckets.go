package api

import (
	"encoding/xml"
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

	// Parse region: header takes precedence, then XML body, then default.
	region := r.Header.Get("X-Stored-Region")
	if region == "" {
		region = r.Header.Get("x-amz-bucket-region")
	}
	if region == "" && r.ContentLength > 0 {
		body, readErr := io.ReadAll(io.LimitReader(r.Body, 4096))
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
		region = "us-west-1"
	}
	if !drivers.IsValidRegion(region) {
		WriteS3Error(w, ErrInvalidLocationConstraint, r.URL.Path, generateRequestID())
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

	w.Header().Set("x-amz-bucket-region", region)
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
		region = "us-west-1"
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

	// Check if bucket exists
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
