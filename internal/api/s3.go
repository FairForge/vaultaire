package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// S3Request represents a parsed S3 API request
type S3Request struct {
	Bucket    string
	Object    string
	Operation string
	Query     map[string]string
	Headers   map[string]string

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

	// Create parser
	parser := NewS3Parser(s.logger)

	// Parse the request
	s3Req, err := parser.ParseRequest(r)
	if err != nil {
		s.logger.Error("Failed to parse S3 request", zap.Error(err))
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// For now, return the parsed request as JSON for testing
	response := map[string]interface{}{
		"bucket":    s3Req.Bucket,
		"object":    s3Req.Object,
		"operation": s3Req.Operation,
		"method":    s3Req.Method,
		"path":      s3Req.Path,
		"query":     s3Req.Query,
		"headers":   s3Req.Headers,
		"timestamp": s3Req.Timestamp,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}
