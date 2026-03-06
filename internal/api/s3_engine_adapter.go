package api

import (
	"context"
	"crypto/md5"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

// S3ToEngine adapts S3 requests to engine operations.
// The db field enables writing object metadata to PostgreSQL on PUT,
// so HEAD requests can be served from the database instead of fetching
// the full object from the backend.
type S3ToEngine struct {
	engine engine.Engine
	db     *sql.DB
	logger *zap.Logger
}

// NewS3ToEngine creates a new adapter
func NewS3ToEngine(e engine.Engine, db *sql.DB, logger *zap.Logger) *S3ToEngine {
	return &S3ToEngine{
		engine: e,
		db:     db,
		logger: logger,
	}
}

// TranslateRequest converts S3 terminology to engine terminology
func (a *S3ToEngine) TranslateRequest(req *S3Request) engine.Operation {
	return engine.Operation{
		Type:      req.Operation,
		Container: req.Bucket, // Bucket → Container
		Artifact:  req.Object, // Object → Artifact
		Context:   context.Background(),
		Metadata:  make(map[string]interface{}),
	}
}

// HandleGet processes S3 GET requests using the engine
func (a *S3ToEngine) HandleGet(w http.ResponseWriter, r *http.Request, bucket, object string) {
	// Check for Range header early
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" {
		// For now, ignore range and return full file
		// TODO: Implement proper range support
		a.logger.Debug("Range request ignored",
			zap.String("range", rangeHeader))
	}

	// Get tenant from context
	t, err := tenant.FromContext(r.Context())
	if err != nil {
		a.logger.Warn("no tenant in context", zap.Error(err))
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	// Translate S3 terms to Engine terms with tenant namespace
	container := t.NamespaceContainer(bucket) // S3 Bucket → Namespaced Container
	artifact := object                        // S3 Object → Engine Artifact

	a.logger.Debug("GET with tenant isolation",
		zap.String("tenant_id", t.ID),
		zap.String("original_bucket", bucket),
		zap.String("namespaced_container", container),
		zap.String("artifact", artifact))

	// Call engine with Container/Artifact terminology
	reader, err := a.engine.Get(r.Context(), container, artifact)
	if err != nil {
		// Map engine errors to S3 errors
		if strings.Contains(err.Error(), "no such file or directory") ||
			strings.Contains(err.Error(), "not found") {
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
		} else {
			a.logger.Error("engine get failed",
				zap.Error(err),
				zap.String("container", container),
				zap.String("artifact", artifact))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		}
		return
	}
	defer func() { _ = reader.Close() }()

	// Set S3-compliant response headers.
	// ETag is intentionally omitted here — GET does not need it and we
	// don't have the hash without re-reading the object. HEAD (which does
	// need ETag) is served from object_head_cache, not this path.
	contentType := a.detectContentType(artifact)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.Header().Set("x-amz-version-id", "null")
	w.Header().Set("Accept-Ranges", "bytes")

	// Stream the content
	written, err := io.Copy(w, reader)
	if err != nil {
		a.logger.Error("failed to stream artifact",
			zap.Error(err),
			zap.String("container", container),
			zap.String("artifact", artifact))
		return
	}

	a.logger.Info("artifact retrieved",
		zap.String("s3.bucket", bucket),
		zap.String("s3.object", object),
		zap.String("engine.container", container),
		zap.String("engine.artifact", artifact),
		zap.Int64("bytes", written))
}

// detectContentType determines MIME type from extension
func (a *S3ToEngine) detectContentType(artifact string) string {
	// Find the last dot for the extension
	dotIdx := strings.LastIndex(artifact, ".")
	if dotIdx == -1 {
		return "application/octet-stream"
	}
	ext := strings.ToLower(artifact[dotIdx:])

	mimeTypes := map[string]string{
		".txt":  "text/plain",
		".html": "text/html",
		".css":  "text/css",
		".js":   "application/javascript",
		".json": "application/json",
		".xml":  "application/xml",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".png":  "image/png",
		".gif":  "image/gif",
		".pdf":  "application/pdf",
		".zip":  "application/zip",
	}

	if mime, ok := mimeTypes[ext]; ok {
		return mime
	}
	return "application/octet-stream"
}

// HandlePut processes S3 PUT requests using the engine.
//
// The request body is wrapped in an io.TeeReader that feeds every byte
// into an MD5 hasher as the data streams through to the engine. This
// means the ETag is computed in a single pass at zero extra memory cost.
//
// After a successful engine.Put, the size, ETag, and content-type are
// persisted to object_head_cache so that HEAD requests can be answered
// from PostgreSQL without touching the storage backend.
func (a *S3ToEngine) HandlePut(w http.ResponseWriter, r *http.Request, bucket, object string) {
	// Get tenant from context
	t, err := tenant.FromContext(r.Context())
	if err != nil {
		a.logger.Warn("no tenant in context", zap.Error(err))
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	container := t.NamespaceContainer(bucket)
	artifact := object

	a.logger.Debug("PUT with tenant isolation",
		zap.String("tenant_id", t.ID),
		zap.String("original_bucket", bucket),
		zap.String("namespaced_container", container),
		zap.String("artifact", artifact))

	// Wrap the request body in a TeeReader so the MD5 hash is computed
	// as data flows through — no second pass, no buffering in memory.
	hasher := md5.New()
	hashingBody := io.TeeReader(r.Body, hasher)

	err = a.engine.Put(r.Context(), container, artifact, hashingBody)
	if err != nil {
		a.logger.Error("engine put failed",
			zap.Error(err),
			zap.String("container", container),
			zap.String("artifact", artifact))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Compute ETag from the MD5 bytes accumulated during the upload.
	// S3 standard: ETag = hex-encoded MD5, wrapped in double quotes in headers.
	etag := fmt.Sprintf("%x", hasher.Sum(nil))

	// Persist metadata to object_head_cache.
	// This is what makes HEAD O(1) instead of O(file_size).
	// ON CONFLICT handles re-uploads of the same key.
	// This INSERT is non-fatal: if it fails the object is still safely
	// stored; HEAD will return 404 until the next successful upload.
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if a.db != nil {
		_, dbErr := a.db.ExecContext(r.Context(), `
			INSERT INTO object_head_cache
				(tenant_id, bucket, object_key, size_bytes, etag, content_type, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, NOW())
			ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
				size_bytes   = EXCLUDED.size_bytes,
				etag         = EXCLUDED.etag,
				content_type = EXCLUDED.content_type,
				updated_at   = NOW()
		`, t.ID, bucket, artifact, r.ContentLength, etag, contentType)
		if dbErr != nil {
			a.logger.Error("failed to cache object metadata — HEAD will return 404 for this object until next upload",
				zap.Error(dbErr),
				zap.String("tenant_id", t.ID),
				zap.String("bucket", bucket),
				zap.String("object", artifact))
			// Do NOT return an error — the object is safely stored.
		}
	}

	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.WriteHeader(http.StatusOK)

	a.logger.Info("artifact stored",
		zap.String("tenant_id", t.ID),
		zap.String("s3.bucket", bucket),
		zap.String("s3.object", object),
		zap.String("etag", etag),
		zap.Int64("size", r.ContentLength))
}

// HandleDelete processes S3 DELETE requests
func (a *S3ToEngine) HandleDelete(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil {
		a.logger.Warn("no tenant in context", zap.Error(err))
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	container := t.NamespaceContainer(bucket)

	if err := a.engine.Delete(r.Context(), container, object); err != nil {
		if strings.Contains(err.Error(), "no such file or directory") ||
			strings.Contains(err.Error(), "not found") {
			// S3 compatibility: DELETE of non-existent object is success
			w.WriteHeader(http.StatusNoContent)
			return
		}
		a.logger.Error("delete failed",
			zap.String("container", container),
			zap.String("artifact", object),
			zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Best-effort: remove from metadata cache.
	// Non-fatal if this fails — stale cache rows are harmless (HEAD returns
	// data for a deleted object, which is only a consistency issue, not data loss).
	if a.db != nil {
		_, _ = a.db.ExecContext(r.Context(), `
			DELETE FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		`, t.ID, bucket, object)
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleList processes S3 LIST requests
func (a *S3ToEngine) HandleList(w http.ResponseWriter, r *http.Request, bucket, prefix string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil {
		a.logger.Warn("no tenant in context", zap.Error(err))
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	container := t.NamespaceContainer(bucket)

	artifacts, err := a.engine.List(r.Context(), container, prefix)
	if err != nil {
		a.logger.Error("list failed",
			zap.String("container", container),
			zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	w.Header().Set("Content-Type", "application/xml")

	if _, err := w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>`)); err != nil {
		a.logger.Error("failed to write XML header", zap.Error(err))
		return
	}
	if _, err := w.Write([]byte(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)); err != nil {
		a.logger.Error("failed to write response", zap.Error(err))
		return
	}
	if _, err := fmt.Fprintf(w, "<Name>%s</Name>", bucket); err != nil {
		a.logger.Error("failed to write bucket name", zap.Error(err))
		return
	}

	for _, artifact := range artifacts {
		if _, err := w.Write([]byte("<Contents>")); err != nil {
			a.logger.Error("failed to write contents tag", zap.Error(err))
			return
		}
		if _, err := fmt.Fprintf(w, "<Key>%s</Key>", artifact.Key); err != nil {
			a.logger.Error("failed to write key", zap.Error(err))
			return
		}
		if _, err := fmt.Fprintf(w, "<Size>%d</Size>", artifact.Size); err != nil {
			a.logger.Error("failed to write size", zap.Error(err))
			return
		}
		if _, err := fmt.Fprintf(w, "<LastModified>%s</LastModified>",
			time.Now().UTC().Format("2006-01-02T15:04:05.000Z")); err != nil {
			a.logger.Error("failed to write last modified", zap.Error(err))
			return
		}
		if _, err := w.Write([]byte("<StorageClass>STANDARD</StorageClass>")); err != nil {
			a.logger.Error("failed to write storage class", zap.Error(err))
			return
		}
		if _, err := w.Write([]byte("</Contents>")); err != nil {
			a.logger.Error("failed to write contents closing tag", zap.Error(err))
			return
		}
	}

	if _, err := w.Write([]byte("</ListBucketResult>")); err != nil {
		a.logger.Error("failed to write closing tag", zap.Error(err))
		return
	}
}
