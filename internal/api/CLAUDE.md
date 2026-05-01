# internal/api

S3-compatible API layer. Translates S3 protocol to engine operations, handles auth, tenant isolation, and error responses.

## Key Files

- **server.go** — Server struct, router setup, middleware chain
- **s3.go** — Request parsing, auth, routing to operation handlers
- **s3_errors.go** — Error codes, messages, and response writing
- **s3_engine_adapter.go** — GET/PUT/DELETE/LIST handlers bridging S3 to engine
- **s3_buckets.go** — CreateBucket, DeleteBucket, ListBuckets
- **s3_versioning.go** — Bucket versioning enable/suspend
- **s3_lock.go** — Object Lock, retention, legal hold
- **s3_multipart.go** — Multipart upload lifecycle
- **s3_notifications.go** — Bucket notification configuration
- **s3_copy.go** — CopyObject handler
- **s3_list.go** — ListObjectsV2 handler
- **s3_presign.go** — Pre-signed URL verification (SigV4 query string auth) and URL generation
- **presigned.go** — Management API endpoint for generating pre-signed URLs (`/api/v1/presigned`)
- **management.go** — JSON response helpers: `writeJSON`, `writeListResponse`, `getRequestID` (Stripe-style envelope)
- **management_errors.go** — 7 typed errors with Stripe-style error envelope (`writeManagementError`)
- **management_routes.go** — RESTful JSON management API under `/api/v1/manage/` (buckets CRUD, objects list, keys CRUD, usage)
- **management_ratelimit.go** — Per-tenant rate limiter middleware (100 req/min, token bucket, X-RateLimit-* headers)
- **llms_txt.go** — Static `/llms.txt` endpoint (plain-text API summary for LLMs)

## Error Response Pattern

Two functions for writing S3 error XML:

- `WriteS3Error(w, code, resource, requestID)` — static message from `errorMessages` map
- `WriteS3ErrorWithContext(w, code, resource, requestID, opts...)` — same but accepts `ErrorOption` functional options for enrichment

### Friendly Suggestions (Phase 5.10.15)

**NoSuchBucket** — call `bucketSuggestion(ctx, db, tenantID, bucket)` to find the closest Levenshtein match among the tenant's own buckets. Only suggests if distance <= 3.

**NoSuchKey** — call `keySuggestion(ctx, db, tenantID, bucket, key)` to find close matches in `object_head_cache`. Bounded to LIMIT 20 keys with matching prefix.

**AccessDenied** — `authErrorHint(errMsg)` maps common auth failures to user-friendly hints (missing header, invalid key, bad signature format).

Usage at call sites:
```go
reqID := generateRequestID()
if suggestion := bucketSuggestion(ctx, s.db, tenantID, bucket); suggestion != "" {
    WriteS3ErrorWithContext(w, ErrNoSuchBucket, r.URL.Path, reqID, WithSuggestion(suggestion))
} else {
    WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, reqID)
}
```

### Security: Cross-Tenant Isolation

`bucketSuggestion` only queries `WHERE tenant_id = $1` — never leaks bucket names across tenants. A bucket owned by tenant A is invisible to tenant B's suggestion query.

## Auth Flow

1. `handleS3Request` checks `isPresignedRequest(r)` — if query has `X-Amz-Algorithm=AWS4-HMAC-SHA256`, routes to `verifyPresignedURL` (SigV4 query string auth)
2. Otherwise calls `auth.ValidateRequest(r)` which parses the Authorization header
3. On failure, returns appropriate S3 error (presigned errors: `ExpiredToken`, `AuthorizationQueryParametersError`, `SignatureDoesNotMatch`)
4. On success, tenant context is set and the request is routed to the operation handler

### Pre-Signed URL Verification (Phase 5.10.16)

`verifyPresignedURL` validates S3 SigV4 query-string auth for browser-direct uploads:
- Parses X-Amz-Credential to extract access key, then looks up secret key + tenant ID from `tenants` table
- Validates expiration (X-Amz-Date + X-Amz-Expires), rejects expired requests
- Rebuilds canonical request (excludes X-Amz-Signature from query string, uses UNSIGNED-PAYLOAD)
- Computes signature and compares with `hmac.Equal` (constant-time)

Management API at `/api/v1/presigned` (JWT-protected) generates pre-signed URLs for dashboard users without needing an AWS SDK.

## Management API (Phase 5.11.0)

JSON REST layer at `/api/v1/manage/` with JWT auth and per-tenant rate limiting. Separate from S3 XML wire protocol.

**Response envelope** (Stripe-style):
- Single: `{"object": "bucket", "name": "...", "request_id": "..."}`
- List: `{"object": "list", "data": [...], "has_more": bool, "next_cursor": "...", "total_count": N, "request_id": "..."}`
- Error: `{"error": {"type": "invalid_request_error", "code": "...", "message": "...", "param": "...", "request_id": "..."}}`

**Error types**: `invalid_request_error` (400), `authentication_error` (401), `permission_error` (403), `not_found_error` (404), `conflict_error` (409), `rate_limit_error` (429), `api_error` (500).

**Rate limiting**: `ManagementRateLimiter` — per-tenant token bucket (100 req/min), separate from the CDN per-IP limiter. Evicts if >10K tenants. Sets `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset` headers; returns `Retry-After` on 429.

**Cursor pagination**: queries `LIMIT N+1`; if N+1 results → `has_more=true`, returns first N, `next_cursor` = last item's name/key.

## Tenant Context

Most handlers use `tenant.FromContext(r.Context())` to get the authenticated tenant. The `S3Request.TenantID` field is also set for convenience.
