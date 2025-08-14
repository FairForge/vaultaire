package api

import (
	"fmt"
	"net/http"
	"os"
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

// handleHeadObject handles HEAD requests (not implemented yet)
func (s *Server) handleHeadObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	WriteS3Error(w, ErrNotImplemented, r.URL.Path, generateRequestID())
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
	// Log dual terminology
	s.logger.Debug("S3 DELETE translating to engine",
		zap.String("s3.bucket", req.Bucket),
		zap.String("s3.object", req.Object),
		zap.String("engine.container", req.Bucket),
		zap.String("engine.artifact", req.Object),
	)

	// Delete using the engine (with context)
	ctx := r.Context()
	err := s.engine.Delete(ctx, req.Bucket, req.Object)
	if err != nil {
		// Check if file doesn't exist
		if os.IsNotExist(err) {
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
			return
		}
		// Other errors
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// S3 DELETE returns 204 No Content on success
	w.WriteHeader(http.StatusNoContent)
}

// handleListObjects handles bucket listing
func (s *Server) handleListObjects(w http.ResponseWriter, r *http.Request, req *S3Request) {
	// Log the operation
	s.logger.Debug("S3 LIST translating to engine",
		zap.String("s3.bucket", req.Bucket),
		zap.String("engine.container", req.Bucket),
	)

	// List objects from the engine (no prefix parameter needed)
	ctx := r.Context()
	objects, err := s.engine.List(ctx, req.Bucket)
	if err != nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Build S3 XML response
	type Object struct {
		Key          string
		LastModified string
		Size         int64
		StorageClass string
	}

	type ListBucketResult struct {
		Name        string
		Prefix      string
		MaxKeys     int
		IsTruncated bool
		Contents    []Object
	}

	result := ListBucketResult{
		Name:        req.Bucket,
		Prefix:      req.Query["prefix"],
		MaxKeys:     1000,
		IsTruncated: false,
		Contents:    make([]Object, 0),
	}

	// Convert engine objects to S3 format
	for _, obj := range objects {
		result.Contents = append(result.Contents, Object{
			Key:          obj.Key,
			LastModified: obj.Modified.Format(time.RFC3339), // Changed from LastModified to Modified
			Size:         obj.Size,
			StorageClass: "STANDARD",
		})
	}

	// Marshal to XML
	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>`))
	w.Write([]byte(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`))
	w.Write([]byte(fmt.Sprintf("<Name>%s</Name>", result.Name)))
	w.Write([]byte(fmt.Sprintf("<Prefix>%s</Prefix>", result.Prefix)))
	w.Write([]byte(fmt.Sprintf("<MaxKeys>%d</MaxKeys>", result.MaxKeys)))
	w.Write([]byte(fmt.Sprintf("<IsTruncated>%t</IsTruncated>", result.IsTruncated)))

	for _, obj := range result.Contents {
		w.Write([]byte("<Contents>"))
		w.Write([]byte(fmt.Sprintf("<Key>%s</Key>", obj.Key)))
		w.Write([]byte(fmt.Sprintf("<LastModified>%s</LastModified>", obj.LastModified)))
		w.Write([]byte(fmt.Sprintf("<Size>%d</Size>", obj.Size)))
		w.Write([]byte(fmt.Sprintf("<StorageClass>%s</StorageClass>", obj.StorageClass)))
		w.Write([]byte("</Contents>"))
	}

	w.Write([]byte("</ListBucketResult>"))
}

// handleListBuckets handles listing all buckets (not implemented yet)
func (s *Server) handleListBuckets(w http.ResponseWriter, r *http.Request, req *S3Request) {
	WriteS3Error(w, ErrNotImplemented, r.URL.Path, generateRequestID())
}
