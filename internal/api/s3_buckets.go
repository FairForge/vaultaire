package api

import (
	"encoding/xml"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

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

	// Get tenant from context
	t, err := tenant.FromContext(ctx)
	tenantID := "default"
	if err == nil && t != nil {
		tenantID = t.ID
	}

	s.logger.Info("Creating bucket",
		zap.String("bucket", bucket),
		zap.String("tenant", tenantID))

	// Create container directory
	dirPath := filepath.Join("/tmp/vaultaire", tenantID, bucket)
	if err := os.MkdirAll(dirPath, 0755); err != nil { // #nosec G703 — TODO: add path sanitization to reject ../ in bucket names
		s.logger.Error("Failed to create container", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	if s.db != nil {
		_, dbErr := s.db.ExecContext(ctx, `
			INSERT INTO buckets (tenant_id, name, visibility)
			VALUES ($1, $2, 'private')
			ON CONFLICT (tenant_id, name) DO NOTHING
		`, tenantID, bucket)
		if dbErr != nil {
			s.logger.Error("failed to persist bucket",
				zap.Error(dbErr), zap.String("bucket", bucket))
		}

		auth.EnsureTenantSlug(ctx, s.db, tenantID, s.logger)
	}

	w.WriteHeader(http.StatusOK)
}

// DeleteBucket handles S3 DeleteBucket operation
func (s *Server) DeleteBucket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse bucket name from the path
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket := strings.SplitN(path, "/", 2)[0]

	// Get tenant from context
	t, err := tenant.FromContext(ctx)
	tenantID := "default"
	if err == nil && t != nil {
		tenantID = t.ID
	}

	s.logger.Info("Deleting bucket",
		zap.String("bucket", bucket),
		zap.String("tenant", tenantID))

	dirPath := filepath.Join("/tmp/vaultaire", tenantID, bucket)

	// Check if bucket exists
	if _, err := os.Stat(dirPath); os.IsNotExist(err) { // #nosec G703 — TODO: add path sanitization to reject ../ in bucket names
		WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, generateRequestID())
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
	if err := os.RemoveAll(dirPath); err != nil { // #nosec G703 — TODO: add path sanitization to reject ../ in bucket names
		s.logger.Error("Failed to delete bucket", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	if s.db != nil {
		_, _ = s.db.ExecContext(ctx, `
			DELETE FROM buckets WHERE tenant_id = $1 AND name = $2
		`, tenantID, bucket)
	}

	w.WriteHeader(http.StatusNoContent)
}
