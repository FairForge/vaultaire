package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

// S3ToEngine adapts S3 requests to engine operations
type S3ToEngine struct {
	engine engine.Engine
	logger *zap.Logger
}

// NewS3ToEngine creates a new adapter
func NewS3ToEngine(e engine.Engine, logger *zap.Logger) *S3ToEngine {
	return &S3ToEngine{
		engine: e,
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

	// Generate ETag from path (consistent across requests)
	h := sha256.New()
	h.Write([]byte(filepath.Join(container, artifact)))
	etag := fmt.Sprintf("%x", h.Sum(nil))[:32]

	// Set S3-compliant response headers
	contentType := a.detectContentType(artifact)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
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

	// Log successful retrieval
	a.logger.Info("artifact retrieved",
		zap.String("s3.bucket", bucket),
		zap.String("s3.object", object),
		zap.String("engine.container", container),
		zap.String("engine.artifact", artifact),
		zap.Int64("bytes", written))
}

// detectContentType determines MIME type from extension
func (a *S3ToEngine) detectContentType(artifact string) string {
	ext := strings.ToLower(filepath.Ext(artifact))

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

// HandlePut processes S3 PUT requests using the engine
func (a *S3ToEngine) HandlePut(w http.ResponseWriter, r *http.Request, bucket, object string) {
	// Get tenant from context
	t, err := tenant.FromContext(r.Context())
	if err != nil {
		a.logger.Warn("no tenant in context", zap.Error(err))
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	// Translate S3 terms to Engine terms with tenant namespace
	container := t.NamespaceContainer(bucket) // Namespaced container
	artifact := object

	a.logger.Debug("PUT with tenant isolation",
		zap.String("tenant_id", t.ID),
		zap.String("original_bucket", bucket),
		zap.String("namespaced_container", container),
		zap.String("artifact", artifact))

	// Call engine with Container/Artifact terminology
	err = a.engine.Put(r.Context(), container, artifact, r.Body)
	if err != nil {
		a.logger.Error("engine put failed",
			zap.Error(err),
			zap.String("container", container),
			zap.String("artifact", artifact))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Calculate ETag using SHA256 of the path
	h := sha256.New()
	h.Write([]byte(filepath.Join(container, artifact)))
	etag := fmt.Sprintf("%x", h.Sum(nil))[:32]

	// Return S3-compliant response
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.WriteHeader(http.StatusOK)

	// Log successful upload
	a.logger.Info("artifact stored",
		zap.String("tenant_id", t.ID),
		zap.String("s3.bucket", bucket),
		zap.String("s3.object", object),
		zap.String("engine.container", container),
		zap.String("engine.artifact", artifact),
		zap.Int64("size", r.ContentLength))
}

// HandleDelete processes S3 DELETE requests
func (a *S3ToEngine) HandleDelete(w http.ResponseWriter, r *http.Request, bucket, object string) {
	// Get tenant from context
	t, err := tenant.FromContext(r.Context())
	if err != nil {
		a.logger.Warn("no tenant in context", zap.Error(err))
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	// Delete using engine with tenant namespace
	container := t.NamespaceContainer(bucket)

	if err := a.engine.Delete(r.Context(), container, object); err != nil {
		// Check if it's a not found error
		if strings.Contains(err.Error(), "no such file or directory") ||
			strings.Contains(err.Error(), "not found") {
			// For S3 compatibility, DELETE of non-existent object is still success
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

	// S3 returns 204 No Content for successful DELETE
	w.WriteHeader(http.StatusNoContent)
}

// HandleList processes S3 LIST requests
func (a *S3ToEngine) HandleList(w http.ResponseWriter, r *http.Request, bucket, prefix string) {
	// Get tenant from context
	t, err := tenant.FromContext(r.Context())
	if err != nil {
		a.logger.Warn("no tenant in context", zap.Error(err))
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	// List using engine with tenant namespace
	container := t.NamespaceContainer(bucket)

	artifacts, err := a.engine.List(r.Context(), container, prefix)
	if err != nil {
		a.logger.Error("list failed",
			zap.String("container", container),
			zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Convert to S3 XML response
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
