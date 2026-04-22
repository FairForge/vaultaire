# internal/auth

Authentication service for Vaultaire. Handles user registration, login, JWT tokens, S3 credential validation, and API key management.

## Key Types

- **AuthService** — stateful service with in-memory maps for O(1) lookups. Backed by PostgreSQL for persistence.
- **User** — `{ID, Email, PasswordHash, Company, TenantID}`
- **Tenant** — `{ID, UserID, AccessKey, SecretKey}` — S3 auth queries `keyIndex[accessKey]`
- **APIKey** — `{ID, UserID, TenantID, Key, Secret, Hash}`

## Critical Methods

- `LoadFromDB(ctx)` — populates in-memory maps from PostgreSQL on startup. Without this, login/S3 auth fails after restart.
- `CreateUserWithTenant(ctx, email, password, company)` — creates user + tenant + API key + quota row. Persists to 4 tables in order: `users → tenants → api_keys → tenant_quotas`.
- `ValidateS3Request(ctx, accessKey)` — returns tenant from `keyIndex` map. Hot path for every S3 request.
- `SetJWTSecret(secret)` — override default JWT key from `JWT_SECRET` env var.
- `EnableMFA(ctx, userID, secret, backupCodes)` — enables TOTP 2FA, hashes backup codes, persists to `user_mfa` table.
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
- Password reset token: `reset|userID|expiry|signature` (1h expiry, single-use, in-memory tracked)

Password reset rate limiting is in-memory (per-email, 3/hour, sliding window). The auth service does not own session state — the dashboard handler invalidates sessions via `SessionStore.DeleteByUserID` after a successful reset.

## MFA

- **MFAService** (`mfa.go`) — standalone TOTP service: `GenerateSecret`, `ValidateCode`, `GenerateBackupCodes`. Uses `github.com/pquerna/otp`.
- **MFASettings** (`auth_mfa.go`) — per-user MFA config stored in `mfaSettings` map (in AuthService). DB-backed via `user_mfa` table.
- Test secret: `JBSWY3DPEHPK3PXP` with code `123456` (hardcoded in `ValidateCode` for testing).

## Maps (in-memory)

| Map | Key | Value | Purpose |
|-----|-----|-------|---------|
| `users` | email | *User | Login lookup |
| `userIndex` | userID | *User | ID-based lookup |
| `tenants` | tenantID | *Tenant | Tenant lookup |
| `keyIndex` | accessKey | *Tenant | S3 auth (hot path) |
| `apiKeys` | key | *APIKey | API key validation |

## Slug Generation + Bucket Backfill (`slug.go`, `backfill.go`)

- `GenerateSlug(company)` — URL-safe slug from company name (deterministic, no DB)
- `IsReservedSlug(slug)` — checks against reserved route paths (admin, cdn, api, etc.)
- `ValidateSlug(slug)` — validates against `^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$`
- `EnsureSlugUnique(ctx, db, slug)` — appends `-N` suffix if slug taken in `tenants` table
- `EnsureTenantSlug(ctx, db, tenantID, logger)` — lazy slug generation on first bucket create
- `CanEnablePublicRead(tier)` — archive-tier gate for public-read bucket visibility
- `BackfillBuckets(ctx, db, logger)` — startup backfill: creates `buckets` rows from `object_head_cache`
- `BackfillSlugs(ctx, db, logger)` — startup slug generation for tenants missing slugs

Both backfill functions run on every startup (called from `server.go`), are idempotent, and log counts.

## Testing

- Unit tests: `go test ./internal/auth/... -short` (no DB needed)
- Integration tests: `go test ./internal/auth/... -run TestLoadFromDB -v` (needs local PostgreSQL)
- Backfill tests: `go test ./internal/auth/... -run TestBackfill -v` (needs local PostgreSQL, skipped with `-short`)
