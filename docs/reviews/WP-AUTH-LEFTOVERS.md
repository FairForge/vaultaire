# WP auth leftovers — five rows of SYNTHESIS table B, plus the closure of WP-R5-6

**Rows:** WP-R5-4, WP-R5-12, WP-R11-6, WP-R12-14, WP-R12-15, WP-R5-6.
**Branch:** `wp/auth-leftovers` (one commit per row) · **PR #584** · **date:** 2026-10-04 ·
**base:** `main` @ `c1f0c58` (#582). No migration. Tests on the private test database
(`vaultaire_test_driver`), everything under `-race`; `make lint` 0 issues, `make gosec` 0 issues.

## What shipped, per row

### WP-R5-4 — `SIGV4_ENFORCE` is gone; SigV4 is the only way in

- `auth.sigV4Enforced()` and the env var are deleted; nothing in `cmd/vaultaire`, `deploy/` or
  `configs/` named it (it was never set in prod — R1). The rows in root `CLAUDE.md` and
  `docs/CONFIG.md` are removed.
- `validateAccessKey` (key-existence auth) is deleted. `Auth.ValidateRequest` has one path:
  parse the `AWS4-HMAC-SHA256` header → `LookupCredential` → verify → bind the payload hash.
  SigV2 `AWS ak:sig` and a bare `?AWSAccessKeyId=` answer `ErrSignatureMismatch`
  (S3 `SignatureDoesNotMatch`) **with or without a database** — the nil-DB "test-tenant"
  convenience stays for an *unauthenticated* local request only, it never dresses a
  key-id-only request up as a tenant.
- Tests: the two `SIGV4_ENFORCE=false` sub-tests that exercised the bypass are gone; in their
  place `SIGV4_ENFORCE=false changes nothing` (wrong signature / SigV2 / bare id all refused with
  the variable set) and `legacy formats are refused even without a database`.
- Left alone: the "regenerate this API key" fail-closed path for rows without a stored secret
  (still the right answer); the historical mentions in `docs/IMPLEMENTATION_PLAN.md` §VG and
  `docs/CODE_REVIEW_PLAN.md`, which describe what *was*.

### WP-R5-12 — key hygiene

- **One list of operations.** `internal/auth/s3_operations.go`: the `Op*` constants and
  `S3Operations` (42 names). `ValidPermissions` is *built* from it (`*` and
  `BypassGovernanceRetention` added). `S3Parser.determineOperation` assigns these constants and
  nothing else; `TestS3ParserEmitsOnlyKnownOperations` (`internal/api`) parses `s3.go` with
  `go/ast` and fails on any string literal but `"Unknown"` / `opUnsupportedSubresource`. The 18
  operations R5-21 named (`Get/PutBucketAcl`, `Get/PutObjectAcl`, `GetBucketLocation`,
  `ListObjectVersions`, `Get/PutBucketLogging`, `Get/Put/DeleteBucketInventory`,
  `Get/Put/DeleteObjectTagging`) are grantable to a scoped key.
- **IP allowlist.** `ValidateIPAllowlist`: every entry an address or a CIDR network, returned
  canonical (`2001:DB8::1` → `2001:db8::1`, `10.1.2.3/24` → `10.1.2.0/24`,
  `::FFFF:9.9.9.9` → `9.9.9.9`); junk is `ErrInvalidIPAllowlist` naming the entry.
  `CheckIPAllowlist` parses both sides (R5-23: `::FFFF:1.2.3.4` matches `1.2.3.4`, IPv6 case does
  not matter; a junk entry still grants nothing; an unparsable client address never matches).
- **Expiry in the past** is `ErrExpiryInPast`. **Where:** `KeyCreateOptions.Validate(now)`, called
  by `GenerateAPIKey` — so the dashboard form, the user API and the management API refuse the
  same scope the same way (the dashboard also stops silently dropping a date that does not parse).
  Management API: 400 `invalid_ip_allowlist` / `invalid_expiry` / `invalid_permissions`
  (`writeKeyScopeError`, shared with the user API); dashboard: a sentence under the form.
- **Expired key, its own code.** S3 already answered `ExpiredToken` (R5-21 asked for
  `AccessDenied`; the prompt chose the key's own code) — it now carries the key's expiry in the
  message instead of the presigned URL's "request has expired" wording
  (`TestS3Auth_ExpiredKeyAnswersExpiredToken`, real SigV4 through `handleS3Request`). The JSON
  API that takes a key is STS's `parent_key_id`: an expired parent is a typed **401
  `parent_key_expired`** (`auth.ErrKeyExpired` from `GetOwnedAPIKey`; it was a 400 `scope_error`).
  `ValidateAPIKey` returns the typed error too.

### WP-R11-6 — key API hygiene

- Every user-API key route answers failures in the management envelope: 404 `key_not_found`
  (an id that is not the caller's — another user's key included), 409 `key_revoked`, 409
  `primary_key`, 400 `invalid_json`, 400 `invalid_days` (`days < 1` — R11-22: 0 and negatives
  used to expire the key at or before now), 400 `invalid_expiry_days` on create (present and
  `< 1`; omitted = no expiry), the WP-R5-12 scope codes, 409 `key_limit_exceeded`,
  500 `internal_error`. Success bodies unchanged (the list is always an array, never `null`).
- `SetAPIKeyExpiration` on a revoked key is `ErrKeyRevoked` (it silently updated the row).
- Management `GET /keys` items carry `is_primary`, `revoked_at`, `last_used` (a revoked key looked
  live — R11-17); the 50-key count reads `api_keys.tenant_id` (the e-mail join is gone).
- OpenAPI (`internal/docs/openapi.go`) says what the handlers do; the drift guard and
  `TestOpenAPISpec_Generation` stay green. Tests: `user_api_keys_hygiene_test.go`.

### WP-R12-14 — `__Host-` session cookie

- `dashauth.SessionCookieName = "__Host-vaultaire_session"`; `SetSessionCookie` and
  `ClearSessionCookie` both send `Secure`, `Path=/`, `HttpOnly`, `SameSite=Lax` and no `Domain`
  (the clearing cookie must satisfy the prefix's rules too, or the browser keeps the session).
  Both also expire the pre-prefix `vaultaire_session` (`LegacySessionCookieName`), which nothing
  reads: a session from before the change signs in once more and carries one cookie.
- Sign-in, the second factor, OAuth and sign-out already went through the two helpers; the one
  literal in `handleLogout` and the literals in three test files use the constant. The CSRF
  token is HMAC(key, cookie value) and follows the rename. `TestSessionCookie_HostPrefixAttributes`
  asserts the attributes on set and clear, and that the legacy name no longer signs anyone in.
- Cookie policy page (`legal/cookies.html`) and `internal/dashboard/CLAUDE.md` name the new cookie.
- Local development: Chrome and Firefox treat `http://localhost` as a secure context, so the
  prefixed cookie is accepted there as the `Secure` cookie already was; `make dash-shots` is
  unaffected.

### WP-R12-15 — password on MFA enrolment; regenerate backup codes

- `POST /dashboard/settings/mfa/enable` requires the account's password (`confirmMFAIdentity`,
  via `ValidatePassword`), checked **before** the pending code is consumed, so a refused attempt
  keeps the QR code the user scanned. An OAuth-only account has no password: it needs a session
  created within the last **10 minutes** (`mfaFreshSessionWindow`; the current session is matched
  by cookie value in `sessions.ListByUserID`) — a fresh sign-in through the identity provider.
  The setup page shows the field or the note (`HasPassword`).
- **New** `POST /dashboard/settings/mfa/backup-codes` (`HandleMFARegenerateBackupCodes`): 2FA
  must be on and the same identity check passes; `AuthService.RegenerateBackupCodes` writes the
  bcrypt hashes to `user_mfa` first (zero rows = `ErrMFANotEnabled`), then memory; every old
  code is dead, the authenticator secret is untouched; the ten new codes render once through the
  setup template's `.Enrolled` branch with a "New Backup Codes" heading; audit
  `mfa.backup_codes_regenerated` (`backup_codes` count). The settings page's 2FA card has the form.
  This is also the remedy for the crash window between `EnableMFA` and the page that showed the
  codes: a user whose codes never rendered gets a set here.
- The CSRF matrix (`TestCSRFMatrix_EveryMutationOfBothChains`, `chi.Walk`) covers the new route
  without being told.
- Tests: handler-level (`mfa_identity_test.go`: no/wrong/right password; OAuth-only fresh vs stale
  session with an injected clock, for enrol and regenerate; regenerate kills old codes and leaves
  the secret), service-level with the database (`mfa_backup_codes_test.go`: persisted, a fresh
  service loaded from the row sees the new set, the audit row), and through the production router
  with the real templates (`mfa_enrolment_test.go`: the code alone enrols nothing, the password
  field is on the page, the regenerate form is on the settings page and needs the password).
- The SYNTHESIS row said "password + current code" for regenerate; the prompt said the password.
  The password alone is required: it is the second proof already, and the authenticator may be
  the thing the user has lost. Noted on the row.
- `HandleMFAEnable` takes the session store now (one more argument; `nil` keeps the password-only
  behaviour, as the handler tests use).

### WP-R5-6 — the credential cache is race-free (closed, not just flipped)

- `TestAuthService_CredentialCacheIsRaceFree`: 18 reader loops (S3 key lookup, API-key check,
  key listing, primary/owned key, user by id/e-mail/tenant, JWT, password lookup, profile,
  preferences, e-mail verification, MFA state/secret/backup code) run while writers rotate a key
  and its successors, create and revoke keys, set expiries, register and evict accounts, write
  preferences and profile, consume TOTP codes, regenerate backup codes and complete an e-mail
  verification; `-race` (what `make test` runs). ~12 s.
- It was **red** against `main`: 14 data races. The #565 note's "`cacheMu` now covers every reader"
  did not hold — `GetUserByOAuth`, `GenerateEmailVerifyToken`, `VerifyEmail`, `IsEmailVerified`,
  `GetUserProfile`, `UpdateUserProfile`, `GetUserPreferences`, `SetUserPreferences` and
  `EnableMFA` read `userIndex` with no lock; `ValidateS3Request` and `ValidateAPIKey` wrote
  `LastUsed`/`UsageCount` on the shared `*APIKey` outside any lock; `findOwnedKey` handed the
  map's pointer out and `Rotate`/`Revoke`/`SetAPIKeyExpiration`/`GetOwnedAPIKey` read it after
  releasing the lock.
- Now: every reader `RLock`, every writer `Lock`; `GetUserByID`/`GetUserByEmail`/`GetUserByOAuth`/
  `ValidateS3Request`/`ValidateAPIKey`/`GetOwnedAPIKey` return **copies**; `snapshotOwnedKey`
  (copy under `RLock`) + `findOwnedKeyLocked` (the pointer, under the caller's lock) replace
  `findOwnedKey`. `IsMFAEnabled`'s `mfaMu` is taken before `cacheMu`, never inside it. The dead
  `verifyTokens` map (written on every resend, read by nothing — R5-24) is deleted.
- Callers of `GetUserByID`/`GetUserByEmail` (dashboard MFA/OAuth, login, password reset, the
  `/auth/login` API) only read fields, so the copy changes nothing for them.

## Left out, and why

- **Profile / preference maps** (the WP-R5-4 row's "(WP-R12-11)" half): still in-process memory,
  now under the lock. Persisting or dropping them is WP-R12-11's call (the dashboard reads
  `EmailNotifications` from the preferences); not an auth-leftovers row.
- **The plan's queue item 10 line** (`docs/IMPLEMENTATION_PLAN.md` Status) is the driver's to
  update; only the six SYNTHESIS rows are flipped here.
- **`docs/API.md` line 43** still says a revoked key answers `AccessDenied`; since WP-R5-14 it is
  `InvalidAccessKeyId`. Not this WP's; one word for the next docs pass.
- The Vault layer (queue item 11) and `docs/CLOUDFLARE.md`: untouched, as asked.

## Observed while running the suites

- `TestOnboarding_PlanWaitingStep` (`internal/dashboard/handlers`) failed once in a full package
  run with the rendered "2 TB downstairs" missing, and passed alone and on the next full run.
  Nothing in this branch touches onboarding; it reads like a shared-DB timing flake of the kind
  `project_ci_flakes` lists. Not investigated further here.
- Under `-race`, bcrypt dominates: a first draft of the race test registered 200 password accounts
  and ran past five minutes without deadlocking. The test uses OAuth registrations (no bcrypt)
  and 60 rounds.

## Adversarial pass

| # | case | result |
|---|---|---|
| 1 | `SIGV4_ENFORCE=false` in the environment, wrong secret / SigV2 / bare `AWSAccessKeyId` | all `ErrSignatureMismatch` (`TestValidateRequest_SigV4Enforcement`) |
| 2 | SigV2 header with no database | refused, not "test-tenant" |
| 3 | a literal operation name added to `determineOperation` | `TestS3ParserEmitsOnlyKnownOperations` fails naming the line |
| 4 | allowlist `10.0.0.0/40`, `office`, `1.2.3.4,5.6.7.8`, empty entry | `ErrInvalidIPAllowlist`, no key created, on every entry point |
| 5 | key with `ExpiresAt = now` | `ErrExpiryInPast` (not after now) |
| 6 | expired key on S3 PUT with real SigV4 | `ExpiredToken`, message carries the expiry; `reason="expired",key_known="true"` |
| 7 | STS mint from an expired parent | 401 `parent_key_expired`, `param=parent_key_id` |
| 8 | revoke / rotate / expire another user's key by id | 404 `key_not_found` |
| 9 | expire with `days: 0`, `-3`, `{}` | 400 `invalid_days` |
| 10 | pre-prefix `vaultaire_session` cookie with a valid token | redirected to `/login` |
| 11 | enable 2FA with the right code and no / wrong password | refused, pending secret kept, code not consumed |
| 12 | OAuth-only account, session 11 min old | enrol and regenerate refused; a session created now passes |
| 13 | regenerate with the right password | old codes dead, new ones work, secret unchanged, audit row |
| 14 | 14 data races on `main` from the new race test | 0 after the lock sweep |
