// internal/api/s3.go
package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/clientip"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/usage"
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

// opUnsupportedSubresource is the operation of a request that names an S3
// sub-resource this server has no handler for (for that method). It has no
// case in handleS3Request, so it is answered 501 NotImplemented.
const opUnsupportedSubresource = "UnsupportedSubresource"

// The S3 sub-resource names, implemented or not. A request that names one
// and matched no handler must not fall through to the method's plain
// operation: before WP-R4-7 `DELETE /{bucket}?lifecycle` was DeleteBucket (an
// empty bucket was deleted), `PUT /{bucket}?policy` was CreateBucket (200 —
// the client believed a policy applied) and `GET /{bucket}?lifecycle` was a
// listing. A1 closed this for ?acl only. partNumber is deliberately absent
// from the object list: it is a parameter of plain GetObject/HeadObject.
var (
	bucketSubresources = []string{
		"accelerate", "acl", "analytics", "cors", "delete", "encryption",
		"intelligent-tiering", "inventory", "lifecycle", "location", "logging",
		"metadataConfiguration", "metadataTable", "metrics", "notification",
		"object-lock", "ownershipControls", "policy", "policyStatus",
		"publicAccessBlock", "replication", "requestPayment", "session",
		"tagging", "uploads", "versioning", "versions", "website",
	}
	objectSubresources = []string{
		"acl", "attributes", "legal-hold", "renameObject", "restore",
		"retention", "select", "tagging", "torrent", "uploadId", "uploads",
	}
)

// namesSubresource reports whether the query carries one of the names.
func namesSubresource(query map[string]string, names []string) bool {
	for _, n := range names {
		if _, ok := query[n]; ok {
			return true
		}
	}
	return false
}

// determineOperation determines the S3 operation from method and path
func (p *S3Parser) determineOperation(req *S3Request, method string) {
	if req.Bucket == "" {
		switch method {
		case "GET":
			req.Operation = auth.OpListBuckets
		default:
			req.Operation = "Unknown"
		}
		return
	}

	if req.Object == "" {
		switch method {
		case "GET":
			if _, ok := req.Query["versioning"]; ok {
				req.Operation = auth.OpGetBucketVersioning
			} else if _, ok := req.Query["location"]; ok {
				req.Operation = auth.OpGetBucketLocation
			} else if _, ok := req.Query["notification"]; ok {
				req.Operation = auth.OpGetBucketNotification
			} else if _, ok := req.Query["object-lock"]; ok {
				req.Operation = auth.OpGetObjectLockConfiguration
			} else if _, ok := req.Query["logging"]; ok {
				req.Operation = auth.OpGetBucketLogging
			} else if _, ok := req.Query["inventory"]; ok {
				req.Operation = auth.OpGetBucketInventory
			} else if _, ok := req.Query["uploads"]; ok {
				req.Operation = auth.OpListMultipartUploads
			} else if _, ok := req.Query["versions"]; ok {
				req.Operation = auth.OpListObjectVersions
			} else if _, ok := req.Query["acl"]; ok {
				req.Operation = auth.OpGetBucketAcl
			} else {
				req.Operation = auth.OpListObjects
			}
		case "PUT":
			if _, ok := req.Query["versioning"]; ok {
				req.Operation = auth.OpPutBucketVersioning
			} else if _, ok := req.Query["notification"]; ok {
				req.Operation = auth.OpPutBucketNotification
			} else if _, ok := req.Query["object-lock"]; ok {
				req.Operation = auth.OpPutObjectLockConfiguration
			} else if _, ok := req.Query["logging"]; ok {
				req.Operation = auth.OpPutBucketLogging
			} else if _, ok := req.Query["inventory"]; ok {
				req.Operation = auth.OpPutBucketInventory
			} else if _, ok := req.Query["acl"]; ok {
				req.Operation = auth.OpPutBucketAcl
			} else {
				req.Operation = auth.OpCreateBucket
			}
		case "DELETE":
			if _, ok := req.Query["inventory"]; ok {
				req.Operation = auth.OpDeleteBucketInventory
			} else {
				req.Operation = auth.OpDeleteBucket
			}
		case "HEAD":
			req.Operation = auth.OpHeadBucket
		case "POST":
			if _, ok := req.Query["delete"]; ok {
				req.Operation = auth.OpDeleteObjects
			} else {
				req.Operation = "Unknown"
			}
		default:
			req.Operation = "Unknown"
		}
		switch req.Operation {
		case auth.OpListObjects, auth.OpCreateBucket, auth.OpDeleteBucket:
			if namesSubresource(req.Query, bucketSubresources) {
				req.Operation = opUnsupportedSubresource
			}
		}
		return
	}

	switch method {
	case "GET":
		if _, ok := req.Query["retention"]; ok {
			req.Operation = auth.OpGetObjectRetention
		} else if _, ok := req.Query["legal-hold"]; ok {
			req.Operation = auth.OpGetObjectLegalHold
		} else if _, ok := req.Query["uploadId"]; ok {
			req.Operation = auth.OpListParts
		} else if _, ok := req.Query["tagging"]; ok {
			req.Operation = auth.OpGetObjectTagging
		} else if _, ok := req.Query["acl"]; ok {
			req.Operation = auth.OpGetObjectAcl
		} else {
			req.Operation = auth.OpGetObject
		}
	case "PUT":
		if _, ok := req.Query["retention"]; ok {
			req.Operation = auth.OpPutObjectRetention
		} else if _, ok := req.Query["legal-hold"]; ok {
			req.Operation = auth.OpPutObjectLegalHold
		} else if _, ok := req.Query["partNumber"]; ok {
			req.Operation = auth.OpUploadPart
		} else if _, ok := req.Query["tagging"]; ok {
			req.Operation = auth.OpPutObjectTagging
		} else if _, ok := req.Query["acl"]; ok {
			// Before A1 (2026-09-18) this fell through to PutObject and
			// overwrote the object's bytes with the ACL XML body.
			req.Operation = auth.OpPutObjectAcl
		} else {
			req.Operation = auth.OpPutObject
		}
	case "DELETE":
		if _, ok := req.Query["uploadId"]; ok {
			req.Operation = auth.OpAbortMultipartUpload
		} else if _, ok := req.Query["tagging"]; ok {
			req.Operation = auth.OpDeleteObjectTagging
		} else {
			req.Operation = auth.OpDeleteObject
		}
	case "HEAD":
		req.Operation = auth.OpHeadObject
	case "POST":
		if _, ok := req.Query["uploads"]; ok {
			req.Operation = auth.OpInitiateMultipartUpload
		} else if _, ok := req.Query["uploadId"]; ok {
			req.Operation = auth.OpCompleteMultipartUpload
		} else if _, ok := req.Query["restore"]; ok {
			req.Operation = auth.OpRestoreObject
		} else {
			req.Operation = auth.OpPostObject
		}
	default:
		req.Operation = "Unknown"
	}
	switch req.Operation {
	case auth.OpGetObject, auth.OpPutObject, auth.OpDeleteObject, auth.OpPostObject:
		if namesSubresource(req.Query, objectSubresources) {
			req.Operation = opUnsupportedSubresource
		}
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
	var scope *auth.KeyScope
	var err error

	if !s.testMode {
		if isPresignedRequest(r) {
			tenantID, scope, err = s.verifyPresignedURL(r)
			if err != nil {
				s.logger.Error("presigned URL verification failed",
					zap.Error(err),
					zap.String("path", r.URL.Path))

				errCode := err.Error()
				reqID := generateRequestID()
				switch errCode {
				case ErrExpiredPresignedRequest, ErrSignatureDoesNotMatch,
					ErrAccessDenied, ErrAuthorizationQueryParametersError,
					ErrInvalidPresignExpires, ErrRequestTimeTooSkewed, ErrInvalidAccessKeyId:
					WriteS3Error(w, errCode, r.URL.Path, reqID)
				default:
					WriteS3Error(w, ErrAccessDenied, r.URL.Path, reqID)
				}
				reason, known := presignFailureReason(err)
				if known {
					// The verifier may have failed before looking the id up.
					known = s.accessKeyExists(r.Context(), auth.AccessKeyFromRequest(r))
				}
				recordAuthFailure(r, reason, known)
				return
			}
		} else {
			a := auth.NewAuth(s.db, s.logger)
			a.MissingSignedHeader = s.missingSignedHeaderHint
			tenantID, scope, err = a.ValidateRequest(r)
			if err != nil {
				s.logger.Error("authentication failed",
					zap.Error(err),
					zap.String("path", r.URL.Path))

				errCode := ErrAccessDenied
				switch {
				case errors.Is(err, auth.ErrAccessKeyRevoked):
					// A revoked or rotated key, or an STS token of one: AWS's
					// code for a key that no longer exists (WP-R5-14).
					errCode = ErrInvalidAccessKeyId
				case errors.Is(err, auth.ErrSignatureMismatch):
					errCode = ErrSignatureDoesNotMatch
				case errors.Is(err, auth.ErrRequestTimeSkewed):
					errCode = ErrRequestTimeTooSkewed
				case errors.Is(err, auth.ErrInvalidContentSHA256):
					errCode = ErrInvalidArgument
				case strings.Contains(err.Error(), "invalid authorization format"),
					strings.Contains(err.Error(), "parse"):
					errCode = ErrSignatureDoesNotMatch
				}
				reqID := generateRequestID()
				if hint := authErrorHint(err.Error()); hint != "" {
					WriteS3ErrorWithContext(w, errCode, r.URL.Path, reqID, WithSuggestion(hint))
				} else {
					WriteS3Error(w, errCode, r.URL.Path, reqID)
				}
				reason, known := authFailureReason(err)
				recordAuthFailure(r, reason, known)
				return
			}
		}
	} else {
		if t, tErr := tenant.FromContext(r.Context()); tErr == nil && t != nil {
			tenantID = t.ID
		} else {
			tenantID = "test"
		}
		scope = &auth.KeyScope{Permissions: []string{"*"}}
		s.logger.Debug("test mode - skipping auth",
			zap.String("tenant_id", tenantID),
			zap.String("path", r.URL.Path))
	}

	// An id starting with '_' is the system's (engine.ChunkAddressTenant is
	// where every chunk blob lives, WP-R8-7): no credential may resolve to
	// one. Registration mints `tenant-<hex>`; this is the check that nobody
	// made such a row by hand.
	if tenantID != "" && engine.IsReservedTenantID(tenantID) {
		s.logger.Error("request refused: the credential resolves to a reserved tenant id",
			zap.String("tenant_id", tenantID), zap.String("path", r.URL.Path))
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	// The key's scope travels with the request: Object Lock reads it to
	// decide whether x-amz-bypass-governance-retention is this key's to send
	// (WP-R4-1 — it used to be a local of this function, and the header
	// alone was the bypass).
	r = r.WithContext(auth.WithKeyScope(r.Context(), scope))

	// Enforce key expiration and IP allowlist before any further processing.
	if scope != nil {
		if auth.IsKeyExpired(scope.ExpiresAt) {
			// The key's own code (WP-R5-12): ExpiredToken, with the key's
			// expiry in the message — not the presigned URL's "request has
			// expired" wording and not a generic AccessDenied.
			WriteS3ErrorWithContext(w, ErrExpiredPresignedRequest, r.URL.Path, generateRequestID(),
				WithSuggestion("This access key expired at "+scope.ExpiresAt.UTC().Format(time.RFC3339)+". Create or rotate a key in the dashboard."))
			recordAuthFailure(r, "expired", true)
			return
		}
		if !auth.CheckIPAllowlist(scope.IPAllowlist, extractClientIP(r)) {
			WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
				WithSuggestion("This key is restricted by IP address."))
			recordAuthFailure(r, "ip_denied", true)
			return
		}
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

	// Enforce permission and bucket scope now that we know the operation.
	if scope != nil {
		if !auth.CheckPermission(scope.Permissions, s3Req.Operation) {
			WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
				WithSuggestion(fmt.Sprintf("This key does not have %s access.", s3Req.Operation)))
			return
		}
		if s3Req.Bucket != "" && !auth.CheckBucketScope(scope.BucketScope, s3Req.Bucket) {
			WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
				WithSuggestion(fmt.Sprintf("This key is restricted to buckets: %s", strings.Join(scope.BucketScope, ", "))))
			return
		}
		// CopyObject reads the SOURCE with this key too (R5-02). Only the
		// PutObject dispatch honours x-amz-copy-source, so only it is gated.
		if hint, denied := copySourceScopeDenied(scope, r); denied && s3Req.Operation == "PutObject" {
			WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(), WithSuggestion(hint))
			return
		}
	}

	// A system bucket (WP-R10-3b) does not exist to the S3 API, except for
	// a read of one of its objects (the presigned export download).
	if systemBucketRefused(w, r, s3Req) {
		return
	}

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

	// Wrap response writer to count egress bytes for bandwidth tracking.
	// Every byte also lands on the tenant's month counter as it is written
	// (WP-R10-9), so a response still in flight counts toward the allowance.
	cw := &countingResponseWriter{ResponseWriter: w, head: r.Method == http.MethodHead}
	metered := tenantID != "" && tenantID != "default" && s.egress != nil
	if metered {
		cw.span = s.egress.span(r.Context(), tenantID)
	}

	// Install a backend-attribution slot: the engine records which storage
	// backend actually served this request, and bandwidth tracking below
	// attributes the bytes to it (backend_bandwidth_daily).
	noteCtx, _ := common.WithBackendNote(r.Context())
	r = r.WithContext(noteCtx)

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
		s.serveGetObject(cw, r, s3Req, tenantID, metered)
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
	case "ListObjectVersions":
		s.handleListObjectVersions(cw, r, s3Req)
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
	case "GetBucketAcl":
		s.handleGetBucketAcl(cw, r, s3Req)
	case "PutBucketAcl":
		s.handlePutBucketAcl(cw, r, s3Req)
	case "GetObjectAcl":
		s.handleGetObjectAcl(cw, r, s3Req)
	case "PutObjectAcl":
		s.handlePutObjectAcl(cw, r, s3Req)
	case "GetBucketLocation":
		s.handleGetBucketLocation(cw, r, s3Req)
	case "HeadBucket":
		s.handleHeadBucket(cw, r, s3Req)
	case "GetBucketVersioning":
		s.handleGetBucketVersioning(cw, r, s3Req)
	case "PutBucketVersioning":
		s.handlePutBucketVersioning(cw, r, s3Req)
	case "GetBucketNotification":
		s.handleGetBucketNotification(cw, r, s3Req)
	case "PutBucketNotification":
		s.handlePutBucketNotification(cw, r, s3Req)
	case "GetObjectLockConfiguration":
		s.handleGetObjectLockConfiguration(cw, r, s3Req)
	case "PutObjectLockConfiguration":
		s.handlePutObjectLockConfiguration(cw, r, s3Req)
	case "GetObjectRetention":
		s.handleGetObjectRetention(cw, r, s3Req)
	case "PutObjectRetention":
		s.handlePutObjectRetention(cw, r, s3Req)
	case "GetObjectLegalHold":
		s.handleGetObjectLegalHold(cw, r, s3Req)
	case "PutObjectLegalHold":
		s.handlePutObjectLegalHold(cw, r, s3Req)
	case "GetBucketLogging":
		s.handleGetBucketLogging(cw, r, s3Req)
	case "PutBucketLogging":
		s.handlePutBucketLogging(cw, r, s3Req)
	case "GetBucketInventory":
		s.handleGetBucketInventory(cw, r, s3Req)
	case "PutBucketInventory":
		s.handlePutBucketInventory(cw, r, s3Req)
	case "DeleteBucketInventory":
		s.handleDeleteBucketInventory(cw, r, s3Req)
	case "RestoreObject":
		s.handleRestoreObject(cw, r, s3Req)
	case "GetObjectTagging":
		s.handleGetObjectTagging(cw, r, s3Req)
	case "PutObjectTagging":
		s.handlePutObjectTagging(cw, r, s3Req)
	case "DeleteObjectTagging":
		s.handleDeleteObjectTagging(cw, r, s3Req)
	default:
		s.logger.Warn("operation not implemented",
			zap.String("operation", s3Req.Operation))
		WriteS3Error(cw, ErrNotImplemented, r.URL.Path, generateRequestID())
	}

	// Record S3 access log for authenticated requests (the resolved tenant:
	// identical to tenantID in production, the context tenant in test mode).
	if t.ID != "" && t.ID != "default" && s.accessLogTracker != nil {
		statusCode := cw.statusCode
		if statusCode == 0 {
			statusCode = http.StatusOK
		}
		s.accessLogTracker.Record(r.Context(), s3AccessEvent{
			tenantID:      t.ID,
			bucket:        s3Req.Bucket,
			objectKey:     s3Req.Object,
			operation:     s3Req.Operation,
			statusCode:    statusCode,
			bytesSent:     cw.bytesWritten,
			bytesReceived: ingressBytes,
			sourceIP:      extractClientIP(r),
			userAgent:     r.UserAgent(),
			requestID:     r.Header.Get("X-Request-Id"),
			loggedAt:      time.Now(),
		})
	}

	// Record bandwidth for authenticated requests, attributed to the backend
	// the engine reports having served the bytes ("" if none was touched).
	if tenantID != "" && tenantID != "default" && s.bandwidthTracker != nil {
		s.bandwidthTracker.recordResponse(tenantID, common.BackendUsed(r.Context()), ingressBytes, cw)
	}
}

// serveGetObject is the one place an object body leaves on the S3 surface
// (header-signed and presigned alike): past the egress allowance the body is
// paced through the tenant's token bucket (WP-R10-9). The wrapper goes on
// here, before the handler, so no path inside it can send around it.
func (s *Server) serveGetObject(cw *countingResponseWriter, r *http.Request, req *S3Request, tenantID string, metered bool) {
	if !metered {
		s.handleGetObject(cw, r, req)
		return
	}
	gw, admitted := s.egress.admit(cw, r, tenantID, egressSurfaceS3)
	if !admitted {
		// The one refusal: the tenant is being paced and already has
		// EGRESS_THROTTLE_MAX_STREAMS paced downloads open. It is the
		// client's doing, so it stays out of the 5xx count the pager reads.
		markClientRefusal(r.Context())
		cw.Header().Set("Retry-After", egressRetryAfter)
		WriteS3Error(cw, ErrSlowDown, r.URL.Path, generateRequestID())
		return
	}
	defer gw.close() // also on a panic: a leaked slot would shrink the guard for good
	s.handleGetObject(gw, r, req)
}

// handleGetObject handles S3 GET requests
func (s *Server) handleGetObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	adapter := NewS3ToEngine(s.engine, s.db, s.logger)
	adapter.sseService = s.sseService
	adapter.chunkEncSvc = s.chunkEncSvc
	adapter.gci = s.gci
	adapter.smartPromoter = s.smartPromoter
	if s.chunkGetPrefetch > 0 {
		adapter.chunkGetPrefetch = s.chunkGetPrefetch
	}

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

	if s.db == nil {
		// HEAD is served from object_head_cache and nowhere else (never a
		// backend round trip); without the database it cannot be answered.
		// R1-05: this used to dereference the nil pool.
		s.logger.Error("HEAD without a metadata database", zap.String("path", r.URL.Path))
		WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
		return
	}

	var sizeBytes int64
	var etag, contentType string
	var updatedAt time.Time
	var metadataJSON []byte
	var backendName string
	var floor string
	var encAlgo string
	var tagsJSON []byte
	var contentDisposition string
	var contentEncoding string
	var contentLanguage string
	var echo putEchoHeaders

	err = s.db.QueryRowContext(r.Context(), `
		SELECT size_bytes, etag, content_type, updated_at, COALESCE(metadata, '{}'), COALESCE(backend_name, ''), COALESCE(encryption_algorithm, ''), COALESCE(tags, '{}'), COALESCE(content_disposition, ''), COALESCE(content_encoding, ''), COALESCE(content_language, ''), COALESCE(cache_control, ''), COALESCE(http_expires, ''), COALESCE(website_redirect_location, ''), floor
		FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
	`, t.ID, req.Bucket, req.Object).Scan(&sizeBytes, &etag, &contentType, &updatedAt, &metadataJSON, &backendName, &encAlgo, &tagsJSON, &contentDisposition, &contentEncoding, &contentLanguage, &echo.CacheControl, &echo.Expires, &echo.WebsiteRedirect, &floor)

	if errors.Is(err, sql.ErrNoRows) {
		s.logger.Warn("HEAD: object not in metadata cache",
			zap.String("tenant_id", t.ID),
			zap.String("bucket", req.Bucket),
			zap.String("object", req.Object))
		reqID := generateRequestID()
		if suggestion := keySuggestion(r.Context(), s.db, t.ID, req.Bucket, req.Object); suggestion != "" {
			WriteS3ErrorWithContext(w, ErrNoSuchKey, r.URL.Path, reqID, WithSuggestion(suggestion))
		} else {
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, reqID)
		}
		return
	}
	if err != nil {
		s.logger.Error("HEAD: metadata query failed", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Conditionals apply to HEAD exactly as to GET (RFC 9110 §13; AWS
	// answers 304 / 412 on HeadObject) — R2-11 / R4-07.
	if code := evaluateConditionalGET(r, etag, updatedAt); code == http.StatusNotModified {
		writeNotModified(w, etag, updatedAt, "")
		return
	} else if code == http.StatusPreconditionFailed {
		WriteS3Error(w, ErrPreconditionFailed, r.URL.Path, generateRequestID())
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
	// The class follows the floor, not the backend (WP-R13-1): a Smart-demoted
	// downstairs object is STANDARD while its bytes sit on the cold backend.
	storageClass := engine.CustomerStorageClass(floor, backendName)
	w.Header().Set("x-amz-storage-class", storageClass)
	w.Header().Set("x-amz-request-id", generateRequestID())
	// V18.2: archive-class objects carry live restore state so Glacier restore
	// pollers (rclone, aws-cli s3api head-object) work unmodified. This is the
	// one deliberate exception to "HEAD never touches the backend" — the
	// x-amz-restore value only exists on the backend, and only objects that
	// REPORT an archive class (attic objects) pay the round trip. A demoted
	// downstairs object reports STANDARD, has no restore state to show and
	// makes no backend call. Failure just omits the header (HEAD itself must
	// stay reliable).
	if engine.IsArchiveClass(storageClass) && s.engine != nil {
		if drv, exists := s.engine.GetDriver(backendName); exists {
			if restorer, isRestorer := drv.(engine.Restorer); isRestorer {
				if st, stErr := restorer.RestoreStatus(r.Context(), t.NamespaceContainer(req.Bucket), req.Object); stErr == nil {
					if st.Restore != "" {
						w.Header().Set("x-amz-restore", st.Restore)
					}
				} else {
					s.logger.Debug("HEAD: restore status unavailable", zap.Error(stErr))
				}
			}
		}
	}
	if encAlgo == crypto.SSECAlgorithm {
		if !crypto.HasSSECHeaders(r) {
			WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
				WithSuggestion("This object was encrypted with SSE-C. Provide the encryption key to access metadata."))
			return
		}
		w.Header().Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
	} else if encAlgo != "" {
		w.Header().Set("x-amz-server-side-encryption", "AES256")
	}
	setS3MetadataHeaders(w, metadataJSON)
	if n := tagCount(tagsJSON); n > 0 {
		w.Header().Set("x-amz-tagging-count", strconv.Itoa(n))
	}
	if cd := sanitizeContentDisposition(contentDisposition); cd != "" {
		w.Header().Set("Content-Disposition", cd)
	}
	if contentEncoding != "" {
		w.Header().Set("Content-Encoding", contentEncoding)
	}
	if contentLanguage != "" {
		w.Header().Set("Content-Language", contentLanguage)
	}
	setEchoHeaders(w.Header(), echo.CacheControl, echo.Expires, echo.WebsiteRedirect)
	// HEAD must not write a body.
	w.WriteHeader(http.StatusOK)
}

// handlePutObject handles PUT requests
func (s *Server) handlePutObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	// WP-1: single quota reservation site. Reserve the declared size before
	// the write, settle to the recorded logical size after it succeeds,
	// release on failure, release the overwritten object's size on overwrite
	// (the overwritten size is captured atomically by the head-cache upsert).
	quotaOn := s.quotaManager != nil && req.TenantID != ""
	// A free-tier tenant at the bucket cap cannot grow a new bucket by
	// writing into it (R10-12).
	if s.freeTierBucketCapBlocksWrite(r.Context(), req.TenantID, req.Bucket) {
		writeFreeTierBucketCap(w, r)
		return
	}
	// The floor is decided by the storage class the write resolves to
	// (header, then bucket tier, then public-bucket placement); resolve it
	// once here, reserve on it, and hand it to the adapter so placement and
	// billing can never disagree.
	storageClass := resolvePutStorageClass(r.Context(), s.db, s.engine, req.TenantID, req.Bucket,
		r.Header.Get("x-amz-storage-class"))
	floor := usage.FloorOf(storageClass)
	var reserved int64
	{
		size := r.ContentLength
		// The decoded length is only meaningful inside aws-chunked framing;
		// on a plain body it is a client-invented number (R2-06).
		if decoded := r.Header.Get("x-amz-decoded-content-length"); decoded != "" && isAWSChunked(r) {
			if n, err := strconv.ParseInt(decoded, 10, 64); err == nil {
				size = n
			}
		}
		if size < 0 {
			// No determinable size (chunked transfer without a decoded-length
			// header): the write cannot be quota-checked before storage, and
			// its head-cache row would record size 0 for real bytes. AWS S3
			// requires a length on PUT for the same reason.
			WriteS3Error(w, ErrMissingContentLength, r.URL.Path, generateRequestID())
			return
		}
		if quotaOn && size > 0 {
			ok, err := reserveQuota(r.Context(), s.quotaManager, req.TenantID, floor, size)
			if err != nil {
				// Fail closed: an unmetered write would corrupt billing.
				s.logger.Error("quota check failed",
					zap.Error(err), zap.String("tenant_id", req.TenantID))
				WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
				return
			}
			if !ok {
				WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
					WithSuggestion("Upgrade at https://stored.ge/dashboard/billing"))
				return
			}
			reserved = size
		}
	}

	adapter := NewS3ToEngine(s.engine, s.db, s.logger)
	adapter.sseService = s.sseService
	adapter.chunkEncSvc = s.chunkEncSvc
	adapter.gci = s.gci
	adapter.flags = s.flags
	adapter.storageClass, adapter.storageClassResolved = storageClass, true
	if s.chunkPutConcurrency > 0 {
		adapter.chunkStoreConcurrency = s.chunkPutConcurrency
	}

	s.logger.Debug("S3 PUT translating to engine",
		zap.String("s3.bucket", req.Bucket),
		zap.String("s3.object", req.Object),
		zap.String("engine.container", req.Bucket),
		zap.String("engine.artifact", req.Object),
		zap.Int64("size", r.ContentLength))

	rec := &countingResponseWriter{ResponseWriter: w}
	adapter.HandlePut(rec, r, req.Bucket, req.Object)

	if !quotaOn {
		return
	}
	ctx, cancel := quotaCtx(r)
	defer cancel()
	if rec.statusCode >= 200 && rec.statusCode < 300 {
		s.settlePutQuota(ctx, req.TenantID, floor, reserved, adapter.putLogicalBytes, adapter.displaced)
	} else {
		s.releaseQuota(ctx, req.TenantID, floor, reserved)
	}
}

// handleDeleteObject handles DELETE requests
func (s *Server) handleDeleteObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	if err := checkMFADelete(r.Context(), s.db, s.auth, s.mfaService, req.TenantID, req.Bucket, r); err != nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}
	adapter := NewS3ToEngine(s.engine, s.db, s.logger)
	adapter.gci = s.gci
	adapter.quota = s.quotaManager
	adapter.HandleDelete(w, r, req.Bucket, req.Object)
}

// handleListObjects handles bucket listing
func (s *Server) handleListObjects(w http.ResponseWriter, r *http.Request, req *S3Request) {
	adapter := NewS3ToEngine(s.engine, s.db, s.logger)
	adapter.HandleListV2(w, r, req.Bucket)
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

// extractClientIP returns the real client IP. All proxy-header trust rules
// live in internal/clientip (last X-Forwarded-For entry = the peer HAProxy
// appended; CF-Connecting-IP only behind a Cloudflare edge) — see R1-01.
func extractClientIP(r *http.Request) string {
	return clientip.FromRequest(r)
}
