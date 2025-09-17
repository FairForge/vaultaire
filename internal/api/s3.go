// internal/api/s3.go
package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

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

	// Parse the path
	p.parsePath(req)

	// Parse query parameters BEFORE determining operation
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			req.Query[key] = values[0]
		}
	}

	// Determine operation (needs query params for multipart detection)
	p.determineOperation(req, r.Method)

	// Parse relevant headers
	p.parseHeaders(req, r)

	// Log the parsed request
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
	// Remove leading slash
	path := strings.TrimPrefix(req.Path, "/")

	// If empty, it's a list buckets request
	if path == "" {
		return
	}

	// Split by first slash
	parts := strings.SplitN(path, "/", 2)

	// First part is always the bucket
	req.Bucket = parts[0]

	// If there's a second part, it's the object key
	if len(parts) > 1 && parts[1] != "" {
		req.Object = parts[1]
	}
}

// determineOperation determines the S3 operation from method and path
func (p *S3Parser) determineOperation(req *S3Request, method string) {
	// Root path operations
	if req.Bucket == "" {
		switch method {
		case "GET":
			req.Operation = "ListBuckets"
		default:
			req.Operation = "Unknown"
		}
		return
	}

	// Bucket-level operations
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

	// Object-level operations with multipart detection
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
		// Check if this is an abort multipart upload
		if _, ok := req.Query["uploadId"]; ok {
			req.Operation = "AbortMultipartUpload"
		} else {
			req.Operation = "DeleteObject"
		}
	case "HEAD":
		req.Operation = "HeadObject"
	case "POST":
		// Check for multipart upload
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
	// Common S3 headers
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

	// Also capture all x-amz-* headers
	for key, values := range r.Header {
		if strings.HasPrefix(strings.ToLower(key), "x-amz-") && len(values) > 0 {
			req.Headers[key] = values[0]
		}
	}
}

// handleS3Request handles S3-compatible API requests
func (s *Server) handleS3Request(w http.ResponseWriter, r *http.Request) {
	// Skip health check endpoints
	if r.URL.Path == "/health" || r.URL.Path == "/ready" ||
		r.URL.Path == "/metrics" || r.URL.Path == "/version" {
		return
	}

	var tenantID string
	var err error

	// Check if we're in test mode
	if !s.testMode {
		// Production mode: validate the request signature
		auth := NewAuth(s.db, s.logger)
		tenantID, err = auth.ValidateRequest(r)
		if err != nil {
			s.logger.Error("authentication failed",
				zap.Error(err),
				zap.String("path", r.URL.Path))

			// Return S3-style authentication error
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			if _, err := fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error>
    <Code>SignatureDoesNotMatch</Code>
    <Message>%s</Message>
    <RequestId>%d</RequestId>
</Error>`, err.Error(), time.Now().UnixNano()); err != nil {
				s.logger.Error("failed to write response", zap.Error(err))
			}
			return
		}
	} else {
		// Test mode: check if tenant is already in context
		if t, err := tenant.FromContext(r.Context()); err == nil && t != nil {
			tenantID = t.ID
		} else {
			// Default to "test" tenant for tests
			tenantID = "test"
		}
		s.logger.Debug("test mode - skipping auth",
			zap.String("tenant_id", tenantID),
			zap.String("path", r.URL.Path))
	}

	// Log authentication status
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

	// Create parser
	parser := NewS3Parser(s.logger)

	// Parse the request
	s3Req, err := parser.ParseRequest(r)
	if err != nil {
		s.logger.Error("Failed to parse S3 request", zap.Error(err))
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	// Put the tenant ID into the context immediately
	if tenantID != "" {
		ctx := context.WithValue(r.Context(), common.TenantIDKey, tenantID)
		r = r.WithContext(ctx)
	}

	s3Req.TenantID = tenantID

	// Always create a tenant - use "default" for anonymous
	if tenantID == "" {
		tenantID = "default"
	}

	// Use existing tenant from context in test mode, or create new one
	var t *tenant.Tenant
	if s.testMode {
		// Try to get tenant from context first
		if existingTenant, err := tenant.FromContext(r.Context()); err == nil && existingTenant != nil {
			t = existingTenant
		} else {
			// Create a test tenant
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
		// Production mode: create tenant normally
		t = &tenant.Tenant{
			ID:                tenantID,
			Namespace:         fmt.Sprintf("tenant/%s/", tenantID),
			Plan:              "starter",
			Status:            "active",
			StorageQuota:      100 * 1024 * 1024 * 1024,
			RequestsPerSecond: 100,
		}
	}

	// Get the existing tenant ID from the context FIRST
	existingTenantID := ""
	if t := r.Context().Value(common.TenantIDKey); t != nil {
		if tid, ok := t.(string); ok {
			existingTenantID = tid
		}
	}

	// Add tenant to request context if not already there
	ctx := tenant.WithTenant(r.Context(), t)

	// Preserve the common.TenantIDKey if it existed
	if existingTenantID != "" {
		ctx = context.WithValue(ctx, common.TenantIDKey, existingTenantID)
	}
	r = r.WithContext(ctx)

	// Log event for ML training data collection
	eventLogger := events.NewEventLogger(s.logger)
	eventLogger.Log(events.Event{
		Type:      "s3_request",
		Container: s3Req.Bucket, // External: bucket, Internal: container
		Artifact:  s3Req.Object, // External: object, Internal: artifact
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

	// Route based on operation
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
	// Use the adapter to translate S3 → Engine
	adapter := NewS3ToEngine(s.engine, s.logger)

	// Log dual terminology for debugging
	s.logger.Debug("S3 GET translating to engine",
		zap.String("s3.bucket", req.Bucket),
		zap.String("s3.object", req.Object),
		zap.String("engine.container", req.Bucket), // Maps to container
		zap.String("engine.artifact", req.Object),  // Maps to artifact
	)

	// Call the adapter's HandleGet
	adapter.HandleGet(w, r, req.Bucket, req.Object)
}

// handleHeadObject handles HEAD requests
func (s *Server) handleHeadObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	// Log dual terminology for debugging
	s.logger.Debug("S3 HEAD translating to engine",
		zap.String("s3.bucket", req.Bucket),
		zap.String("s3.object", req.Object),
		zap.String("engine.container", req.Bucket),
		zap.String("engine.artifact", req.Object),
	)

	// Get the tenant from context
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	// Build the container name with tenant prefix
	containerName := fmt.Sprintf("%s_%s", t.ID, req.Bucket)

	// Try to get the artifact to check if it exists
	reader, err := s.engine.Get(r.Context(), containerName, req.Object)
	if err != nil {
		s.logger.Debug("HeadObject failed",
			zap.String("bucket", req.Bucket),
			zap.String("object", req.Object),
			zap.Error(err))
		WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
		return
	}

	// Read the data to get the size
	var size int64 = 0
	if reader != nil {
		// For HEAD, we need to get the size somehow
		// Read all data to count bytes (not efficient but works for MVP)
		buf := make([]byte, 8192)
		for {
			n, err := reader.Read(buf)
			size += int64(n)
			if err != nil {
				break
			}
		}
		_ = reader.Close() // Error intentionally ignored
	}

	// Set the required headers
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", "\"d41d8cd98f00b204e9800998ecf8427e\"") // Mock ETag for now
	w.Header().Set("Last-Modified", time.Now().UTC().Format(time.RFC1123))
	w.Header().Set("x-amz-storage-class", "STANDARD")

	// HEAD requests don't have a body, just headers
	w.WriteHeader(http.StatusOK)
}

// handlePutObject handles PUT requests
func (s *Server) handlePutObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	// Use the adapter to translate S3 → Engine
	adapter := NewS3ToEngine(s.engine, s.logger)

	// Log dual terminology
	s.logger.Debug("S3 PUT translating to engine",
		zap.String("s3.bucket", req.Bucket),
		zap.String("s3.object", req.Object),
		zap.String("engine.container", req.Bucket),
		zap.String("engine.artifact", req.Object),
		zap.Int64("size", r.ContentLength))

	// Call the adapter's HandlePut
	adapter.HandlePut(w, r, req.Bucket, req.Object)
}

// handleDeleteObject handles DELETE requests
func (s *Server) handleDeleteObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	// Use the adapter for tenant isolation (like PUT and GET do)
	adapter := NewS3ToEngine(s.engine, s.logger)
	adapter.HandleDelete(w, r, req.Bucket, req.Object)
}

// handleListObjects handles bucket listing
func (s *Server) handleListObjects(w http.ResponseWriter, r *http.Request, req *S3Request) {
	// Use the adapter for tenant isolation (like PUT/GET/DELETE do)
	adapter := NewS3ToEngine(s.engine, s.logger)
	adapter.HandleList(w, r, req.Bucket, "") // Empty prefix for now
}

// handleListBuckets handles listing all buckets
func (s *Server) handleListBuckets(w http.ResponseWriter, r *http.Request, req *S3Request) {
	s.ListBuckets(w, r)
}
