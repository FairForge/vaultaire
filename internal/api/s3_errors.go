package api

import (
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// S3Error represents an S3 error response
type S3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId,omitempty"`
}

// S3 Error codes
const (
	ErrNoSuchBucket           = "NoSuchBucket"
	ErrNoSuchKey              = "NoSuchKey"
	ErrBucketAlreadyExists    = "BucketAlreadyExists"
	ErrBucketNotEmpty         = "BucketNotEmpty"
	ErrInvalidBucketName      = "InvalidBucketName"
	ErrInvalidObjectName      = "InvalidObjectName"
	ErrAccessDenied           = "AccessDenied"
	ErrInvalidRequest         = "InvalidRequest"
	ErrIncompleteBody         = "IncompleteBody"
	ErrInternalError          = "InternalError"
	ErrNotImplemented         = "NotImplemented"
	ErrMissingContentLength   = "MissingContentLength"
	ErrRequestTimeout         = "RequestTimeout"
	ErrBadDigest              = "BadDigest"
	ErrEntityTooLarge         = "EntityTooLarge"
	ErrMalformedXML           = "MalformedXML"
	ErrMethodNotAllowed       = "MethodNotAllowed"
	ErrSignatureDoesNotMatch  = "SignatureDoesNotMatch"
	ErrAccountSuspended       = "AccountSuspended"
	ErrSlowDown               = "SlowDown"
	ErrNoSuchUpload           = "NoSuchUpload"
	ErrInvalidPart            = "InvalidPart"
	ErrInvalidPartOrder       = "InvalidPartOrder"
	ErrEntityTooSmall         = "EntityTooSmall"
	ErrInvalidPartNumber      = "InvalidPartNumber"
	ErrNoSuchVersion          = "NoSuchVersion"
	ErrObjectLocked           = "ObjectLocked"
	ErrInvalidRetentionPeriod = "InvalidRetentionPeriod"
)

// Error messages
var errorMessages = map[string]string{
	ErrNoSuchBucket:           "The specified bucket does not exist",
	ErrNoSuchKey:              "The specified key does not exist",
	ErrBucketAlreadyExists:    "The requested bucket name is not available",
	ErrBucketNotEmpty:         "The bucket you tried to delete is not empty",
	ErrInvalidBucketName:      "The specified bucket is not valid",
	ErrInvalidObjectName:      "The specified object name is not valid",
	ErrAccessDenied:           "Access denied",
	ErrInvalidRequest:         "Invalid request",
	ErrIncompleteBody:         "You did not provide the number of bytes specified by the Content-Length HTTP header",
	ErrInternalError:          "We encountered an internal error. Please try again",
	ErrNotImplemented:         "A feature you requested is not yet implemented",
	ErrMissingContentLength:   "You must provide the Content-Length HTTP header",
	ErrRequestTimeout:         "Your socket connection to the server was not read from or written to within the timeout period",
	ErrBadDigest:              "The Content-MD5 you specified did not match what we received",
	ErrEntityTooLarge:         "Your proposed upload exceeds the maximum allowed size",
	ErrMalformedXML:           "The XML you provided was not well-formed or did not validate against our published schema",
	ErrMethodNotAllowed:       "The specified method is not allowed against this resource",
	ErrSignatureDoesNotMatch:  "The request signature we calculated does not match the signature you provided",
	ErrAccountSuspended:       "Your account has been suspended. Contact support for assistance.",
	ErrSlowDown:               "Monthly bandwidth limit exceeded. Upgrade your plan or wait for the next billing cycle.",
	ErrNoSuchUpload:           "The specified multipart upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.",
	ErrInvalidPart:            "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.",
	ErrInvalidPartOrder:       "The list of parts was not in ascending order. The parts list must be specified in order by part number.",
	ErrEntityTooSmall:         "Your proposed upload is smaller than the minimum allowed object size. Each part must be at least 5 MB in size, except the last part.",
	ErrInvalidPartNumber:      "Part number must be an integer between 1 and 10000, inclusive.",
	ErrNoSuchVersion:          "The version ID specified in the request does not match an existing version.",
	ErrObjectLocked:           "Object is protected by Object Lock",
	ErrInvalidRetentionPeriod: "The retention period specified is not valid",
}

// HTTP status codes for errors
var errorStatusCodes = map[string]int{
	ErrNoSuchBucket:           http.StatusNotFound,
	ErrNoSuchKey:              http.StatusNotFound,
	ErrBucketAlreadyExists:    http.StatusConflict,
	ErrBucketNotEmpty:         http.StatusConflict,
	ErrInvalidBucketName:      http.StatusBadRequest,
	ErrInvalidObjectName:      http.StatusBadRequest,
	ErrAccessDenied:           http.StatusForbidden,
	ErrInvalidRequest:         http.StatusBadRequest,
	ErrIncompleteBody:         http.StatusBadRequest,
	ErrInternalError:          http.StatusInternalServerError,
	ErrNotImplemented:         http.StatusNotImplemented,
	ErrMissingContentLength:   http.StatusLengthRequired,
	ErrRequestTimeout:         http.StatusRequestTimeout,
	ErrBadDigest:              http.StatusBadRequest,
	ErrEntityTooLarge:         http.StatusRequestEntityTooLarge,
	ErrMalformedXML:           http.StatusBadRequest,
	ErrMethodNotAllowed:       http.StatusMethodNotAllowed,
	ErrSignatureDoesNotMatch:  http.StatusForbidden,
	ErrAccountSuspended:       http.StatusForbidden,
	ErrSlowDown:               http.StatusTooManyRequests,
	ErrNoSuchUpload:           http.StatusNotFound,
	ErrInvalidPart:            http.StatusBadRequest,
	ErrInvalidPartOrder:       http.StatusBadRequest,
	ErrEntityTooSmall:         http.StatusBadRequest,
	ErrInvalidPartNumber:      http.StatusBadRequest,
	ErrNoSuchVersion:          http.StatusNotFound,
	ErrObjectLocked:           http.StatusForbidden,
	ErrInvalidRetentionPeriod: http.StatusBadRequest,
}

// WriteS3Error writes an S3-compatible error response
func WriteS3Error(w http.ResponseWriter, code string, resource string, requestID string) {
	message, exists := errorMessages[code]
	if !exists {
		message = "Unknown error"
		code = ErrInternalError
	}

	statusCode, exists := errorStatusCodes[code]
	if !exists {
		statusCode = http.StatusInternalServerError
	}

	errResp := S3Error{
		Code:      code,
		Message:   message,
		Resource:  resource,
		RequestID: requestID,
	}

	xmlData, err := xml.MarshalIndent(errResp, "", "  ")
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(statusCode)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(xmlData)
}

// ErrorContext carries optional context for enriched error messages.
type ErrorContext struct {
	Suggestion string
}

// ErrorOption configures an ErrorContext.
type ErrorOption func(*ErrorContext)

// WithSuggestion appends a helpful suggestion to the error message.
func WithSuggestion(s string) ErrorOption {
	return func(ec *ErrorContext) { ec.Suggestion = s }
}

// WriteS3ErrorWithContext writes an S3-compatible error response with optional
// enrichment (e.g. Levenshtein-based "Did you mean?" suggestions). Call sites
// that don't need suggestions can omit the opts — behavior is identical to
// WriteS3Error in that case.
func WriteS3ErrorWithContext(w http.ResponseWriter, code string, resource string, requestID string, opts ...ErrorOption) {
	ec := &ErrorContext{}
	for _, o := range opts {
		o(ec)
	}

	message, exists := errorMessages[code]
	if !exists {
		message = "Unknown error"
		code = ErrInternalError
	}

	if ec.Suggestion != "" {
		if !strings.HasSuffix(message, ".") {
			message += "."
		}
		message += " " + ec.Suggestion
	}

	statusCode, exists := errorStatusCodes[code]
	if !exists {
		statusCode = http.StatusInternalServerError
	}

	errResp := S3Error{
		Code:      code,
		Message:   message,
		Resource:  resource,
		RequestID: requestID,
	}

	xmlData, err := xml.MarshalIndent(errResp, "", "  ")
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(statusCode)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(xmlData)
}

// levenshtein computes the edit distance between two strings using the
// Wagner-Fischer algorithm. O(len(a)*len(b)) time and O(min(len(a),len(b))) space.
func levenshtein(a, b string) int {
	if len(a) < len(b) {
		a, b = b, a
	}
	if len(b) == 0 {
		return len(a)
	}

	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			ins := curr[j-1] + 1
			del := prev[j] + 1
			sub := prev[j-1] + cost
			curr[j] = min(ins, min(del, sub))
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

const maxSuggestionDistance = 3

// bucketSuggestion queries the tenant's own buckets and returns a "Did you
// mean 'X'?" string if a close match exists. Returns "" if no match is close
// enough or if db is nil. Only considers the requesting tenant's buckets —
// never leaks cross-tenant information.
func bucketSuggestion(ctx context.Context, db *sql.DB, tenantID, bucket string) string {
	if db == nil {
		return ""
	}

	rows, err := db.QueryContext(ctx,
		`SELECT name FROM buckets WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return ""
	}
	defer func() { _ = rows.Close() }()

	bestName := ""
	bestDist := maxSuggestionDistance + 1

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		d := levenshtein(bucket, name)
		if d < bestDist {
			bestDist = d
			bestName = name
		}
	}

	if bestName != "" && bestDist <= maxSuggestionDistance {
		return fmt.Sprintf("Did you mean '%s'?", bestName)
	}
	return ""
}

// keySuggestion queries object_head_cache for keys in the same bucket that
// share a common prefix and returns a "Did you mean 'X'?" string if a close
// match exists. The query is bounded (LIMIT 20) to keep it cheap.
func keySuggestion(ctx context.Context, db *sql.DB, tenantID, bucket, key string) string {
	if db == nil {
		return ""
	}

	// Use the key's directory prefix to narrow the search.
	prefix := ""
	if idx := strings.LastIndex(key, "/"); idx >= 0 {
		prefix = key[:idx+1]
	}

	var rows *sql.Rows
	var err error
	if prefix != "" {
		rows, err = db.QueryContext(ctx, `
			SELECT object_key FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key LIKE $3
			LIMIT 20`, tenantID, bucket, prefix+"%")
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT object_key FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2
			LIMIT 20`, tenantID, bucket)
	}
	if err != nil {
		return ""
	}
	defer func() { _ = rows.Close() }()

	bestKey := ""
	bestDist := maxSuggestionDistance + 1

	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			continue
		}
		d := levenshtein(key, k)
		if d < bestDist {
			bestDist = d
			bestKey = k
		}
	}

	if bestKey != "" && bestDist <= maxSuggestionDistance {
		return fmt.Sprintf("Did you mean '%s'?", bestKey)
	}
	return ""
}

// authErrorHint returns additional context for common auth failure reasons.
// The returned string is appended to the base error message, so it should not
// repeat "Access denied".
func authErrorHint(errMsg string) string {
	switch {
	case strings.Contains(errMsg, "missing authorization"):
		return "No authorization header provided."
	case strings.Contains(errMsg, "invalid access key"):
		return "The AWS access key ID you provided does not exist in our records."
	case strings.Contains(errMsg, "invalid authorization format"):
		return "Invalid signature — check your secret key and signing method."
	default:
		return ""
	}
}

// generateRequestID creates a unique request ID
func generateRequestID() string {
	return fmt.Sprintf("%d-%d", time.Now().Unix(), time.Now().Nanosecond())
}
