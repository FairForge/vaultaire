package api

import (
	"context"
	"crypto/md5"

	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
		if strings.Contains(err.Error(), "no such file or directory") {
			// Check if it's the container or artifact that's missing
			containerPath := filepath.Join("/tmp/vaultaire", container)
			if _, err := os.Stat(containerPath); os.IsNotExist(err) {
				WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, generateRequestID())
			} else {
				WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
			}
		} else {
			a.logger.Error("engine get failed",
				zap.Error(err),
				zap.String("container", container),
				zap.String("artifact", artifact))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		}
		return
	}
	defer reader.Close()

	// Get file info for headers
	filePath := filepath.Join("/tmp/vaultaire", container, artifact)
	var fileInfo os.FileInfo
	var etag string

	if info, err := os.Stat(filePath); err == nil {
		fileInfo = info
		// Calculate simple ETag
		h := md5.New()
		h.Write([]byte(fmt.Sprintf("%s-%d", artifact, info.Size())))
		etag = fmt.Sprintf("%x", h.Sum(nil))
	} else {
		// Fallback if we can't stat
		fileInfo = nil
		etag = "unknown"
	}

	// Set S3-compliant response headers
	contentType := a.detectContentType(artifact)
	w.Header().Set("Content-Type", contentType)

	if fileInfo != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(fileInfo.Size(), 10))
		w.Header().Set("Last-Modified", fileInfo.ModTime().UTC().Format(http.TimeFormat))
	}

	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.Header().Set("x-amz-version-id", "null")
	w.Header().Set("Accept-Ranges", "bytes")

	// Handle range requests if present
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		a.handleRangeRequest(w, r, reader, fileInfo, rangeHeader)
		return
	}

	// Stream the content
	written, err := io.Copy(w, reader)
	if err != nil {
		a.logger.Error("failed to stream artifact",
			zap.Error(err),
			zap.String("container", container),
			zap.String("artifact", artifact))
		return
	}

	// Log successful retrieval for ML
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

// handleRangeRequest handles partial content requests
func (a *S3ToEngine) handleRangeRequest(w http.ResponseWriter, r *http.Request,
	reader io.ReadCloser, fileInfo os.FileInfo, rangeHeader string) {

	// Parse range header
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		http.Error(w, "Invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	rangeSpec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.Split(rangeSpec, "-")

	var start, end int64
	var err error

	if parts[0] != "" {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			http.Error(w, "Invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}

	size := int64(-1)
	if fileInfo != nil {
		size = fileInfo.Size()
	}

	if len(parts) > 1 && parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			http.Error(w, "Invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	} else if size > 0 {
		end = size - 1
	} else {
		http.Error(w, "Cannot determine range without size", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// Validate range
	if start < 0 || (size > 0 && end >= size) || start > end {
		http.Error(w, "Range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// Try to seek if the reader supports it
	if seeker, ok := reader.(io.ReadSeeker); ok {
		if _, err := seeker.Seek(start, io.SeekStart); err != nil {
			a.logger.Error("failed to seek", zap.Error(err))
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
	} else {
		// If can't seek, skip bytes
		if _, err := io.CopyN(io.Discard, reader, start); err != nil {
			a.logger.Error("failed to skip to range start", zap.Error(err))
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
	}

	// Set partial content headers
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)

	// Copy the requested range
	_, err = io.CopyN(w, reader, end-start+1)
	if err != nil && err != io.EOF {
		a.logger.Error("failed to copy range", zap.Error(err))
	}
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

	// Calculate ETag (simple MD5 of the path for now)
	h := md5.New()
	h.Write([]byte(fmt.Sprintf("%s/%s", container, artifact)))
	etag := fmt.Sprintf("%x", h.Sum(nil))

	// Return S3-compliant response
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.WriteHeader(http.StatusOK)

	// Log successful upload for ML
	a.logger.Info("artifact stored",
		zap.String("tenant_id", t.ID),
		zap.String("s3.bucket", bucket),
		zap.String("s3.object", object),
		zap.String("engine.container", container),
		zap.String("engine.artifact", artifact),
		zap.Int64("size", r.ContentLength))
}
