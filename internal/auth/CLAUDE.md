# internal/auth

Authentication service for Vaultaire. Handles user registration, login, JWT tokens, S3 credential validation, and API key management.

## Key Types

- **AuthService** — stateful service with in-memory maps for O(1) lookups. Backed by PostgreSQL for persistence.
- **User** — `{ID, Email, PasswordHash, Company, TenantID, EmailVerified, CreatedAt, UpdatedAt}`
- **Tenant** — `{ID, UserID, AccessKey, SecretKey, CreatedAt}` — the pair is the MIRROR of the live primary `api_keys` row (WP-R5-14); the in-memory `keyIndex[accessKey]` is built from live `api_keys` rows only
- **APIKey** — `{ID, UserID, TenantID, IsPrimary, Name, Key, Secret, Hash, Permissions, BucketScope, IPAllowlist, ExpiresAt, LastUsed, CreatedAt, RevokedAt, Metadata, UsageCount, LastIP}`
- **KeyScope** — `{Permissions, BucketScope, IPAllowlist, ExpiresAt}` — returned from auth lookups for scope enforcement
- **KeyCreateOptions** — optional scope params for `GenerateAPIKey`

## Audit trail (Review R11-09)

`GenerateAPIKey`, `RotateAPIKey`, `RevokeAPIKey`, `SetAPIKeyExpiration`, `ChangePassword`, `CompletePasswordReset`, `EnableMFA`, `DisableMFA` and `CreateUserWithTenant` each write one `audit_logs` row through `AuthService.record` → `internal/audit` (`key.created`, `key.rotated`, `key.revoked`, `key.expiry_set`, `auth.password_changed`, `auth.password_reset`, `mfa.enabled`, `mfa.disabled`, `account.created`). Writing here — not in the handlers — is what makes the dashboard, `/api/v1/user` and `/api/v1/manage` agree (the R4-22 lesson). The actor and client IP come from the request context (`audit.WithActor` / `audit.WithRequest`, set by `requireJWT`, the dashboard session middleware and `requestIDMiddleware`); an admin resetting another user's MFA is recorded as `performed_by = admin`, `user_id = subject`. Nil `sqlDB` = no row. The old in-memory `AuditLogger` and `GenerateAPIKeyWithAudit` were deleted in Review R15.

`ErrUnknownAccessKey` (`sigv4.go`) is what `LookupCredential` returns for an id that exists nowhere; `ErrAccessKeyRevoked` (`handlers.go`) for one that exists and is dead — a revoked or rotated key, or an STS token whose parent is (known-and-dead: the S3 layer answers `InvalidAccessKeyId` and counts it with `key_known="true"`, reason `revoked`). `AccessKeyFromRequest` extracts the presented id for the bounded `key_hash` label only.

## The credential lifecycle (WP-R5-14 / WP-R5-9 / WP-R5-5 / WP-R5-10, migration 076, `docs/reviews/WP-R5-14.md`)

**The `api_keys` row IS the credential.** Registration used to write the primary pair twice (`tenants.access_key/secret_key` and an `api_keys` row named `primary`); `RotateAPIKey`/`RevokeAPIKey` wrote `api_keys` only and `lookupCredential` read `tenants` FIRST with no revocation check, so a rotated primary pair kept authenticating for ever (found live by WP-R10-3b). Now:

- `api_keys.tenant_id` is on the row (WP-R5-9): no lookup joins `users.email = tenants.email` any more; a row left NULL by the backfill never authenticates. `api_keys.is_primary` marks the account's primary pair; one LIVE primary per tenant (partial unique index). `tenants.access_key/secret_key` mirror the live primary — rewritten by `rotateRows` in the same transaction, read by no credential lookup (`TestLookupCredential_TenantsPairWithoutARowIsUnknown`).
- `Auth.LookupCredential` (exported; `handlers.go`) is THE lookup — S3 header auth, `verifyPresignedURL` and STS go through it. `api_keys` by `key_id` → revoked row = `ErrAccessKeyRevoked`; then `sts_tokens` LEFT JOIN the parent row: parent missing, revoked or expired = `ErrAccessKeyRevoked` (WP-R5-5 cascade without deleting anything; one query on the hot path).
- `RotateAPIKey`: one transaction — `UPDATE … SET revoked_at WHERE revoked_at IS NULL` (zero rows = `ErrKeyRevoked`: two concurrent rotations never make two successors), INSERT the successor with the old scope and `is_primary`, rewrite the mirror for a primary. The old key is dropped from `keyIndex` in the same call (`markRevokedLocked`) — it used to authenticate from the in-memory map until the next restart. The audit row carries `primary` and `sts_tokens_invalidated` (unexpired children counted, not deleted).
- `RevokeAPIKey` refuses the primary (`ErrPrimaryKeyRevoke`; dashboard flash, user API 409, management API 409 `primary_key`): an account must keep one way in, and the export download link, the presigned-URL route (`auth.PrimaryPair`) and the credentials surfaces mint from it. Rotation is the way to kill a leaked primary — it hands the successor over in the same response. A scoped VLT_ key with `*` is not the primary.
- `GetPrimaryAPIKey(ctx, tenantID)` (in-memory, no secret) is the STS route's default parent; `PrimaryPair(ctx, db, tenantID)` the pair for signing on the tenant's behalf. `checkKeyCap` excludes `is_primary` rows.
- STS (`sts.go`): `intersectIPRestrict` replaces `narrowIPRestrict` (R5-10b: a token could drop the parent's IP restriction) — every requested entry must lie inside a parent entry, disjoint = `ErrSTSScope`; a token's TTL is clamped to the parent's `expires_at`.
- JWTs (`auth.go`): `ValidateJWT` checks `iss` (`jwtIssuer`), requires `iat`, refuses a token for a user not in `userIndex` (erased/evicted) and one whose `iat` second is before `users.password_changed_at` (`ErrJWTRevoked`; stamped by `setPassword` from `ChangePassword` and `CompletePasswordReset`, loaded by `LoadFromDB`). The dashboard keeps the session a password change was made from (`DeleteByUserIDExcept`); the API holder signs in again.
- `cacheMu` covers every reader and writer of the credential maps in this package — closed for real by WP-R5-6 (`cache_race_test.go`): WP-R5-14's claim left `GetUserByOAuth`, `GenerateEmailVerifyToken`/`VerifyEmail`/`IsEmailVerified`, `GetUserProfile`/`UpdateUserProfile`, `GetUserPreferences`/`SetUserPreferences` and `EnableMFA` reading `userIndex` unlocked, and `ValidateS3Request`/`ValidateAPIKey` writing `LastUsed`/`UsageCount` on the shared `*APIKey` outside any lock.

## Critical Methods

- `LoadFromDB(ctx)` — populates in-memory maps from PostgreSQL on startup. Without this, login/S3 auth fails after restart.
- `CreateUserWithTenant(ctx, email, password, company)` — creates user + tenant + API key + quota row. Persists to 4 tables in order: `users → tenants → api_keys → tenant_quotas`, in ONE transaction (`persistNewAccount`, R10 — a partial registration can no longer exist). Enforces `MinPasswordLength` (8, `ErrPasswordTooShort`) for every signup entry point — the web form checked it, `/auth/register` did not (Review R12); OAuth passes "" and gets no hash.
- `GenerateAPIKey` — enforces the free-tier key cap (`ErrKeyLimitReached`): `usage.FreeTierLimits.MaxAPIKeys` ACTIVE keys beyond the tenant's primary pair (the row registration writes); paid tiers are not capped here. One chokepoint for the dashboard, the management API and the user API (R11-16 / WP-R11-6; the dashboard's own count included the primary and refused every free account — R12 P1).
- `ConsumeTOTPCode(userID, code)` — TOTP replay guard (RFC 6238 §5.2, R5-15c): the same code is accepted once per 90 s window; the dashboard's `/login/verify-2fa` treats a repeat as a failed factor.
- `ValidateS3Request(ctx, accessKey)` — returns tenant from `keyIndex` map. (The S3 request path itself verifies against the database — `Auth.ValidateRequest`; this is the cache lookup.)
- `Evict(userID, tenantID)` — WP-R10-3: removes an erased account from every map (user by e-mail/id, tenant, its access key, the user's/tenant's API keys, MFA, profile, preferences). The runner calls it after `account.EraseRows` so an erased user cannot sign in from the boot-time cache until the next restart. `cacheMu` (RWMutex) guards the credential maps AND the fields of the `*User`/`*APIKey`/`*Tenant` values they hold: every reader takes `RLock`, every writer `Lock`, and what leaves the service is a copy (`GetUserByID`/`GetUserByEmail`/`GetUserByOAuth`/`ValidateS3Request`/`GetOwnedAPIKey` return copies; `snapshotOwnedKey` + `findOwnedKeyLocked` replace the old `findOwnedKey` that handed the map's pointer out). Proof: `TestAuthService_CredentialCacheIsRaceFree` — every reader against concurrent rotation, revocation, key creation, registration, eviction and settings writes, under `-race` (WP-R5-6). The dead `verifyTokens` map (written on every resend, read by nothing — R5-24) is gone.
- `SetJWTSecret(secret)` — override default JWT key from `JWT_SECRET` env var.
- `EnableMFA(ctx, userID, secret, backupCodes)` — enables TOTP 2FA, hashes backup codes, persists to `user_mfa` table. The database row is written FIRST, the in-memory copy after it (WP-R12-8: a refused write used to leave the account "enabled" in memory until the next restart). Callers pass a secret the SERVER generated and kept (the dashboard's `MFAEnrolmentStore`) — never one from a request.
- `DisableMFA(ctx, userID)` — disables 2FA, deletes from `user_mfa` table.
- `IsMFAEnabled(ctx, userID)` — checks in-memory MFA state (O(1)).
- `GetMFASecret(ctx, userID)` — returns TOTP secret for enabled users.
- `ValidateBackupCode(ctx, userID, code)` — checks and consumes a single-use backup code.
- `LoadMFAFromDB(ctx)` — loads MFA settings from `user_mfa` table on startup.
- `SetVerifySecret(secret)` — sets HMAC key used for both email verification and password reset tokens.
- `GenerateEmailVerifyToken(ctx, userID)` — creates HMAC-signed token (24h expiry) for email verification.
- `VerifyEmail(ctx, token)` — validates token signature/expiry, marks user as verified.
- `IsEmailVerified(ctx, userID)` — checks in-memory `email_verified` flag.
- `RequestPasswordReset(ctx, email)` — issues HMAC-signed reset token (1h expiry). Rate-limited to 3 requests/hour per email; returns `ErrResetRateLimited` when exceeded.
- `CompletePasswordReset(ctx, token, newPassword)` — validates token, updates password, returns userID. Caller must invalidate the user's existing sessions on success.

## Email Verification + Password Reset

Both flows share the `verifySecret` HMAC key but use distinct payload formats so tokens are not interchangeable:
- Email verify token: `userID|expiry|signature` (24h expiry)
- Password reset token: `reset|userID|expiry|fingerprint|signature` (1h expiry). `fingerprint` = first 16 hex of SHA-256(current password hash), so the token is **single-use by construction**: the reset itself (or any password change) changes the hash and every outstanding token stops verifying — no server-side state, restart-safe (R5-03). Minting or verifying with an empty HMAC secret returns `ErrNoVerifySecret` (fail closed; `VERIFY_SECRET` falls back to `JWT_SECRET` in server.go)

Password reset rate limiting is in-memory (per-email, 3/hour, sliding window). The auth service does not own session state — the dashboard handler invalidates sessions via `SessionStore.DeleteByUserID` after a successful reset.

## MFA

- **MFAService** (`mfa.go`) — standalone TOTP service: `GenerateSecret`, `ValidateCode`, `GenerateBackupCodes`. Uses `github.com/pquerna/otp`.
- **MFASettings** (`auth_mfa.go`) — per-user MFA config stored in `mfaSettings` map (in AuthService). DB-backed via `user_mfa` table.
- There is **no** test shortcut in `ValidateCode` (the former `JBSWY3DPEHPK3PXP`/`123456` pair was removed in R5-15 — it could be enrolled via the setup form). Tests mint real codes with `totp.GenerateCode`.

## Maps (in-memory)

| Map | Key | Value | Purpose |
|-----|-----|-------|---------|
| `users` | email | *User | Login lookup |
| `userIndex` | userID | *User | ID-based lookup |
| `tenants` | tenantID | *Tenant | Tenant lookup |
| `keyIndex` | accessKey | *Tenant | S3 auth (hot path). Includes scoped VLT_ keys mapped to owning tenant. |
| `apiKeys` | key | *APIKey | API key validation. Carries scope data (Permissions, BucketScope, IPAllowlist, ExpiresAt). |

## Scoped API Keys (Phase 5.11.4)

`scoped_keys.go` — permission check functions reusable by S3 enforcement and future STS (Phase 5.11.5):
- `CheckPermission(keyPerms, operation)` — `["*"]` allows all; otherwise exact match
- `CheckBucketScope(scopes, bucket)` — empty = unrestricted
- `CheckIPAllowlist(allowlist, clientIP)` — CIDR networks and single addresses, both sides PARSED (`::FFFF:1.2.3.4` matches `1.2.3.4`, case/zero-padding of IPv6 does not matter — R5-23); empty = unrestricted; a junk entry grants nothing
- `ValidateIPAllowlist(entries)` — every entry an address or a CIDR network, returned canonical (`net.IP.String` / `net.IPNet.String`); junk is `ErrInvalidIPAllowlist` naming the entry
- `KeyCreateOptions.Validate(now)` — permissions known, allowlist canonical, `ExpiresAt` after now (`ErrExpiryInPast`); called by `GenerateAPIKey` so the dashboard form, the user API and the management API refuse the same scope the same way (WP-R5-12)
- `ErrKeyExpired` — a live key past `expires_at`: `GetOwnedAPIKey` (the STS parent → 401 `parent_key_expired`) and `ValidateAPIKey` return it; S3 answers `ExpiredToken` with the key's expiry in the message (`handleS3Request`)
- `IsKeyExpired(expiresAt)` — nil = never expires
- `ValidatePermissions(perms)` — validates against `ValidPermissions`, which is BUILT from `S3Operations` (`s3_operations.go`: the `Op*` constants the S3 parser `determineOperation` assigns and nothing else — `TestS3ParserEmitsOnlyKnownOperations` in `internal/api` reads the parser's source) plus `*` and the one privilege that is not an operation: `BypassGovernanceRetention`. One list (WP-R5-12; the old literal copy lagged the parser by 18 operations). Unknown = `ErrInvalidPermission`
- `PermBypassGovernanceRetention` / `KeyScope.CanBypassGovernanceRetention()` (WP-R4-1) — whether the key may have `x-amz-bypass-governance-retention` honoured: `*` (primary key, key without a permission list) or the explicit permission; an STS token (`KeyScope.Temporary`) only with the explicit permission; nil scope = no. The permission grants no operation by itself
- `WithKeyScope(ctx, scope)` / `KeyScopeFromContext(ctx)` — the authenticated key's scope on the request context (set by `api.handleS3Request`, read by the Object Lock bypass check)

`GenerateAPIKey(ctx, userID, name, *KeyCreateOptions)` — accepts scope options. Persists to `api_keys` FIRST (nil scope slices are normalised to `{}` — `bucket_scope`/`ip_allowlist` are NOT NULL, R5-05), then publishes to `apiKeys`/`keyIndex`; a failed INSERT leaves no in-memory key.

**Key lifecycle is persisted (R5-01, migration 064; WP-R5-14 above):** `RevokeAPIKey` and `RotateAPIKey` stamp `api_keys.revoked_at` (rotate also INSERTs the replacement with the old scope, in one transaction); `SetAPIKeyExpiration` writes `expires_at`. The S3 auth path (`Auth.LookupCredential`, used by `verifyPresignedURL` too) reads `api_keys` per request and refuses a revoked row — the in-memory `RevokedAt` is a mirror for listings, and the hot index forgets a revoked key at once. Corrupt `permissions` JSON resolves to NO permissions, never `["*"]` (R5-14).

`LoadFromDB` — loads `api_keys` table with scope columns (permissions JSONB, bucket_scope TEXT[], ip_allowlist TEXT[], expires_at TIMESTAMPTZ, secret_key TEXT). Populates both `apiKeys` and `keyIndex` maps.

`Auth.ValidateRequest` (handlers.go) — returns `(tenantID, *KeyScope, error)` through `LookupCredential`: `api_keys` by `key_id` (the primary pair is an `is_primary` row), then `sts_tokens` joined to the parent. Nothing reads `tenants.access_key`.

## SigV4 Signature Verification (WP-4)

`sigv4.go` — full AWS Signature V4 verification for header-auth requests (`verifySigV4`): canonical request rebuilt with AWS URI/query encoding, string-to-sign, HMAC chain, constant-time compare. 15-min clock skew (`ErrRequestTimeSkewed`), credential-scope date bound to X-Amz-Date. SigV4 is the only way in (WP-R5-4): the `SIGV4_ENFORCE` kill-switch and the key-existence `validateAccessKey` helper are gone; SigV2 `AWS ak:sig` and bare `AWSAccessKeyId` query auth answer `ErrSignatureMismatch` (S3 `SignatureDoesNotMatch`), with or without a database. Keys whose plaintext secret was never stored (legacy bcrypt-hash-only `api_keys` rows) fail closed with a "regenerate this API key" error — `CreateUserWithTenant` and `GenerateAPIKey` both store `secret_key` so new keys always verify.

`sigv4_payload.go` — payload binding (`wrapPayloadVerification`, called from `ValidateRequest` after a signature verifies): the signature proves the DECLARED `x-amz-content-sha256`; the body is wrapped in a reader that hashes bytes as the handler consumes them and fails the final read with `ErrContentSHA256Mismatch` when the digest differs (API layer maps it to `XAmzContentSHA256Mismatch`, 400, via `bodyReadErrorCode`). `UNSIGNED-PAYLOAD` and `STREAMING-*` markers pass through unverified (per-chunk signatures of aws-chunked framing are NOT yet verified — future work); other non-digest values are rejected at auth time with `ErrInvalidContentSHA256` (→ `InvalidArgument`, 400).

## Signup Gate (pre-launch)

Public account creation can be closed with a single switch. `CreateUserWithTenant`
is the **sole chokepoint** for every signup path — the dashboard `/register` form,
the JSON `POST /auth/register` API, **and** OAuth signup (`CreateUserFromOAuth`
calls `CreateUserWithTenant` internally). Gating that one function blocks all three.

`CreateUserFromOAuth` returns `(*User, *Tenant, *APIKey, error)` — the APIKey
carries the plaintext secret so the OAuth callback can reveal it once (B2);
it is non-nil only when a new account was actually created.

- `SetSignupsEnabled(bool)` / `SignupsEnabled() bool` — toggle/read. Default **true**.
- `SetSignupsEnabledFunc(func() bool)` — 1.13: wires a dynamic source (the
  feature-flag service) as the authority; once set it overrides the static bool
  for both the gate and the read path. server.go points it at the `signups`
  flag (in-code default = `SIGNUPS_ENABLED` env, DB row overrides at runtime).
- When disabled, `CreateUserWithTenant` returns `ErrSignupsDisabled` before any work
  (no DB write, no in-memory entry). OAuth wraps it with `%w`, so callers use
  `errors.Is(err, auth.ErrSignupsDisabled)`.
- **Existing-user login is unaffected** — only *new account creation* is gated.
  Password login and OAuth login for already-linked users still work (those paths
  don't call `CreateUserWithTenant`).
- Wired in `server.go`: `SIGNUPS_ENABLED` env (parsed with `strconv.ParseBool`;
  unset = enabled). Handlers respond gracefully: dashboard `/register` shows
  "Signups are closed — join the waitlist" (and the GET page redirects to `/`),
  `/auth/register` returns 403, OAuth callback redirects would-be new signups to `/`.

## Slug Generation + Bucket Backfill (`slug.go`, `backfill.go`)

- `GenerateSlug(company)` — URL-safe slug from company name (deterministic, no DB)
- `IsReservedSlug(slug)` — checks against reserved route paths (admin, cdn, api, etc.)
- `EnsureSlugUnique(ctx, db, slug)` — appends `-N` suffix if slug taken in `tenants` table
- `EnsureTenantSlug(ctx, db, tenantID, logger)` — lazy slug generation on first bucket create
- `CanEnablePublicRead(tier)` — archive-tier gate for public-read bucket visibility
- `BackfillBuckets(ctx, db, logger)` — startup backfill: creates `buckets` rows from `object_head_cache`
- `BackfillSlugs(ctx, db, logger)` — startup slug generation for tenants missing slugs

Both backfill functions run on every startup (called from `server.go`), are idempotent, and log counts.

## STS Temporary Credentials (Phase 5.11.5)

`sts.go` — AWS STS-compatible short-lived S3 credentials with scope intersection:
- `STSToken` — access key (ASIA prefix), secret, tenant, parent key ID, scoped permissions/buckets/IPs, expiry
- `STSRequest` — requested permissions, bucket scope, IP restrictions, TTL (1–43200s, default 3600)
- `GenerateSTSToken(ctx, db, tenantID, parentKeyID, parentScope, req)` — mints token with scope intersection (permissions = intersection with parent, buckets = intersection, IP = intersection — WP-R5-5, every requested entry inside a parent entry; TTL clamped to the parent's expiry). `parentKeyID` must be a real `api_keys.key_id`: the credential lookup joins it and refuses the token once the parent is revoked, rotated, expired or gone. The GOVERNANCE bypass is never inherited (WP-R4-1): `BypassGovernanceRetention` stays on the token only when the request names it and the parent may bypass — an unscoped request copies the parent's operations without it, and a token's `*` does not include it. Persists to `sts_tokens` table. Secret stored in plaintext (required for SigV4 verification). **Review R11-03:** empty bucket/IP scopes are persisted as `{}` — nil slices became NULL and every unscoped mint failed the NOT NULL constraint; caller-fixable failures (no overlap, unknown permission) wrap `ErrSTSScope`, everything else is internal. `STSRequest.ParentKeyID` names one of the caller's own live keys as the parent (`AuthService.GetOwnedAPIKey`, typed `ErrKeyNotFound` / `ErrKeyRevoked`); the API's default parent is the account's full authority.
- `CleanupExpiredSTSTokens(ctx, db)` — deletes expired tokens and returns how many; the API server runs it hourly as the `sts_cleanup` job of its scheduler (`internal/api/jobs.go`, WP-R13-3 — it was a goroutine here that waited an hour after every start and reported nothing)

S3 auth integration: `LookupCredential` (handlers.go) falls back to the `sts_tokens` table for ASIA-prefixed keys after `api_keys`, LEFT JOIN the parent row. `verifyPresignedURL` (s3_presign.go) calls the same function. Expired tokens, and tokens of a dead parent, are rejected at auth time.

## Testing

- Unit tests: `go test ./internal/auth/... -short` (no DB needed)
- Integration tests: `go test ./internal/auth/... -run TestLoadFromDB -v` (needs local PostgreSQL)
- Backfill tests: `go test ./internal/auth/... -run TestBackfill -v` (needs local PostgreSQL, skipped with `-short`)
