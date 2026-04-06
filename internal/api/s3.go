// internal/api/s3.go
package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/events"
	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

// S3Request represents a parsed S3 API request
type S3Request struct {
	Bucket    string
	Object    string
	Operation string
	Query     map[string]string
	Headers   map[string]string
	TenantID  string

	// Request metadata
	Method    string
	Path      string
	Timestamp time.Time
}

// S3Parser parses S3-compatible API requests
type S3Parser struct {
	logger *zap.Logger
}

// NewS3Parser creates a new S3 request parser
func NewS3Parser(logger *zap.Logger) *S3Parser {
	return &S3Parser{
		logger: logger,
	}
}

// ParseRequest parses an HTTP request into S3Request
func (p *S3Parser) ParseRequest(r *http.Request) (*S3Request, error) {
	req := &S3Request{
		Method:    r.Method,
		Path:      r.URL.Path,
		Timestamp: time.Now(),
		Query:     make(map[string]string),
		Headers:   make(map[string]string),
	}

	p.parsePath(req)

	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			req.Query[key] = values[0]
		}
	}

	p.determineOperation(req, r.Method)
	p.parseHeaders(req, r)

	p.logger.Info("Parsed S3 request",
		zap.String("bucket", req.Bucket),
		zap.String("object", req.Object),
		zap.String("operation", req.Operation),
		zap.String("method", req.Method),
		zap.String("path", req.Path),
	)

	return req, nil
}

// parsePath extracts bucket and object from URL path
func (p *S3Parser) parsePath(req *S3Request) {
	path := strings.TrimPrefix(req.Path, "/")
	if path == "" {
		return
	}

	parts := strings.SplitN(path, "/", 2)
	req.Bucket = parts[0]

	if len(parts) > 1 && parts[1] != "" {
		req.Object = parts[1]
	}
}

// determineOperation determines the S3 operation from method and path
func (p *S3Parser) determineOperation(req *S3Request, method string) {
	if req.Bucket == "" {
		switch method {
		case "GET":
			req.Operation = "ListBuckets"
		default:
			req.Operation = "Unknown"
		}
		return
	}

	if req.Object == "" {
		switch method {
		case "GET":
			req.Operation = "ListObjects"
		case "PUT":
			req.Operation = "CreateBucket"
		case "DELETE":
			req.Operation = "DeleteBucket"
		case "HEAD":
			req.Operation = "HeadBucket"
		default:
			req.Operation = "Unknown"
		}
		return
	}

	switch method {
	case "GET":
		if _, ok := req.Query["uploadId"]; ok {
			req.Operation = "ListParts"
		} else {
			req.Operation = "GetObject"
		}
	case "PUT":
		if _, ok := req.Query["partNumber"]; ok {
			req.Operation = "UploadPart"
		} else {
			req.Operation = "PutObject"
		}
	case "DELETE":
		if _, ok := req.Query["uploadId"]; ok {
			req.Operation = "AbortMultipartUpload"
		} else {
			req.Operation = "DeleteObject"
		}
	case "HEAD":
		req.Operation = "HeadObject"
	case "POST":
		if _, ok := req.Query["uploads"]; ok {
			req.Operation = "InitiateMultipartUpload"
		} else if _, ok := req.Query["uploadId"]; ok {
			req.Operation = "CompleteMultipartUpload"
		} else {
			req.Operation = "PostObject"
		}
	default:
		req.Operation = "Unknown"
	}
}

// parseHeaders extracts relevant S3 headers
func (p *S3Parser) parseHeaders(req *S3Request, r *http.Request) {
	headersToParse := []string{
		"Content-Type",
		"Content-Length",
		"Content-MD5",
		"x-amz-content-sha256",
		"x-amz-date",
		"x-amz-storage-class",
		"x-amz-acl",
		"Authorization",
		"Range",
	}

	for _, header := range headersToParse {
		if value := r.Header.Get(header); value != "" {
			req.Headers[header] = value
		}
	}

	for key, values := range r.Header {
		if strings.HasPrefix(strings.ToLower(key), "x-amz-") && len(values) > 0 {
			req.Headers[key] = values[0]
		}
	}
}

// handleS3Request handles S3-compatible API requests
func (s *Server) handleS3Request(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" || r.URL.Path == "/ready" ||
		r.URL.Path == "/metrics" || r.URL.Path == "/version" {
		return
	}

	var tenantID string
	var err error

	if !s.testMode {
		auth := auth.NewAuth(s.db, s.logger)
		tenantID, err = auth.ValidateRequest(r)
		if err != nil {
			s.logger.Error("authentication failed",
				zap.Error(err),
				zap.String("path", r.URL.Path))

			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			if _, err := fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error>
    <Code>SignatureDoesNotMatch</Code>
    <Message>%s</Message>
    <RequestId>%d</RequestId>
</Error>`, err.Error(), time.Now().UnixNano()); err != nil { // #nosec G705 — S3 XML protocol output
				s.logger.Error("failed to write response", zap.Error(err))
			}
			return
		}
	} else {
		if t, err := tenant.FromContext(r.Context()); err == nil && t != nil {
			tenantID = t.ID
		} else {
			tenantID = "test"
		}
		s.logger.Debug("test mode - skipping auth",
			zap.String("tenant_id", tenantID),
			zap.String("path", r.URL.Path))
	}

	if tenantID != "" {
		s.logger.Info("authenticated request",
			zap.String("tenant_id", tenantID),
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path))
	} else {
		s.logger.Debug("anonymous request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path))
	}

	parser := NewS3Parser(s.logger)

	s3Req, err := parser.ParseRequest(r)
	if err != nil {
		s.logger.Error("Failed to parse S3 request", zap.Error(err))
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	if tenantID != "" {
		ctx := context.WithValue(r.Context(), common.TenantIDKey, tenantID)
		r = r.WithContext(ctx)
	}

	s3Req.TenantID = tenantID

	if tenantID == "" {
		tenantID = "default"
	}

	var t *tenant.Tenant
	if s.testMode {
		if existingTenant, err := tenant.FromContext(r.Context()); err == nil && existingTenant != nil {
			t = existingTenant
		} else {
			t = &tenant.Tenant{
				ID:                tenantID,
				Namespace:         fmt.Sprintf("tenant/%s/", tenantID),
				Plan:              "starter",
				Status:            "active",
				StorageQuota:      100 * 1024 * 1024 * 1024,
				RequestsPerSecond: 100,
			}
		}
	} else {
		t = &tenant.Tenant{
			ID:                tenantID,
			Namespace:         fmt.Sprintf("tenant/%s/", tenantID),
			Plan:              "starter",
			Status:            "active",
			StorageQuota:      100 * 1024 * 1024 * 1024,
			RequestsPerSecond: 100,
		}
	}

	existingTenantID := ""
	if tv := r.Context().Value(common.TenantIDKey); tv != nil {
		if tid, ok := tv.(string); ok {
			existingTenantID = tid
		}
	}

	ctx := tenant.WithTenant(r.Context(), t)
	if existingTenantID != "" {
		ctx = context.WithValue(ctx, common.TenantIDKey, existingTenantID)
	}
	r = r.WithContext(ctx)

	eventLogger := events.NewEventLogger(s.logger)
	eventLogger.Log(events.Event{
		Type:      "s3_request",
		Container: s3Req.Bucket,
		Artifact:  s3Req.Object,
		Operation: s3Req.Operation,
		TenantID:  tenantID,
		Data: map[string]interface{}{
			"method":    r.Method,
			"path":      r.URL.Path,
			"size":      r.ContentLength,
			"query":     len(s3Req.Query),
			"headers":   len(s3Req.Headers),
			"tenant_id": tenantID,
		},
	})

	switch s3Req.Operation {
	case "GetObject":
		s.handleGetObject(w, r, s3Req)
	case "HeadObject":
		s.handleHeadObject(w, r, s3Req)
	case "PutObject":
		s.handlePutObject(w, r, s3Req)
	case "DeleteObject":
		s.handleDeleteObject(w, r, s3Req)
	case "ListObjects":
		s.handleListObjects(w, r, s3Req)
	case "ListBuckets":
		s.handleListBuckets(w, r, s3Req)
	case "CreateBucket":
		s.CreateBucket(w, r)
	case "DeleteBucket":
		s.DeleteBucket(w, r)
	case "InitiateMultipartUpload":
		s.handleInitiateMultipartUpload(w, r, s3Req.Bucket, s3Req.Object)
	case "UploadPart":
		s.handleUploadPart(w, r, s3Req.Bucket, s3Req.Object)
	case "CompleteMultipartUpload":
		s.handleCompleteMultipartUpload(w, r, s3Req.Bucket, s3Req.Object)
	case "AbortMultipartUpload":
		s.handleAbortMultipartUpload(w, r, s3Req.Bucket, s3Req.Object)
	case "ListParts":
		s.handleListParts(w, r, s3Req.Bucket, s3Req.Object)
	default:
		s.logger.Warn("operation not implemented",
			zap.String("operation", s3Req.Operation))
		WriteS3Error(w, ErrNotImplemented, r.URL.Path, generateRequestID())
	}
}

// handleGetObject handles S3 GET requests
func (s *Server) handleGetObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	adapter := NewS3ToEngine(s.engine, s.db, s.logger)

	s.logger.Debug("S3 GET translating to engine",
		zap.String("s3.bucket", req.Bucket),
		zap.String("s3.object", req.Object),
		zap.String("engine.container", req.Bucket),
		zap.String("engine.artifact", req.Object),
	)

	adapter.HandleGet(w, r, req.Bucket, req.Object)
}

// handleHeadObject handles HEAD requests by querying PostgreSQL metadata.
//
// Previously this called engine.Get() and read every byte just to count
// them — HEAD latency scaled with file size (500MB = 62s) and AWS CLI
// downloads broke entirely for large files because CLI does HEAD before GET.
//
// Now it queries object_head_cache which is written on every successful PUT.
// HEAD latency is now ~1ms regardless of object size.
func (s *Server) handleHeadObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	var sizeBytes int64
	var etag, contentType string

	err = s.db.QueryRowContext(r.Context(), `
		SELECT size_bytes, etag, content_type
		FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
	`, t.ID, req.Bucket, req.Object).Scan(&sizeBytes, &etag, &contentType)

	if err == sql.ErrNoRows {
		s.logger.Warn("HEAD: object not in metadata cache",
			zap.String("tenant_id", t.ID),
			zap.String("bucket", req.Bucket),
			zap.String("object", req.Object))
		WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
		return
	}
	if err != nil {
		s.logger.Error("HEAD: metadata query failed", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Only emit Content-Length when we have a valid, known size.
	// A value of 0 means the client uploaded without a Content-Length header
	// (chunked transfer encoding); we stored 0 as a safe sentinel.
	// Emitting "Content-Length: -1" is invalid HTTP and causes HAProxy to 502.
	if sizeBytes >= 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", sizeBytes))
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("Last-Modified", time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	w.Header().Set("x-amz-storage-class", "STANDARD")
	w.Header().Set("x-amz-request-id", generateRequestID())
	// HEAD must not write a body.
	w.WriteHeader(http.StatusOK)
}

// handlePutObject handles PUT requests
func (s *Server) handlePutObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	adapter := NewS3ToEngine(s.engine, s.db, s.logger)

	s.logger.Debug("S3 PUT translating to engine",
		zap.String("s3.bucket", req.Bucket),
		zap.String("s3.object", req.Object),
		zap.String("engine.container", req.Bucket),
		zap.String("engine.artifact", req.Object),
		zap.Int64("size", r.ContentLength))

	adapter.HandlePut(w, r, req.Bucket, req.Object)
}

// handleDeleteObject handles DELETE requests
func (s *Server) handleDeleteObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	adapter := NewS3ToEngine(s.engine, s.db, s.logger)
	adapter.HandleDelete(w, r, req.Bucket, req.Object)
}

// handleListObjects handles bucket listing
func (s *Server) handleListObjects(w http.ResponseWriter, r *http.Request, req *S3Request) {
	adapter := NewS3ToEngine(s.engine, s.db, s.logger)
	adapter.HandleList(w, r, req.Bucket, "")
}

// handleListBuckets handles listing all buckets
func (s *Server) handleListBuckets(w http.ResponseWriter, r *http.Request, req *S3Request) {
	s.ListBuckets(w, r)
}
