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

## Testing

- Unit tests: `go test ./internal/auth/... -short` (no DB needed)
- Integration tests: `go test ./internal/auth/... -run TestLoadFromDB -v` (needs local PostgreSQL)
