package api

import (
	"encoding/xml"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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

	// List containers for this tenant
	basePath := filepath.Join("/tmp/vaultaire", tenantID)

	entries, err := os.ReadDir(basePath)
	if err != nil {
		if os.IsNotExist(err) {
			// No buckets yet, return empty list
			response := ListBucketsResponse{}
			response.Owner.ID = tenantID
			response.Owner.DisplayName = tenantID

			w.Header().Set("Content-Type", "application/xml")
			xml.NewEncoder(w).Encode(response)
			return
		}
		s.logger.Error("Failed to list containers", zap.Error(err))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Build response
	response := ListBucketsResponse{}
	response.Owner.ID = tenantID
	response.Owner.DisplayName = tenantID

	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			response.Buckets.Bucket = append(response.Buckets.Bucket, BucketInfo{
				Name:         entry.Name(),
				CreationDate: time.Now(),
			})
		}
	}

	// Send XML response
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
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		s.logger.Error("Failed to create container", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
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
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
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
	if err := os.RemoveAll(dirPath); err != nil {
		s.logger.Error("Failed to delete bucket", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
