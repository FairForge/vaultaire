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

	p.parseHeaders(req, r) // before the operation: UploadPartCopy is named by a header
	p.determineOperation(req, r.Method)

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
			} else if _, ok := req.Query["cors"]; ok {
				req.Operation = auth.OpGetBucketCors
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
			} else if _, ok := req.Query["cors"]; ok {
				req.Operation = auth.OpPutBucketCors
			} else {
				req.Operation = auth.OpCreateBucket
			}
		case "DELETE":
			if _, ok := req.Query["inventory"]; ok {
				req.Operation = auth.OpDeleteBucketInventory
			} else if _, ok := req.Query["cors"]; ok {
				req.Operation = auth.OpDeleteBucketCors
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
			// x-amz-copy-source on an UploadPart = UploadPartCopy (WP-R3-2).
			if req.Headers["X-Amz-Copy-Source"] != "" {
				req.Operation = auth.OpUploadPartCopy
			} else {
				req.Operation = auth.OpUploadPart
			}
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
	// An unsigned non-GET of an operational path answers an empty 200, as it
	// always has; a SIGNED request there is a bucket's (RP56-1: the four are
	// valid bucket names, and the credential gate sends them here).
	if !signedS3Request(r) && (r.URL.Path == "/health" || r.URL.Path == "/ready" ||
		r.URL.Path == "/metrics" || r.URL.Path == "/version") {
		return
	}

	// CORS (s3_cors.go): every response to a browser's request varies on
	// Origin. Which rule's headers it carries is decided once the tenant is
	// known (applyS3CORSForTenant below — the caller's own bucket only); a
	// response written before that (an auth failure) may carry
	// Access-Control-Allow-Origin alone (applyS3CORSPreAuth).
	varyOrigin(w, r)

	// RP78-3: the request's one id, on every response and in its row.
	w, _ = withS3RequestID(w)

	var tenantID string
	var scope *auth.KeyScope
	var err error

	if !s.testMode {
		if isPresignedRequest(r) {
			tenantID, scope, err = s.verifyPresignedURL(r)
			if err != nil {
				// A refused credential is the client's outcome, not ours: Info,
				// no stack (Prompt 2b.4 E4.2 — scanners sent dozens an hour at
				// Error). The signal is vaultaire_auth_failures_total.
				s.logger.Info("presigned URL verification failed",
					zap.Error(err),
					zap.String("path", r.URL.Path))

				errCode := err.Error()
				reqID := generateRequestID()
				s.applyS3CORSPreAuth(w, r)
				switch errCode {
				case errTestPresignExpires:
					WriteS3ErrorWithContext(w, ErrAuthorizationQueryParametersError, r.URL.Path, reqID,
						WithMessage(testPresignExpiresMessage))
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
					known = s.accessKeyExists(r.Context(), attemptedAccessKey(r))
				}
				recordAuthFailure(r, reason, known)
				return
			}
		} else {
			tenantID, scope, err = s.s3Auth().ValidateRequest(r)
			if err != nil && r.Header.Get("Authorization") == "" && s.serveWebNotFound(w, r) {
				return // a browser's typo on the web host: the 404 page, not AccessDenied XML
			}
			if errors.Is(err, auth.ErrIPDenied) {
				// The key's allow-list, applied inside the one verifier
				// (WP-EDGE-1 Part A): the same refusal as before.
				s.applyS3CORSPreAuth(w, r)
				WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
					WithSuggestion("This key is restricted by IP address."))
				recordAuthFailure(r, "ip_denied", true)
				return
			}
			if err != nil {
				s.logger.Info("authentication failed", // the client's outcome (E4.2)
					zap.Error(err),
					zap.String("path", r.URL.Path))

				errCode := ErrAccessDenied
				switch {
				case errors.Is(err, auth.ErrCredentialUnavailable):
					// A wrapped secret we could not open (plan 43.7): ours,
					// never the caller's AccessDenied.
					errCode = ErrInternalError
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
				s.applyS3CORSPreAuth(w, r)
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
		s.applyS3CORSPreAuth(w, r)
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	// WP-LOG-1: a request whose credential resolved a tenant is recorded
	// ONCE, whatever answers it — a refusal below included — by the deferred
	// recordS3Access (the bucket log and the request log, access_log.go).
	// The writer counts the body and keeps the S3 error answered.
	var cw *countingResponseWriter
	var s3Req *S3Request
	var ingressBytes int64
	if tenantID != "" {
		cw = &countingResponseWriter{ResponseWriter: w, head: r.Method == http.MethodHead}
		w = cw
		defer func() { s.recordS3Access(cw, r, tenantID, s3Req, ingressBytes) }()
	}

	// WP-DX-1: a test-mode (sandbox) tenant — a VLT_TEST_ key's — is
	// metered under its own id and never billed (RP80-1), and every
	// response says so (sandbox.go).
	sandbox := isSandbox(r.Context(), s.db, tenantID)
	if sandbox {
		w.Header().Set("x-stored-mode", "test")
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
			s.applyS3CORSPreAuth(w, r)
			WriteS3ErrorWithContext(w, ErrExpiredPresignedRequest, r.URL.Path, generateRequestID(),
				WithSuggestion("This access key expired at "+scope.ExpiresAt.UTC().Format(time.RFC3339)+". Create or rotate a key in the dashboard."))
			recordAuthFailure(r, "expired", true)
			return
		}
		if !auth.CheckIPAllowlist(scope.IPAllowlist, extractClientIP(r)) {
			s.applyS3CORSPreAuth(w, r)
			WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
				WithSuggestion("This key is restricted by IP address."))
			recordAuthFailure(r, "ip_denied", true)
			return
		}
	}

	// The caller is who the key says: the CORS headers of ITS bucket (never
	// another tenant's same-named one) on every response from here on — the
	// operation's success and its errors alike.
	s.applyS3CORSForTenant(w, r, tenantID)

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

	s3Req, err = parser.ParseRequest(r)
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
		if !auth.OperationAllowed(scope.Permissions, s3Req.Operation) {
			WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
				WithSuggestion(fmt.Sprintf("This key does not have %s access.", s3Req.Operation)))
			return
		}
		if s3Req.Bucket != "" && !auth.CheckBucketScope(scope.BucketScope, s3Req.Bucket) {
			WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
				WithSuggestion(fmt.Sprintf("This key is restricted to buckets: %s", strings.Join(scope.BucketScope, ", "))))
			return
		}
		// A copy reads the SOURCE with this key too (R5-02): CopyObject (a
		// PutObject with x-amz-copy-source) and UploadPartCopy (WP-R3-2) —
		// one gate, the two operations that honour the header.
		if hint, denied := copySourceScopeDenied(scope, r); denied &&
			(s3Req.Operation == auth.OpPutObject || s3Req.Operation == auth.OpUploadPartCopy) {
			WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(), WithSuggestion(hint))
			return
		}
	}

	// A system bucket (WP-R10-3b) does not exist to the S3 API, except for
	// a read of one of its objects (the presigned export download).
	if systemBucketRefused(w, r, s3Req) {
		return
	}
	// The bucket name and the key length, for every operation (WP-R2-7).
	if requestNameRefused(w, r, s3Req) {
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
	if cw == nil { // an anonymous request: counted, not recorded
		cw = &countingResponseWriter{ResponseWriter: w, head: r.Method == http.MethodHead}
	}
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
		} else if !s.slcWrite(cw, r, t.ID) {
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
		if !s.slcWrite(cw, r, t.ID) {
			s.handleUploadPart(cw, r, s3Req.Bucket, s3Req.Object)
		}
	case "UploadPartCopy":
		s.handleUploadPartCopy(cw, r, s3Req.Bucket, s3Req.Object)
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
	case "GetBucketCors":
		s.handleGetBucketCors(cw, r, s3Req)
	case "PutBucketCors":
		s.handlePutBucketCors(cw, r, s3Req)
	case "DeleteBucketCors":
		s.handleDeleteBucketCors(cw, r, s3Req)
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

	// Record bandwidth for authenticated requests, attributed to the backend
	// the engine reports having served the bytes ("" if none was touched).
	if tenantID != "" && tenantID != "default" && s.bandwidthTracker != nil {
		s.bandwidthTracker.recordResponse(tenantID, common.BackendUsed(r.Context()), ingressBytes, cw)
	}
}

// recordS3Access writes one request to the access-log tracker (the bucket
// log and the request log, WP-LOG-1): what the response writer saw — the
// status, the bytes, the S3 error code and the request id answered — and
// what the request named. Deferred by handleS3Request from the moment the
// tenant is known, so a refusal after authentication is recorded too.
func (s *Server) recordS3Access(cw *countingResponseWriter, r *http.Request, tenantID string, req *S3Request, ingress int64) {
	if s.accessLogTracker == nil || tenantID == "" || tenantID == "default" {
		return
	}
	status := cw.statusCode
	if status == 0 {
		status = http.StatusOK
	}
	reqID := s3RequestIDOf(cw, cw.errRequestID) // the request's one id (RP78-3)
	if reqID == "" {
		reqID = cw.Header().Get("x-amz-request-id")
	}
	ev := s3AccessEvent{
		tenantID: tenantID, operation: r.Method, statusCode: status,
		bytesSent: cw.bytesWritten, bytesReceived: ingress,
		sourceIP: extractClientIP(r), userAgent: r.UserAgent(),
		requestID: reqID, errorCode: cw.errCode,
		keyID: attemptedAccessKey(r), host: "origin", loggedAt: time.Now(),
	}
	if req != nil {
		ev.bucket, ev.objectKey, ev.operation = req.Bucket, req.Object, req.Operation
	}
	s.accessLogTracker.Record(r.Context(), ev)
}

// serveGetObject is the one place an object body leaves on the S3 surface
// (header-signed and presigned alike): past the egress allowance the body is
// paced through the tenant's token bucket (WP-R10-9). The wrapper goes on
// here, before the handler, so no path inside it can send around it.
func (s *Server) serveGetObject(cw *countingResponseWriter, r *http.Request, req *S3Request, tenantID string, metered bool) {
	slcTenant := tenantID
	if t, err := tenant.FromContext(r.Context()); err == nil && t != nil {
		slcTenant = t.ID
	}
	if !metered {
		if !s.slcRead(cw, r, slcTenant) {
			s.handleGetObject(cw, r, req)
		}
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
	if !s.slcRead(gw, r, slcTenant) {
		s.handleGetObject(gw, r, req)
	}
}

// handleGetObject handles S3 GET requests
func (s *Server) handleGetObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	adapter := s.readAdapter()

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

	// HEAD ?versionId (WP-R2-1 C2) answers from the version's own row — a
	// non-current version keeps its bytes since C1 — never from the head row,
	// which describes the current object. The latest version falls through
	// to the head row (it IS the current object).
	if vid := r.URL.Query().Get("versionId"); vid != "" {
		if s.headVersion(w, r, t.ID, req, vid) {
			return
		}
		w.Header().Set("x-amz-version-id", vid)
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
	var storageKey string
	var ssecKeyMD5 string
	var lockMode string
	var lockUntil sql.NullTime
	var legalHold sql.NullBool

	// One query: the head row with the key's lock row joined (RP17-1) —
	// HEAD stays one round trip to the cache (non-negotiable 2).
	err = s.db.QueryRowContext(r.Context(), `
		SELECT h.size_bytes, h.etag, h.content_type, h.updated_at, COALESCE(h.metadata, '{}'), COALESCE(h.backend_name, ''), COALESCE(h.encryption_algorithm, ''), COALESCE(h.tags, '{}'), COALESCE(h.content_disposition, ''), COALESCE(h.content_encoding, ''), COALESCE(h.content_language, ''), COALESCE(h.cache_control, ''), COALESCE(h.http_expires, ''), COALESCE(h.website_redirect_location, ''), h.floor, COALESCE(h.storage_key, ''), COALESCE(h.ssec_key_md5, ''),
		       `+objectLockColumns+`
		FROM object_head_cache h
		LEFT JOIN object_locks l ON l.tenant_id = h.tenant_id AND l.bucket = h.bucket AND l.object_key = h.object_key
		WHERE h.tenant_id = $1 AND h.bucket = $2 AND h.object_key = $3
	`, t.ID, req.Bucket, req.Object).Scan(&sizeBytes, &etag, &contentType, &updatedAt, &metadataJSON, &backendName, &encAlgo, &tagsJSON, &contentDisposition, &contentEncoding, &contentLanguage, &echo.CacheControl, &echo.Expires, &echo.WebsiteRedirect, &floor, &storageKey, &ssecKeyMD5,
		&lockMode, &lockUntil, &legalHold)

	// R85-2: a HEAD of the current object of a versioned bucket names its
	// version, as GET does — and a key whose latest version is a delete
	// marker says so on its 404.
	latestVID, latestMarker := "", false
	if r.URL.Query().Get("versionId") == "" {
		latestVID, latestMarker = s.latestVersion(r.Context(), t.ID, req.Bucket, req.Object, err == nil)
	}
	if errors.Is(err, sql.ErrNoRows) {
		s.logger.Warn("HEAD: object not in metadata cache",
			zap.String("tenant_id", t.ID),
			zap.String("bucket", req.Bucket),
			zap.String("object", req.Object))
		if latestMarker {
			w.Header().Set("x-amz-version-id", latestVID)
			w.Header().Set("x-amz-delete-marker", "true")
		}
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

	// An SSE-C object without the customer's key: AWS's 400, decided before
	// any object header is set (R8-14; R2-22: size and ETag went out first).
	if crypto.IsSSEC(encAlgo) && !crypto.HasSSECHeaders(r) {
		WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(), WithMessage(sseCRefusal))
		return
	}
	// A key of another MD5 than the row recorded: AWS's 403, cache-only
	// (RP14-11, 094 — HEAD is still one row read; it answered 200 because
	// the key could only be checked against the blob). A row with no MD5
	// (written before 094) still answers 200.
	if crypto.IsSSEC(encAlgo) && ssecKeyMismatch(r, ssecKeyMD5) {
		w.Header().Del("x-amz-version-id")
		WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(), WithMessage(sseCKeyRefusal))
		return
	}

	// The class follows the floor, not the backend (WP-R13-1): a Smart-demoted
	// downstairs object is STANDARD while its bytes sit on the cold backend.
	storageClass := engine.CustomerStorageClass(floor, backendName)
	disposition := r.URL.Query().Get("response-content-disposition")
	if disposition == "" {
		disposition = contentDisposition
	}
	oh := objectHeaders{ETag: etag, LastModified: updatedAt, ContentType: contentType, Size: sizeBytes,
		StorageClass: storageClass, Metadata: metadataJSON, TagCount: tagCount(tagsJSON),
		Disposition: disposition, Encoding: contentEncoding, Language: contentLanguage,
		CacheControl: echo.CacheControl, Expires: echo.Expires, WebsiteRedirect: echo.WebsiteRedirect,
		SSE: sseOf(encAlgo, crypto.IsSSEC(encAlgo)), SSECKeyMD5: ssecKeyMD5}
	oh.LockMode, oh.LockRetainUntil, oh.LegalHold = lockHeadersOf(lockMode, lockUntil, legalHold)
	switch {
	case latestVID != "" && !latestMarker:
		oh.VersionID = latestVID
	case r.URL.Query().Get("versionId") != "":
		oh.VersionID = r.URL.Query().Get("versionId")
	}

	// Conditionals apply to HEAD exactly as to GET (RFC 9110 §13; AWS
	// answers 304 / 412 on HeadObject) — R2-11 / R4-07.
	if code := evaluateConditionalGET(r, etag, updatedAt); code == http.StatusNotModified {
		oh.write(w, headersNotModified, nil)
		w.WriteHeader(http.StatusNotModified)
		return
	} else if code == http.StatusPreconditionFailed {
		WriteS3Error(w, ErrPreconditionFailed, r.URL.Path, generateRequestID())
		return
	}

	// A Range answers as on GET (AWS): 206 with the range's length, 416 when
	// nothing satisfies it (any range of an empty object — R2-21); a
	// multi-range is ignored (RFC 9110 §14.2).
	mode, rng, ok := headRange(w, r, sizeBytes)
	if !ok {
		return
	}
	oh.write(w, mode, rng)
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
				// The restore state of the blob the row addresses (WP-R2-1).
				if st, stErr := restorer.RestoreStatus(r.Context(), t.NamespaceContainer(req.Bucket), blobArtifact(req.Object, storageKey)); stErr == nil {
					if st.Restore != "" {
						w.Header().Set("x-amz-restore", st.Restore)
					}
				} else {
					s.logger.Debug("HEAD: restore status unavailable", zap.Error(stErr))
				}
			}
		}
	}
	// HEAD must not write a body.
	w.WriteHeader(headStatus(mode))
}

// headRange reads a HEAD's Range against an object of size bytes: the mode
// and the range to answer with, or false when it answered 416 itself.
func headRange(w http.ResponseWriter, r *http.Request, size int64) (headerMode, *httpRange, bool) {
	rh := r.Header.Get("Range")
	if rh == "" || errors.Is(rangeParseErr(rh, size), errMultiRange) {
		return headersHead, nil, true
	}
	rng, err := parseRangeHeader(rh, size)
	if err != nil {
		writeRangeNotSatisfiable(w, size)
		return 0, nil, false
	}
	return headersRange, rng, true
}

// headStatus is the status a HEAD answers in mode.
func headStatus(mode headerMode) int {
	if mode == headersRange {
		return http.StatusPartialContent
	}
	return http.StatusOK
}

// latestVersion is the key's latest version id on a versioned bucket ("" on
// an unversioned one, or none): a delete marker (marker), the id of the
// current object, or "null" for a current object with no version row (one
// written before versioning was enabled — ListObjectVersions lists it as
// null). The database only, as every HEAD.
func (s *Server) latestVersion(ctx context.Context, tenantID, bucket, key string, headFound bool) (vid string, marker bool) {
	if vs := getBucketVersioningStatus(ctx, s.db, tenantID, bucket); vs != "Enabled" && vs != "Suspended" {
		return "", false
	}
	err := s.db.QueryRowContext(ctx, `SELECT version_id, is_delete_marker FROM object_versions
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_latest`, tenantID, bucket, key).Scan(&vid, &marker)
	switch {
	case err == nil:
		return vid, marker
	case errors.Is(err, sql.ErrNoRows) && headFound:
		return "null", false
	}
	return "", false
}

// headVersion answers HEAD ?versionId=vid from the object_versions row and
// reports true, or reports false for the key's latest version (the head row
// describes it). The R2-03 501 is kept only for a non-current version
// written before C1, which kept no bytes (no address) — except the newest
// live version behind a delete marker, whose bytes are intact at <key>. A
// delete marker answers 404 with x-amz-delete-marker, as GET does. Like
// every HEAD: the database only, never a backend call.
func (s *Server) headVersion(w http.ResponseWriter, r *http.Request, tenantID string, req *S3Request, vid string) bool {
	var marker, latest bool
	var etag, contentType, storageKey, backend, encAlgo, floor string
	var size int64
	var createdAt time.Time
	var attrs objectAttrs
	var meta []byte
	var ssecKeyMD5 string
	var lockMode string
	var lockUntil sql.NullTime
	var legalHold sql.NullBool
	err := s.db.QueryRowContext(r.Context(), `
		SELECT v.is_delete_marker, v.is_latest, v.etag, v.content_type, v.size_bytes, v.created_at,
		       COALESCE(v.storage_key, ''), COALESCE(v.backend_name, ''), COALESCE(v.encryption_algorithm, ''), COALESCE(v.floor, ''),
		       `+versionAttrColumns+`, COALESCE(v.ssec_key_md5, ''),
		       `+objectLockColumns+`
		FROM object_versions v
		LEFT JOIN object_locks l ON l.tenant_id = v.tenant_id AND l.bucket = v.bucket AND l.object_key = v.object_key
		WHERE v.tenant_id = $1 AND v.bucket = $2 AND v.object_key = $3 AND v.version_id = $4`,
		tenantID, req.Bucket, req.Object, vid).Scan(&marker, &latest, &etag, &contentType, &size, &createdAt,
		&storageKey, &backend, &encAlgo, &floor, &meta, &attrs.ContentDisposition, &attrs.ContentEncoding, &attrs.ContentLanguage,
		&attrs.CacheControl, &attrs.Expires, &attrs.WebsiteRedirect, &ssecKeyMD5, &lockMode, &lockUntil, &legalHold)
	if errors.Is(err, sql.ErrNoRows) {
		if isUnanchoredNullVersion(r.Context(), s.db, tenantID, req.Bucket, req.Object, vid) {
			return false // the current object is the null version (GET answers the same)
		}
		WriteS3Error(w, ErrNoSuchVersion, r.URL.Path, generateRequestID())
		return true
	}
	if err != nil {
		s.logger.Error("HEAD ?versionId: version query failed", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return true
	}
	if marker {
		// A version id naming a delete marker: AWS answers 405 (a marker has
		// no object to read), naming the marker.
		writeDeleteMarkerVersion(w, r, vid, createdAt)
		return true
	}
	if latest {
		return false
	}
	w.Header().Set("x-amz-version-id", vid)
	if storageKey == "" && !NewS3ToEngine(s.engine, s.db, s.logger).versionBytesIntact(r.Context(), tenantID, req.Bucket, req.Object, vid) {
		WriteS3ErrorWithContext(w, ErrNotImplemented, r.URL.Path, generateRequestID(),
			WithSuggestion("This version was written before 2026-10-12, when versions began keeping their own bytes; its data is not retained."))
		return true
	}
	if crypto.IsSSEC(encAlgo) && !crypto.HasSSECHeaders(r) {
		w.Header().Del("x-amz-version-id")
		WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(), WithMessage(sseCRefusal))
		return true
	}
	if crypto.IsSSEC(encAlgo) && ssecKeyMismatch(r, ssecKeyMD5) {
		// The version row's own key MD5 (RP14-11, 094).
		w.Header().Del("x-amz-version-id")
		WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(), WithMessage(sseCKeyRefusal))
		return true
	}
	if floor == "" {
		floor = usage.FloorStandard
	}
	// What the version recorded (R61-8, 092): none of it for one written
	// before — the current object's belong to the current object. Tags are
	// the key's (per-version tags are not kept), as GET ?versionId sends.
	disposition := r.URL.Query().Get("response-content-disposition")
	if disposition == "" {
		disposition = attrs.ContentDisposition
	}
	oh := objectHeaders{ETag: etag, LastModified: createdAt, ContentType: contentType, Size: size,
		StorageClass: engine.CustomerStorageClass(floor, backend), VersionID: vid, Metadata: meta,
		Disposition: disposition, Encoding: attrs.ContentEncoding, Language: attrs.ContentLanguage,
		CacheControl: attrs.CacheControl, Expires: attrs.Expires, WebsiteRedirect: attrs.WebsiteRedirect,
		SSE: sseOf(encAlgo, crypto.IsSSEC(encAlgo)), SSECKeyMD5: ssecKeyMD5}
	oh.LockMode, oh.LockRetainUntil, oh.LegalHold = lockHeadersOf(lockMode, lockUntil, legalHold)
	if code := evaluateConditionalGET(r, etag, createdAt); code == http.StatusNotModified {
		oh.write(w, headersNotModified, nil)
		w.WriteHeader(http.StatusNotModified)
		return true
	} else if code == http.StatusPreconditionFailed {
		WriteS3Error(w, ErrPreconditionFailed, r.URL.Path, generateRequestID())
		return true
	}
	mode, rng, ok := headRange(w, r, size)
	if !ok {
		return true
	}
	oh.write(w, mode, rng)
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.WriteHeader(headStatus(mode))
	return true
}

// writeDeleteMarkerVersion answers a GET or HEAD whose versionId names a
// delete marker as AWS does: 405 MethodNotAllowed with the marker named.
func writeDeleteMarkerVersion(w http.ResponseWriter, r *http.Request, vid string, at time.Time) {
	w.Header().Set("x-amz-version-id", vid)
	w.Header().Set("x-amz-delete-marker", "true")
	if !at.IsZero() {
		w.Header().Set("Last-Modified", at.UTC().Format(http.TimeFormat))
	}
	w.Header().Set("Allow", "DELETE")
	WriteS3Error(w, ErrMethodNotAllowed, r.URL.Path, generateRequestID())
}

// readAdapter is the adapter a read runs on: the S3 GET and the CDN's read
// of a chunked object (one chunk read-ahead configuration for both).
func (s *Server) readAdapter() *S3ToEngine {
	a := NewS3ToEngine(s.engine, s.db, s.logger)
	a.sseService = s.sseService
	a.chunkEncSvc = s.chunkEncSvc
	a.gci = s.gci
	a.tenantKeys = s.tenantKeys
	a.smartPromoter = s.smartPromoter
	a.siaPromoter = s.siaPromoter
	a.vaultParity = s.vaultParity
	if s.chunkGetPrefetch > 0 {
		a.chunkGetPrefetch = s.chunkGetPrefetch
	}
	if s.largeGet.budget != nil { // a Server built by NewServer
		a.largeGet = s.largeGet
	}
	a.flags = s.flags
	return a
}

// writeAdapter is the adapter a write runs on: the server's encryption
// services, content index, flags and chunk knobs — the one place a PUT and a
// CompleteMultipartUpload take them from, so the two place the same bytes
// the same way (WP-R3-1).
func (s *Server) writeAdapter() *S3ToEngine {
	a := NewS3ToEngine(s.engine, s.db, s.logger)
	a.sseService = s.sseService
	a.allowUnsealedCleartext = s.allowUnsealedCleartext
	a.allowUnsealedSia = s.allowUnsealedSia
	a.allowUnsealedTape = s.allowUnsealedTape
	a.chunkEncSvc = s.chunkEncSvc
	a.tenantKeys = s.tenantKeys
	a.gci = s.gci
	a.flags = s.flags
	if s.chunkPutConcurrency > 0 {
		a.chunkStoreConcurrency = s.chunkPutConcurrency
	}
	a.chunkingThreshold = s.chunkingThreshold
	if s.chunkPendingTakeover > 0 {
		a.chunkPendingTakeover = s.chunkPendingTakeover
	}
	if s.chunkPutBound > 0 {
		a.chunkPutBound = s.chunkPutBound
	}
	a.chunkPuts = s.chunkPuts()
	a.chunkPutBudget = s.chunkPutBudget
	return a
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
	// The bucket must exist, before a byte of the body is read (R85-1).
	if !s.requireWriteBucket(w, r, "put", req.TenantID, req.Bucket) {
		return
	}
	// The floor is decided by the storage class the write resolves to
	// (header, then bucket tier, then public-bucket placement); resolve it
	// once here, reserve on it, and hand it to the adapter so placement and
	// billing can never disagree.
	storageClass := resolvePutStorageClass(r.Context(), s.db, s.engine, tierPlacementGate(s.flags), req.TenantID, req.Bucket,
		r.Header.Get("x-amz-storage-class"))
	floor := usage.FloorOf(storageClass)
	var bill *writeBill
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
		if quotaOn {
			// Net of what the commit releases on this floor at the quota
			// edge (Part B); settled in the commit's transaction (Part A).
			b, ok, err := s.reserveWrite(r.Context(), req.TenantID, req.Bucket, req.Object, floor, size)
			if err != nil {
				// Fail closed: an unmetered write would corrupt billing.
				s.logger.Error("quota check failed",
					zap.Error(err), zap.String("tenant_id", req.TenantID))
				WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
				return
			}
			if !ok {
				WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
					WithSuggestion("Upgrade at "+billingURL()))
				return
			}
			bill = b
		}
	}

	adapter := s.writeAdapter()
	adapter.storageClass, adapter.storageClassResolved = storageClass, true
	adapter.bill = bill

	s.logger.Debug("S3 PUT translating to engine",
		zap.String("s3.bucket", req.Bucket),
		zap.String("s3.object", req.Object),
		zap.String("engine.container", req.Bucket),
		zap.String("engine.artifact", req.Object),
		zap.Int64("size", r.ContentLength))

	rec := &countingResponseWriter{ResponseWriter: w}
	adapter.HandlePut(rec, r, req.Bucket, req.Object)

	// The bill was settled in the write's commit; a write that committed
	// nothing gives its reservation back.
	if quotaOn && bill != nil && !adapter.billSettled {
		ctx, cancel := quotaCtx(r)
		defer cancel()
		s.releaseQuota(ctx, req.TenantID, floor, bill.Reserved)
	}
}

// handleDeleteObject handles DELETE requests
func (s *Server) handleDeleteObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	if err := checkMFADelete(r.Context(), s.db, s.auth, s.mfaService, req.TenantID, req.Bucket, r); err != nil {
		s.logMFADeleteUnknown(err, req.Bucket)
		writeMFADeleteRefusal(w, r, err)
		return
	}
	adapter := NewS3ToEngine(s.engine, s.db, s.logger)
	adapter.gci = s.gci
	adapter.quota = s.quotaManager
	adapter.vaultParity = s.vaultParity
	// Without the promoter the delete's aftermath cannot settle a
	// Smart-demoted object's second copy (it was missing here until the
	// 2026-10-07 review; only the batch path had it).
	adapter.smartPromoter = s.smartPromoter
	adapter.siaPromoter = s.siaPromoter
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
// A sandbox, and a platform's child, is suspended with its parent (RP80-2:
// an account suspended for abuse kept its test keys; WP-PLAT-1) — and an
// app's child with its platform (WP-ACCT-1, depth 2). Suspension flows down
// managed and app edges only: a SPONSORED child is a person's own account,
// never suspended for its sponsor's abuse (docs/ACCOUNTS.md). One query
// reads the tenant, its parent and its grandparent.
func isTenantSuspended(ctx context.Context, db *sql.DB, tenantID string) bool {
	var suspended bool
	err := db.QueryRowContext(ctx, `
		SELECT t.suspended_at IS NOT NULL
		    OR (COALESCE(t.child_kind, '') <> 'sponsored' AND p.suspended_at IS NOT NULL)
		    OR (t.parent_tenant_id IS NOT NULL AND COALESCE(t.child_kind, '') <> 'sponsored'
		        AND COALESCE(p.child_kind, '') <> 'sponsored' AND g.suspended_at IS NOT NULL)
		  FROM tenants t
		  LEFT JOIN tenants p ON p.id = COALESCE(t.sandbox_of, t.parent_tenant_id)
		  LEFT JOIN tenants g ON g.id = p.parent_tenant_id
		 WHERE t.id = $1`, tenantID).Scan(&suspended)
	if err != nil {
		return false // fail open — if we can't check, allow access
	}
	return suspended
}

// extractClientIP returns the real client IP. All proxy-header trust rules
// live in internal/clientip (last X-Forwarded-For entry = the peer HAProxy
// appended; CF-Connecting-IP only behind a Cloudflare edge) — see R1-01.
func extractClientIP(r *http.Request) string {
	return clientip.FromRequest(r)
}
