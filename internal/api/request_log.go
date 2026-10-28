package api

// The developers' request log (WP-LOG-1, Stage 1a Prompt 49; plan §
// WP-STRIPE): every S3 request of the last 7 days, for every tenant, on the
// dashboard's Developers page and GET /api/v1/manage/requests — the time,
// operation, bucket, key, status, error code, the code's suggestion, the
// request id the client was answered (what a support ticket quotes), the
// bytes and the host that served it.
//
// One write path: the access-log tracker's second sink (access_log.go). The
// S3 dispatcher records every request whose credential resolved a tenant —
// refusals after authentication included — once, in a deferred write
// (s3.go), with the error code and request id the response writer kept
// (noteS3Error, s3_errors.go). The row holds nothing else: never a body, a
// header, an IP address, a user agent, an SSE-C key or any secret
// (TestRequestLog_NeverHoldsAnSSECKeyOrASecret). The writer is behind the
// request_log flag (default ON) so a bad write path can be switched off.
//
// The suggestion is stored once per error code (request_suggestions) and
// joined on read. It is the code's GENERIC advice below — never the
// response's own suggestion, which may name one tenant's bucket or key
// ("Did you mean …?") while request_suggestions is every tenant's table.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/requestlog"
)

// requestSuggestions is the suggestion shown beside each S3 error code on
// the Developers page: what a developer can do about it. Every code the S3
// API answers has one (TestRequestLog_EveryErrorCarriesItsSuggestion).
var requestSuggestions = map[string]string{
	ErrNoSuchBucket:                      "Check the bucket name and that it belongs to this account (bucket names are per account).",
	ErrNoSuchKey:                         "Check the key: keys are case-sensitive and include every path segment.",
	ErrBucketAlreadyExists:               "This account already has a bucket of that name: pick another name, or use the existing bucket.",
	ErrBucketNotEmpty:                    "Delete the bucket's objects (and their versions, for a versioned bucket) before the bucket.",
	ErrInvalidBucketName:                 "Bucket names are 3-63 characters of lower-case letters, digits, dots and hyphens; some names are reserved.",
	ErrInvalidObjectName:                 "Use a key that is valid UTF-8 and does not contain characters the API refuses.",
	ErrAccessDenied:                      "The key may lack this operation or bucket, be limited to some IP addresses, or the object may be locked: check the key under API Keys.",
	ErrInvalidAccessKeyId:                "The access key id is unknown, revoked or rotated: use a current key from API Keys.",
	ErrInvalidRequest:                    "The request's combination of headers or parameters is not accepted: read the error message for which.",
	ErrIncompleteBody:                    "The body ended before its Content-Length: retry the upload.",
	ErrInternalError:                     "Our error: retry with backoff, and quote the request id to support if it persists.",
	ErrNotImplemented:                    "This S3 operation or header is not supported yet: see the API reference for what is.",
	ErrMissingContentLength:              "Send a Content-Length header (or an aws-chunked body with its decoded length).",
	ErrRequestTimeout:                    "The body arrived too slowly: retry, and check the client's network or upload concurrency.",
	ErrBadDigest:                         "The Content-MD5 does not match the body: the body changed in transit, or the MD5 was computed over other bytes.",
	ErrInvalidDigest:                     "The Content-MD5 header is not a valid base64 MD5.",
	ErrEntityTooLarge:                    "The object or part is larger than allowed: upload it as a multipart upload, parts under 95 MB through the edge.",
	ErrMalformedXML:                      "The XML body does not parse or does not match the operation's schema.",
	ErrMethodNotAllowed:                  "This HTTP method is not allowed on this resource.",
	ErrSignatureDoesNotMatch:             "Check the secret key, the region (us-east-1) and that nothing rewrites the request (a proxy changing headers or the path).",
	ErrRequestTimeTooSkewed:              "The client's clock is more than 15 minutes off: sync it (NTP) and retry.",
	ErrXAmzContentSHA256Mismatch:         "The x-amz-content-sha256 header does not match the body: the body changed after it was signed.",
	ErrInvalidArgument:                   "A parameter or header has a value the API does not accept: read the error message for which.",
	ErrKeyTooLong:                        "Shorten the key: it is longer than this bucket's storage accepts.",
	ErrAccountSuspended:                  "The account is suspended: contact support.",
	ErrCORSForbidden:                     "The browser's origin or method is not allowed by the bucket's CORS rules.",
	ErrNoSuchCORSConfiguration:           "The bucket has no CORS configuration.",
	ErrSlowDown:                          "Too many requests at once, or past the egress allowance: retry after the Retry-After header, with fewer in flight.",
	ErrNoSuchUpload:                      "The multipart upload id is unknown, completed or aborted (uploads idle 48 hours are aborted): start a new upload.",
	ErrInvalidPart:                       "A part listed in CompleteMultipartUpload is missing or its ETag does not match: list the parts and retry.",
	ErrInvalidPartOrder:                  "List the parts in ascending part-number order.",
	ErrEntityTooSmall:                    "Every part but the last must be at least 5 MiB.",
	ErrXAmzContentChecksumMismatch:       "The checksum header does not match the body: the body changed in transit.",
	ErrInvalidPartNumber:                 "Part numbers run from 1 to 10,000.",
	ErrNoSuchVersion:                     "The version id is unknown for this key: list the object's versions.",
	ErrAccessControlListNotSupported:     "ACLs are not supported: use the bucket's visibility (private or public) instead.",
	ErrInvalidRetentionPeriod:            "Give a retention date in the future, within the bucket's Object Lock rules.",
	ErrExpiredPresignedRequest:           "The presigned link or the key has expired: sign a new link, or rotate the key.",
	ErrAuthorizationQueryParametersError: "The presigned link's query parameters are incomplete or malformed: sign it again with the SDK.",
	ErrInvalidPresignExpires:             "A presigned link may live at most 7 days: sign it with a shorter expiry.",
	ErrQuotaExceeded:                     "The account's quota is full: delete objects (and abort unfinished uploads), or resize the quota under Billing.",
	ErrServiceUnavailable:                "A storage backend is unavailable right now: retry after the Retry-After header.",
	ErrInsufficientStorage:               "The storage behind this bucket is full: retry later, or write to another bucket.",
	ErrInvalidBucketState:                "The bucket's state does not allow this (versioning or Object Lock settings): read the error message.",
	ErrPreconditionFailed:                "An If-Match / If-None-Match / If-Modified-Since condition did not hold: re-read the object and retry.",
	ErrInvalidLocationConstraint:         "That region is not available to this account: choose one listed on the bucket form.",
	ErrInvalidTag:                        "Tags are at most 10 per object, keys up to 128 and values up to 256 characters.",
	ErrInvalidObjectState:                "The object is archived: restore it first (RestoreObject), then read it.",
	ErrRestoreAlreadyInProgress:          "A restore of this object is already running: wait for it to finish.",
}

// requestSuggestion is the code's suggestion ("" for an unknown code).
func requestSuggestion(code string) string { return requestSuggestions[code] }

// requestLogInsertSQL is the ONE statement that writes request_log: the
// columns it names are everything a row may hold.
const requestLogInsertSQL = `
	INSERT INTO request_log (tenant_id, key_id, logged_at, op, bucket, object_key, status, error_code, request_id, bytes_in, bytes_out, host)
	SELECT * FROM unnest($1::text[], $2::text[], $3::timestamptz[], $4::text[], $5::text[], $6::text[],
	                     $7::smallint[], $8::text[], $9::text[], $10::bigint[], $11::bigint[], $12::text[])`

// writeRequestLog inserts a batch of events in one statement, then the
// suggestion of each error code this process has not stored yet.
func (at *S3AccessLogTracker) writeRequestLog(ctx context.Context, events []s3AccessEvent) error {
	n := len(events)
	tenant, key, at_, op, bucket, obj := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	status, code, reqID, in, out, host := make([]int64, n), make([]string, n), make([]string, n), make([]int64, n), make([]int64, n), make([]string, n)
	codes := map[string]bool{}
	for i, e := range events {
		tenant[i], key[i], at_[i], op[i], bucket[i], obj[i] = e.tenantID, e.keyID, e.loggedAt.UTC().Format(time.RFC3339Nano), e.operation, e.bucket, e.objectKey
		status[i], code[i], reqID[i], in[i], out[i], host[i] = int64(e.statusCode), e.errorCode, e.requestID, e.bytesReceived, e.bytesSent, e.host
		if e.errorCode != "" {
			codes[e.errorCode] = true
		}
	}
	if _, err := at.db.ExecContext(ctx, requestLogInsertSQL, pq.Array(tenant), pq.Array(key), pq.Array(at_), pq.Array(op), pq.Array(bucket),
		pq.Array(obj), pq.Array(status), pq.Array(code), pq.Array(reqID), pq.Array(in), pq.Array(out), pq.Array(host)); err != nil {
		return fmt.Errorf("write %d request log rows: %w", n, err)
	}
	for c := range codes {
		if _, done := at.suggested.Load(c); done {
			continue
		}
		s := requestSuggestion(c)
		if s == "" {
			continue
		}
		if _, err := at.db.ExecContext(ctx, `
			INSERT INTO request_suggestions (error_code, suggestion) VALUES ($1, $2)
			ON CONFLICT (error_code) DO UPDATE SET suggestion = EXCLUDED.suggestion
			WHERE request_suggestions.suggestion IS DISTINCT FROM EXCLUDED.suggestion`, c, s); err != nil {
			return fmt.Errorf("store the suggestion of %s: %w", c, err)
		}
		at.suggested.Store(c, true)
	}
	return nil
}

// handleMgmtRequests is GET /api/v1/manage/requests: the caller's tenant's
// request log, newest first — filters key, since (RFC 3339), status (403,
// 4xx, error), request_id; keyset paged by cursor; limit ≤ 200.
func (s *Server) handleMgmtRequests(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}
	q, msg := requestLogQueryFrom(r, tenantID)
	if msg != "" {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_parameter", msg, "")
		return
	}
	if s.db == nil {
		writeListResponse(w, nil, false, "", 0)
		return
	}
	rows, next, err := requestlog.Read(r.Context(), s.db, q)
	if err != nil {
		s.logger.Error("manage requests: read failed", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "the request log could not be read", "")
		return
	}
	items := make([]interface{}, len(rows))
	for i := range rows {
		items[i] = rows[i]
	}
	writeListResponse(w, items, next != "", next, 0)
}

// requestLogQueryFrom reads the API's filters ("" message = valid).
func requestLogQueryFrom(r *http.Request, tenantID string) (requestlog.Query, string) {
	v := r.URL.Query()
	q := requestlog.Query{TenantID: tenantID, KeyID: strings.TrimSpace(v.Get("key")), RequestID: strings.TrimSpace(v.Get("request_id"))}
	if s := v.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return q, "since must be an RFC 3339 time (2026-10-28T00:00:00Z)"
		}
		q.Since = t
	}
	var err error
	if q.StatusMin, q.StatusMax, err = requestlog.ParseStatus(v.Get("status")); err != nil {
		return q, err.Error()
	}
	if q.After, err = requestlog.ParseCursor(v.Get("cursor")); err != nil {
		return q, err.Error()
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 200 {
			return q, "limit must be 1-200"
		}
		q.Limit = n
	}
	return q, ""
}
