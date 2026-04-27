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

1. `handleS3Request` calls `auth.ValidateRequest(r)` which parses the Authorization header
2. On failure, `authErrorHint` maps the error to a hint, and `WriteS3ErrorWithContext` returns it
3. On success, tenant context is set and the request is routed to the operation handler

## Tenant Context

Most handlers use `tenant.FromContext(r.Context())` to get the authenticated tenant. The `S3Request.TenantID` field is also set for convenience.
