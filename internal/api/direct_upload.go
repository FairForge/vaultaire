package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
)

// Direct uploads (Phase 40.1(a), 2026-10-04): for a PUBLIC bucket the bytes go
// client → R2 on a Vaultaire-issued presigned PUT and never cross the origin;
// the client then calls complete, Vaultaire HEADs the object on R2, reserves
// quota and writes the head row, so routing truth, listing, HEAD and the CDN
// path see the object exactly as if it had been uploaded through the API.
//
// Only public-read buckets qualify: they are the ones placed on R2 anyway
// (publicBucketStorageClass), so a direct upload lands where a proxied one
// would. A private bucket is refused — its backend is not R2 and a presigned
// URL would have to carry a vendor's credentials.
//
// Limits of this first cut: an overwrite of a key whose bytes sit on another
// backend leaves the old blob for the erasure sweep; the uploaded object is
// registered without encryption or user metadata; a complete that never comes
// leaves an unregistered blob R2's lifecycle must expire.

// DirectUploader is what a backend offers for direct uploads. The R2 driver
// implements it.
type DirectUploader interface {
	PresignPut(ctx context.Context, container, artifact, contentType string, ttl time.Duration) (string, error)
	Stat(ctx context.Context, container, artifact string) (size int64, etag, contentType string, err error)
}

const directUploadTTL = 15 * time.Minute

var errDirectUploadUnavailable = errors.New("direct uploads need a registered r2 driver")

func (s *Server) directUploader() (DirectUploader, error) {
	if s.engine == nil {
		return nil, errDirectUploadUnavailable
	}
	drv, exists := s.engine.GetDriver("r2")
	if !exists {
		return nil, errDirectUploadUnavailable
	}
	du, ok := drv.(DirectUploader)
	if !ok {
		return nil, errDirectUploadUnavailable
	}
	return du, nil
}

// directUploadBucket checks the bucket is the tenant's and public-read.
// It writes the management error itself and returns false when not.
func (s *Server) directUploadBucket(w http.ResponseWriter, r *http.Request, tenantID, bucket string) bool {
	if s.db == nil {
		writeManagementError(w, ErrTypeAPI, "internal_error", "no database", "")
		return false
	}
	var visibility string
	err := s.db.QueryRowContext(r.Context(),
		"SELECT visibility FROM buckets WHERE tenant_id = $1 AND name = $2", tenantID, bucket).Scan(&visibility)
	if errors.Is(err, sql.ErrNoRows) {
		writeManagementError(w, ErrTypeNotFound, "bucket_not_found", "bucket not found", "name")
		return false
	}
	if err != nil {
		s.logger.Error("direct upload: read bucket", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to read bucket", "")
		return false
	}
	if visibility != "public-read" {
		writeManagementError(w, ErrTypeInvalidRequest, "bucket_not_public",
			"direct uploads are offered for public-read buckets only (the ones served from R2); upload through the S3 endpoint instead", "name")
		return false
	}
	return true
}

func validDirectUploadKey(key string) bool {
	return key != "" && len(key) <= 1024 && !strings.HasPrefix(key, "/") && !strings.Contains(key, "..") && strings.TrimSpace(key) == key
}

type mgmtDirectUpload struct {
	Object      string    `json:"object"`
	Bucket      string    `json:"bucket"`
	Key         string    `json:"key"`
	Method      string    `json:"method"`
	URL         string    `json:"url"`
	ContentType string    `json:"content_type,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
	Complete    string    `json:"complete"`
	RequestID   string    `json:"request_id,omitempty"`
}

// POST /api/v1/manage/buckets/{name}/direct-uploads  {key, content_type?}
func (s *Server) handleMgmtCreateDirectUpload(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}
	bucket := chi.URLParam(r, "name")
	var req struct {
		Key         string `json:"key"`
		ContentType string `json:"content_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	if !validDirectUploadKey(req.Key) {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_key", "key is required, at most 1024 characters, no leading slash", "key")
		return
	}
	du, err := s.directUploader()
	if err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "direct_upload_unavailable", "direct uploads are not enabled on this deployment", "")
		return
	}
	if !s.directUploadBucket(w, r, tenantID, bucket) {
		return
	}
	ctx := common.WithTenantID(r.Context(), tenantID)
	url, err := du.PresignPut(ctx, tenantID+"_"+bucket, req.Key, req.ContentType, directUploadTTL)
	if err != nil {
		s.logger.Error("direct upload: presign", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to presign the upload", "")
		return
	}
	writeJSON(w, http.StatusCreated, mgmtDirectUpload{
		Object: "direct_upload", Bucket: bucket, Key: req.Key, Method: http.MethodPut, URL: url,
		ContentType: req.ContentType, ExpiresAt: time.Now().UTC().Add(directUploadTTL),
		Complete:  "/api/v1/manage/buckets/" + bucket + "/direct-uploads/complete",
		RequestID: getRequestID(w),
	})
}

type mgmtDirectUploadComplete struct {
	Object      string `json:"object"`
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	ETag        string `json:"etag"`
	ContentType string `json:"content_type,omitempty"`
	Backend     string `json:"backend"`
	RequestID   string `json:"request_id,omitempty"`
}

// POST /api/v1/manage/buckets/{name}/direct-uploads/complete  {key}
func (s *Server) handleMgmtCompleteDirectUpload(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}
	bucket := chi.URLParam(r, "name")
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	if !validDirectUploadKey(req.Key) {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_key", "key is required, at most 1024 characters, no leading slash", "key")
		return
	}
	du, err := s.directUploader()
	if err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "direct_upload_unavailable", "direct uploads are not enabled on this deployment", "")
		return
	}
	if !s.directUploadBucket(w, r, tenantID, bucket) {
		return
	}
	ctx := common.WithTenantID(r.Context(), tenantID)
	container := tenantID + "_" + bucket
	size, etag, contentType, err := du.Stat(ctx, container, req.Key)
	if err != nil {
		writeManagementError(w, ErrTypeNotFound, "object_not_uploaded",
			"the object is not on the backend yet — PUT it to the presigned URL first", "key")
		return
	}
	if s.quotaManager != nil {
		ok, qErr := reserveQuota(ctx, s.quotaManager, tenantID, "standard", size)
		if qErr != nil {
			s.logger.Error("direct upload: quota", zap.Error(qErr))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to reserve quota", "")
			return
		}
		if !ok {
			// The bytes are already on R2: they must not stay there unregistered.
			if drv, exists := s.engine.GetDriver("r2"); exists {
				_ = drv.Delete(ctx, container, req.Key)
			}
			writeManagementError(w, ErrTypePermission, "quota_exceeded", "storage quota exceeded; the uploaded object was removed", "")
			return
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to begin", "")
		return
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertWholeObjectHeadRow(ctx, tx, tenantID, bucket, req.Key, size, etag, "r2", "standard",
		objectAttrs{ContentType: contentType}); err != nil {
		s.logger.Error("direct upload: head row", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to register the object", "")
		return
	}
	if err := tx.Commit(); err != nil {
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to commit", "")
		return
	}
	recordObjectVersion(ctx, s.db, tenantID, bucket, req.Key, size, etag, contentType, "r2")
	writeJSON(w, http.StatusOK, mgmtDirectUploadComplete{
		Object: "object", Bucket: bucket, Key: req.Key, Size: size, ETag: etag, ContentType: contentType,
		Backend: "r2", RequestID: getRequestID(w),
	})
}
