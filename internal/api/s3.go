// internal/api/s3.go
package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/xml"
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

// xmlEscape escapes a string for safe inclusion in XML element content.
func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

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
			if _, ok := req.Query["uploads"]; ok {
				req.Operation = "ListMultipartUploads"
			} else {
				req.Operation = "ListObjects"
			}
		case "PUT":
			req.Operation = "CreateBucket"
		case "DELETE":
			req.Operation = "DeleteBucket"
		case "HEAD":
			req.Operation = "HeadBucket"
		case "POST":
			if _, ok := req.Query["delete"]; ok {
				req.Operation = "DeleteObjects"
			} else {
				req.Operation = "Unknown"
			}
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
			// Escape the error message to prevent XML/XSS injection.
			safeMsg := xmlEscape(err.Error())
			if _, werr := fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error>
    <Code>SignatureDoesNotMatch</Code>
    <Message>%s</Message>
    <RequestId>%d</RequestId>
</Error>`, safeMsg, time.Now().UnixNano()); werr != nil {
				s.logger.Error("failed to write response", zap.Error(werr))
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

		// Phase 3.4: check if tenant is suspended before processing request.
		if s.db != nil && isTenantSuspended(r.Context(), s.db, tenantID) {
			s.logger.Warn("suspended tenant attempted S3 request",
				zap.String("tenant_id", tenantID),
				zap.String("path", r.URL.Path))
			WriteS3Error(w, ErrAccountSuspended, r.URL.Path, generateRequestID())
			return
		}
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

	// Phase 4.2: check bandwidth limit before data-transfer operations.
	if tenantID != "" && tenantID != "default" && s.bandwidthTracker != nil {
		if s3Req.Operation == "GetObject" || s3Req.Operation == "PutObject" || s3Req.Operation == "UploadPart" {
			if s.bandwidthTracker.IsOverLimit(r.Context(), tenantID) {
				s.logger.Warn("bandwidth limit exceeded",
					zap.String("tenant_id", tenantID),
					zap.String("operation", s3Req.Operation))
				WriteS3Error(w, ErrSlowDown, r.URL.Path, generateRequestID())
				return
			}
		}
	}

	// Wrap response writer to count egress bytes for bandwidth tracking.
	cw := &countingResponseWriter{ResponseWriter: w}

	// Track ingress bytes from PUT/UploadPart request bodies.
	var ingressBytes int64
	if s3Req.Operation == "PutObject" || s3Req.Operation == "UploadPart" {
		ingressBytes = r.ContentLength
		if ingressBytes < 0 {
			ingressBytes = 0
		}
	}

	switch s3Req.Operation {
	case "GetObject":
		s.handleGetObject(cw, r, s3Req)
	case "HeadObject":
		s.handleHeadObject(cw, r, s3Req)
	case "PutObject":
		if r.Header.Get("x-amz-copy-source") != "" {
			s.handleCopyObject(cw, r, s3Req)
		} else {
			s.handlePutObject(cw, r, s3Req)
		}
	case "DeleteObject":
		s.handleDeleteObject(cw, r, s3Req)
	case "DeleteObjects":
		s.handleDeleteObjects(cw, r, s3Req)
	case "ListObjects":
		s.handleListObjects(cw, r, s3Req)
	case "ListBuckets":
		s.handleListBuckets(cw, r, s3Req)
	case "CreateBucket":
		s.CreateBucket(cw, r)
	case "DeleteBucket":
		s.DeleteBucket(cw, r)
	case "InitiateMultipartUpload":
		s.handleInitiateMultipartUpload(cw, r, s3Req.Bucket, s3Req.Object)
	case "UploadPart":
		s.handleUploadPart(cw, r, s3Req.Bucket, s3Req.Object)
	case "CompleteMultipartUpload":
		s.handleCompleteMultipartUpload(cw, r, s3Req.Bucket, s3Req.Object)
	case "AbortMultipartUpload":
		s.handleAbortMultipartUpload(cw, r, s3Req.Bucket, s3Req.Object)
	case "ListParts":
		s.handleListParts(cw, r, s3Req.Bucket, s3Req.Object)
	case "ListMultipartUploads":
		s.handleListMultipartUploads(cw, r, s3Req.Bucket)
	default:
		s.logger.Warn("operation not implemented",
			zap.String("operation", s3Req.Operation))
		WriteS3Error(cw, ErrNotImplemented, r.URL.Path, generateRequestID())
	}

	// Record bandwidth for authenticated requests.
	if tenantID != "" && tenantID != "default" && s.bandwidthTracker != nil {
		s.bandwidthTracker.Record(r.Context(), tenantID, ingressBytes, cw.bytesWritten)
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
	var updatedAt time.Time

	err = s.db.QueryRowContext(r.Context(), `
		SELECT size_bytes, etag, content_type, updated_at
		FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
	`, t.ID, req.Bucket, req.Object).Scan(&sizeBytes, &etag, &contentType, &updatedAt)

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
	if !updatedAt.IsZero() {
		w.Header().Set("Last-Modified", updatedAt.UTC().Format(http.TimeFormat))
	} else {
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	}
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

// isTenantSuspended checks if a tenant has been suspended by an admin.
func isTenantSuspended(ctx context.Context, db *sql.DB, tenantID string) bool {
	var suspendedAt sql.NullTime
	err := db.QueryRowContext(ctx,
		`SELECT suspended_at FROM tenants WHERE id = $1`, tenantID).Scan(&suspendedAt)
	if err != nil {
		return false // fail open — if we can't check, allow access
	}
	return suspendedAt.Valid
}
