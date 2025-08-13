package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/events"
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

	// Determine operation
	p.determineOperation(req, r.Method)

	// Parse query parameters
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			req.Query[key] = values[0]
		}
	}

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

	// Object-level operations
	switch method {
	case "GET":
		req.Operation = "GetObject"
	case "PUT":
		req.Operation = "PutObject"
	case "DELETE":
		req.Operation = "DeleteObject"
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

	// Validate the request signature FIRST
	auth := NewAuth(s.db, s.logger) // Create auth instance
	tenantID, err := auth.ValidateRequest(r)
	if err != nil {
		s.logger.Error("authentication failed",
			zap.Error(err),
			zap.String("path", r.URL.Path))

		// Return S3-style authentication error
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error>
	<Code>SignatureDoesNotMatch</Code>
	<Message>%s</Message>
	<RequestId>%d</RequestId>
</Error>`, err.Error(), time.Now().UnixNano())
		return
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

	s3Req.TenantID = tenantID

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

	// For now, return not implemented for actual S3 operations
	if s3Req.Operation != "" && s3Req.Operation != "Unknown" {
		WriteS3Error(w, ErrNotImplemented, r.URL.Path, generateRequestID())
		return
	}

	// If no specific operation, return method not allowed
	WriteS3Error(w, ErrMethodNotAllowed, r.URL.Path, generateRequestID())
}
