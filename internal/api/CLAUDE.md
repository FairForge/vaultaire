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
- **s3_mfa_delete.go** — MFA Delete enforcement (`checkMFADelete` helper, `errMFARequired` sentinel)
- **s3_multipart.go** — Multipart upload lifecycle
- **s3_notifications.go** — Bucket notification configuration
- **s3_copy.go** — CopyObject handler
- **s3_list.go** — ListObjectsV2 handler
- **s3_presign.go** — Pre-signed URL verification (SigV4 query string auth) and URL generation
- **presigned.go** — Management API endpoint for generating pre-signed URLs (`/api/v1/presigned`)
- **sts_routes.go** — STS temporary credential endpoint (`POST /api/v1/sts/token`)
- **cdn_analytics.go** — CDNAnalyticsTracker: buffered CDN access event writer (Record/Flush/CheckBudget) + hourly rollup
- **management.go** — JSON response helpers: `writeJSON`, `writeListResponse`, `getRequestID` (Stripe-style envelope)
- **management_errors.go** — 7 typed errors with Stripe-style error envelope (`writeManagementError`)
- **management_routes.go** — RESTful JSON management API under `/api/v1/manage/` (buckets CRUD, objects list, keys CRUD, usage)
- **management_ratelimit.go** — Per-tenant rate limiter middleware (100 req/min, token bucket, X-RateLimit-* headers)
- **idempotency.go** — `Idempotency-Key` header middleware for management API (24h cache, replay with `Idempotency-Replayed: true`)
- **metadata.go** — S3 user metadata extraction/validation, Stripe-style merge for management API PATCH
- **events.go** — Event emitter (`emitEvent`), webhook dispatch, HMAC signing, `GET /api/v1/events` list endpoint
- **webhooks_routes.go** — Webhook CRUD API (`/api/v1/webhooks`): create, list, update, delete, delivery history, test fire
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

## API Versioning (Phase 5.11.1)

Every response includes `X-Vaultaire-Version: YYYY-MM-DD` (Stripe-style date versioning). The `APIVersion` const in `server.go` is the source of truth. `versionMiddleware` sets the header on all responses and logs the client's version at Debug level if sent. No version translation logic yet — just header plumbing.

## Idempotency Keys (Phase 5.11.2)

`Idempotency-Key` header on PUT/POST/DELETE management API requests. Opt-in — absent header passes through. Max 256 chars. Cached in `idempotency_cache` table (24h TTL, hourly cleanup goroutine). Only 2xx responses are cached. Replayed responses include `Idempotency-Replayed: true` header. Reusing a key with different method/path returns 409 `idempotency_key_reuse`. GET/HEAD/OPTIONS are always skipped. If db is nil, middleware passes through silently.

## Metadata on Resources (Phase 5.11.3)

User-defined metadata on objects and buckets. Two paths:

**S3 protocol**: `x-amz-meta-*` headers on PUT are extracted (prefix stripped, keys lowercased), validated (max 50 keys, 500 chars/value, 2KB total), stored in `object_head_cache.metadata` JSONB. Returned as `x-amz-meta-*` headers on GET and HEAD.

**Management API**: `GET /api/v1/manage/buckets/{name}` includes `metadata` field. `PATCH /api/v1/manage/buckets/{name}` accepts `{"metadata": {...}}` with Stripe-style merge semantics (null value deletes key).

Key files: `metadata.go` (extract/set/validate/merge helpers), `s3_engine_adapter.go` (PUT stores, GET returns), `s3.go` (HEAD returns), `management_routes.go` (PATCH handler).

**sqlmock note**: JSONB columns must use `[]byte` (not `string`) in `AddRow` to match real PostgreSQL driver behavior when scanning into `[]byte`/`json.RawMessage`.

## STS Temporary Credentials (Phase 5.11.5)

`POST /api/v1/sts/token` (JWT-protected) mints short-lived S3 credentials. Response: `{"object": "sts_token", "access_key": "ASIA...", "secret_key": "...", "expiration": "...", "request_id": "..."}`.

Scope intersection: requested permissions/buckets are intersected with the parent key's scope (JWT user's first API key, or full access if none). STS tokens can never escalate beyond the parent. IP restrictions are narrowed (request can only restrict further, not broaden).

S3 auth: both `validateAccessKey` (handlers.go) and `verifyPresignedURL` (s3_presign.go) have an ASIA-prefix fallback that queries `sts_tokens` after checking `tenants` and `api_keys`. Expired tokens are rejected. Cleanup: hourly goroutine in `auth.StartSTSCleanup`.

## Event Log + Webhook Management API (Phase 5.11.6)

Persistent event log + webhook CRUD API for SaaS developer integrations. Three new tables: `events`, `webhook_endpoints`, `webhook_deliveries` (migration 033).

**Event types**: `object.created`, `object.deleted`, `object.downloaded`, `bucket.created`, `bucket.deleted`, `key.created`, `key.revoked`, `sts.token_created`, `webhook.test`.

**`emitEvent(ctx, db, logger, eventType, tenantID, data)`** — inserts event row synchronously, dispatches webhooks asynchronously in a goroutine. Nil-safe on db. Wired into S3 handlers (PUT/GET/DELETE), bucket create/delete, key create/revoke, and STS token creation.

**Webhook dispatch**: queries `webhook_endpoints` for tenant, filters by `event_filter` (exact match, `object.*` wildcards, `*` catch-all). Delivers via HTTP POST with `X-Webhook-Signature: sha256=<hmac-hex>` header. Records delivery in `webhook_deliveries` (status, response_code, latency_ms). No retry loop in this phase — `next_retry_at` column reserved for future use.

**Endpoints** (JWT-protected, rate-limited):
- `GET /api/v1/events` — cursor pagination (created_at), type filter, tenant-scoped
- `POST /api/v1/webhooks` — create (returns `whsec_` secret, only time it's exposed)
- `GET /api/v1/webhooks` — list (secret omitted)
- `PATCH /api/v1/webhooks/{id}` — partial update (url, events, enabled)
- `DELETE /api/v1/webhooks/{id}` — cascade deletes deliveries
- `GET /api/v1/webhooks/{id}/deliveries` — delivery history with cursor pagination
- `POST /api/v1/webhooks/{id}/test` — fires synthetic `webhook.test` event

## Free Tier Quota Enforcement (Phase 5.11.10)

`handlePutObject` (s3.go) checks `quotaManager.CheckAndReserve()` before writing. Returns `QuotaExceeded` (403) with upgrade suggestion if quota is full. Size is read from `Content-Length` or `x-amz-decoded-content-length` (chunked uploads).

`CreateBucket` (s3_buckets.go) checks the tenant's tier via `quotaManager.GetTier()`. Free tier tenants are limited to `usage.FreeTierLimits.MaxBuckets` (1). Returns `QuotaExceeded` with bucket-specific suggestion.

Error code `ErrQuotaExceeded` in `s3_errors.go` — 403 status, message includes upgrade hint via `WithSuggestion`.

## Auth Flow

1. `handleS3Request` checks `isPresignedRequest(r)` — if query has `X-Amz-Algorithm=AWS4-HMAC-SHA256`, routes to `verifyPresignedURL` (SigV4 query string auth)
2. Otherwise calls `auth.ValidateRequest(r)` which parses the Authorization header
3. Both paths return `(tenantID, *auth.KeyScope, error)` — scope carries permissions, bucket restrictions, IP allowlist, and expiration. Auth lookup order: tenants (primary key) → api_keys (VLT_ scoped) → sts_tokens (ASIA temporary)
4. On failure, returns appropriate S3 error (presigned errors: `ExpiredToken`, `AuthorizationQueryParametersError`, `SignatureDoesNotMatch`)
5. On success, scope enforcement runs before operation routing:
   - `IsKeyExpired` → 403 `ExpiredToken`
   - `CheckIPAllowlist` with `extractClientIP(r)` (CF-Connecting-IP > X-Forwarded-For > RemoteAddr) → 403 `AccessDenied`
   - After `ParseRequest`: `CheckPermission` and `CheckBucketScope` → 403 `AccessDenied` with suggestion hint
6. Tenant context is set and the request is routed to the operation handler

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

## CDN Access Analytics (Phase 5.11.12)

`CDNAnalyticsTracker` in `cdn_analytics.go` buffers CDN access events in memory and flushes them to `cdn_access_log` every 5 seconds (or at 100 events). Follows the same pattern as `BandwidthTracker`. Initialized in `NewServer()`, nil-safe throughout.

- **Record()** — called from `handleCDNRequest` after successful GET (both full and range). Captures tenant, bucket, key, bytes, country (CF-IPCountry header), referer. Skips HEAD/OPTIONS.
- **CheckBudget()** — called before object lookup in `handleCDNRequest`. Queries `buckets.bandwidth_budget_bytes` and `cdn_stats_daily` for current month total (plus in-memory buffer). Returns 429 with `Retry-After: 3600` if exceeded. Budget of 0 = unlimited.
- **StartRollup()** — hourly goroutine aggregates `cdn_access_log` into `cdn_stats_daily` (requests, bytes_sent, unique_objects per tenant/bucket/date).

Dashboard analytics page at `/dashboard/buckets/{name}/analytics` (handler in `internal/dashboard/handlers/bucket_analytics.go`). Shows 24h/7d/30d downloads + bandwidth, top objects, geographic breakdown, budget gauge. Only linked from bucket objects page for public-read buckets.

Tables: `cdn_access_log`, `cdn_stats_daily` (migration 035).

## MFA Delete (Phase 5.14.3)

S3-compatible MFA Delete enforcement for Object Lock buckets. When `mfa_delete_enabled = TRUE` on a bucket, DELETE operations and versioning suspension require a valid TOTP code in the `x-amz-mfa` header.

**Header format**: `x-amz-mfa: <serial-or-anything> <6-digit-totp-code>` — the serial portion is ignored (tenant already authenticated via S3 credentials); only the last space-separated token is used as the TOTP code.

**`checkMFADelete(ctx, db, authSvc, mfaSvc, tenantID, bucket, r)`** — called from Server-level handlers (not the adapter) to validate MFA before destructive operations. Returns nil if MFA Delete is not enabled or verification succeeds.

**Wiring**:
- `handleDeleteObject` (s3.go) — checks before creating the adapter (Option B: minimal blast radius)
- `handlePutBucketVersioning` (s3_versioning.go) — enforces MFA when enabling/disabling MFA Delete or suspending versioning on an MFA-enabled bucket
- `handleGetBucketVersioning` (s3_versioning.go) — returns `<MfaDelete>Enabled</MfaDelete>` in XML when set

**Constraints**: MFA Delete can only be enabled on buckets with `object_lock_enabled = TRUE`. Attempting to enable on a non-lock bucket returns `InvalidBucketState` (409).

**Error types**: `errMFARequired` (sentinel, maps to AccessDenied), `mfaNotConfiguredError` (MFA not set up on the user account).

Table: `buckets.mfa_delete_enabled` (migration 036).

## SSE-S3 Server-Side Encryption (Phase 5.14.4)

Transparent server-side encryption at rest using ML-KEM-768 (post-quantum key encapsulation) + AES-256-GCM. Activated by setting `ENCRYPTION_MASTER_KEY` env var.

**Activation**: per-request via `x-amz-server-side-encryption: AES256` header, or per-bucket via `sse_enabled` column (defaults to TRUE for new buckets when SSE is available).

**PUT flow** (s3_engine_adapter.go HandlePut):
1. Check `shouldEncrypt`: sseService != nil AND (header present OR bucket sse_enabled) AND size > 0 AND size <= 256 MiB
2. EnsureTenantKey (idempotent — creates ML-KEM-768 keypair if missing)
3. ReadAll plaintext through TeeReader (computes ETag on plaintext)
4. EncryptBytes → ciphertext (plaintext + 1117B overhead)
5. engine.Put with ciphertext; object_head_cache stores plaintext size + encryption_algorithm

**GET flow** (s3_engine_adapter.go HandleGet):
1. Query encryption_algorithm from object_head_cache
2. engine.Get → encrypted blob
3. If encrypted: ReadAll + DecryptBytes → plaintext reader
4. Serve plaintext (range requests work on decrypted data)

**HEAD flow** (s3.go handleHeadObject):
- Returns plaintext size from cache + `x-amz-server-side-encryption: AES256` header

**Key files**: `internal/crypto/sse_s3.go` (service), `internal/crypto/CLAUDE.md` (crypto docs)

**Tables**: `tenant_encryption_keys` (keypairs), `buckets.sse_enabled`, `object_head_cache.encryption_algorithm` (migration 037)

## Tenant Context

Most handlers use `tenant.FromContext(r.Context())` to get the authenticated tenant. The `S3Request.TenantID` field is also set for convenience.
