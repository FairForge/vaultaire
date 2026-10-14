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
// The upload goes to its own address (WP-R2-1 Part C): the presign names
// <key>@<id> and returns it as storage_key; complete accepts only an address
// of exactly that shape for its key, finds it on R2, and flips the head row
// (and the version row) to it in the one transaction every writer uses — the
// key's previous object, wherever it is, is retired, never overwritten.
//
// Limits: the uploaded object is registered without user metadata; a
// complete that never comes leaves an unregistered blob R2's lifecycle must
// expire.
//
// The bytes never cross the origin, so they cannot be encrypted at rest: a
// bucket that requires encryption (sse_enabled — a bucket created while a
// master key is set and made public afterwards) is refused at create, and at
// complete the plaintext the client already put on R2 is removed (WP-R8-1
// follow-up A1: no plaintext byte in an sse_enabled bucket).

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

// directUploadBucket checks the bucket is the tenant's, public-read, and
// does not require encryption. It writes the management error itself and
// returns false when not; encrypted reports the refusal was the bucket's
// encryption (complete then removes the bytes already on R2).
func (s *Server) directUploadBucket(w http.ResponseWriter, r *http.Request, tenantID, bucket string) (ok, encrypted bool) {
	if s.db == nil {
		writeManagementError(w, ErrTypeAPI, "internal_error", "no database", "")
		return false, false
	}
	var visibility string
	var sseEnabled bool
	err := s.db.QueryRowContext(r.Context(),
		"SELECT visibility, sse_enabled FROM buckets WHERE tenant_id = $1 AND name = $2", tenantID, bucket).Scan(&visibility, &sseEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		writeManagementError(w, ErrTypeNotFound, "bucket_not_found", "bucket not found", "name")
		return false, false
	}
	if err != nil {
		s.logger.Error("direct upload: read bucket", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to read bucket", "")
		return false, false
	}
	if visibility != "public-read" {
		writeManagementError(w, ErrTypeInvalidRequest, "bucket_not_public",
			"direct uploads are offered for public-read buckets only (the ones served from R2); upload through the S3 endpoint instead", "name")
		return false, false
	}
	if sseEnabled {
		writeManagementError(w, ErrTypeInvalidRequest, "bucket_encrypted",
			"this bucket encrypts objects at rest, and a direct upload never passes through the server that encrypts them; upload through the S3 endpoint instead", "name")
		return false, true
	}
	return true, false
}

func validDirectUploadKey(key string) bool {
	return key != "" && len(key) <= 1024 && !strings.HasPrefix(key, "/") && !strings.Contains(key, "..") && strings.TrimSpace(key) == key
}

type mgmtDirectUpload struct {
	Object      string    `json:"object"`
	Bucket      string    `json:"bucket"`
	Key         string    `json:"key"`
	StorageKey  string    `json:"storage_key"`
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
	if ok, _ := s.directUploadBucket(w, r, tenantID, bucket); !ok {
		return
	}
	if why, tooLong := blobKeyTooLong(r.Context(), s.db, tenantID, bucket, req.Key); tooLong {
		writeManagementError(w, ErrTypeInvalidRequest, "key_too_long", why, "key")
		return
	}
	blob, err := newBlobAddress(req.Key)
	if err != nil {
		s.logger.Error("direct upload: blob address", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to presign the upload", "")
		return
	}
	ctx := common.WithTenantID(r.Context(), tenantID)
	url, err := du.PresignPut(ctx, tenantID+"_"+bucket, blob, req.ContentType, directUploadTTL)
	if err != nil {
		s.logger.Error("direct upload: presign", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to presign the upload", "")
		return
	}
	writeJSON(w, http.StatusCreated, mgmtDirectUpload{
		Object: "direct_upload", Bucket: bucket, Key: req.Key, StorageKey: blob, Method: http.MethodPut, URL: url,
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

// POST /api/v1/manage/buckets/{name}/direct-uploads/complete  {key, storage_key}
func (s *Server) handleMgmtCompleteDirectUpload(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}
	bucket := chi.URLParam(r, "name")
	var req struct {
		Key        string `json:"key"`
		StorageKey string `json:"storage_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	if !validDirectUploadKey(req.Key) {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_key", "key is required, at most 1024 characters, no leading slash", "key")
		return
	}
	// Only the address the presign handed out for THIS key: never the key
	// itself, another key's address or anything else a client makes up.
	if !isBlobAddressOf(req.Key, req.StorageKey) {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_storage_key",
			"storage_key must be the one returned when the upload was created for this key", "storage_key")
		return
	}
	du, err := s.directUploader()
	if err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "direct_upload_unavailable", "direct uploads are not enabled on this deployment", "")
		return
	}
	ctx := common.WithTenantID(r.Context(), tenantID)
	container := tenantID + "_" + bucket
	blob := req.StorageKey
	removeUpload := func(why string) {
		if drv, exists := s.engine.GetDriver("r2"); exists {
			if delErr := drv.Delete(ctx, container, blob); delErr != nil && !isObjectMissingErr(delErr) {
				s.logger.Error("direct upload: could not remove the refused upload from R2", zap.String("why", why),
					zap.String("tenant_id", tenantID), zap.String("bucket", bucket), zap.String("storage_key", blob), zap.Error(delErr))
			}
		}
	}
	if ok, encrypted := s.directUploadBucket(w, r, tenantID, bucket); !ok {
		if encrypted {
			// The client already put plaintext on R2 for a bucket that
			// requires encryption: it must not stay there.
			removeUpload("encrypted bucket")
		}
		return
	}
	size, etag, contentType, err := du.Stat(ctx, container, blob)
	if err != nil {
		writeManagementError(w, ErrTypeNotFound, "object_not_uploaded",
			"the object is not on the backend yet — PUT it to the presigned URL first", "key")
		return
	}
	if lockErr := checkObjectLock(ctx, s.db, tenantID, bucket, req.Key, false); lockErr != nil {
		removeUpload("object lock")
		writeManagementError(w, ErrTypePermission, "object_locked", "the key is under Object Lock; the uploaded object was removed", "key")
		return
	}
	var bill *writeBill
	if s.quotaManager != nil {
		res, qErr := reserveQuota(ctx, s.quotaManager, tenantID, "standard", size)
		if qErr != nil {
			s.logger.Error("direct upload: quota", zap.Error(qErr))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to reserve quota", "")
			return
		}
		bill = &writeBill{Floor: "standard", Reserved: size, FloorReserved: res.Floor}
		if !res.OK {
			// The bytes are already on R2: they must not stay there unregistered.
			removeUpload("quota")
			writeManagementError(w, ErrTypePermission, "quota_exceeded", "storage quota exceeded; the uploaded object was removed", "")
			return
		}
	}
	etag = strings.Trim(etag, `"`)
	_, err = commitBlobWrite(ctx, s.db, nil, blobWrite{TenantID: tenantID, Bucket: bucket, Key: req.Key, Bill: bill,
		Version: &versionWrite{Size: size, ETag: etag, ContentType: contentType, Backend: "r2", Blob: versionBlobRef{StorageKey: blob}},
		Upsert: func(tx *sql.Tx) error {
			return upsertWholeObjectHeadRow(ctx, tx, tenantID, bucket, req.Key, blob, size, etag, "r2", "standard",
				objectAttrs{ContentType: contentType})
		}})
	if err != nil {
		s.logger.Error("direct upload: head row", zap.Error(err))
		s.releaseQuota(context.WithoutCancel(ctx), tenantID, "standard", size)
		retireOrphanWrite(ctx, s.db, s.logger, tenantID, bucket, blob, "r2")
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to register the object", "")
		return
	}
	writeJSON(w, http.StatusOK, mgmtDirectUploadComplete{
		Object: "object", Bucket: bucket, Key: req.Key, Size: size, ETag: etag, ContentType: contentType,
		Backend: "r2", RequestID: getRequestID(w),
	})
}
