# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Vaultaire is a universal storage orchestration engine providing a unified S3-compatible API across multiple storage backends (local, iDrive e2 (primary, per-region), Seagate Lyve Cloud, Geyser (tape), Cloudflare R2 (public buckets only), OneDrive fleet (permafrost, internal), Quotaless/S3-compat (dormant)). It is the core of FairForge's commercial product stored.ge — prices live in `internal/api/landing/prices.json`.

**Language**: Go 1.25 | **Database**: PostgreSQL 15+ | **Router**: chi/v5 | **Logging**: Uber Zap

## Strategic Reference Documents

- **`.private/LAUNCH_STRATEGY.md`** — Launch day offerings, 18-month roadmap, self-hosted fleet buying sequence, Quotaless exit plan
- **`.private/VAULT_SERIES_ECONOMICS.md`** — Vault1/3/5/10/18/50/100 tiers with overselling, LET marketing, per-tier COGS
- **`.private/ADVANCED_ARCHITECTURE.md`** — FastCDC, global content index, convergent encryption, federation protocol, seamless node addition
- **`.private/PRODUCT_LINEUP.md`** — Full 7-product catalog, COGS, margins, use cases, pricing strategy
- **`.private/IDRIVE_RESELLER_API.md`** — Complete iDrive e2 Reseller API reference, all endpoints, pricing, 6-phase integration plan
- **`.private/TIER_STRATEGY.md`** — Three-tier GTM: Vault (archive), Standard (smart), Performance (B2 killer). Selling model, use cases, novel features
- **`.private/PERMAFROST_TESTING_RESULTS.md`** — OneDrive benchmark results v1→v2→v3 (HTTP/1.1 + Range = 214 MB/s fleet)
- **`internal/drivers/onedrive_README.md`** — OneDrive integration + dual-transport pattern (HTTP/2 for API, HTTP/1.1 for CDN)
- **`internal/drivers/lyve_README.md`** — Lyve Cloud 2 ops manual: per-region bucket homing, replication-policy, probe recipes
- **`internal/drivers/quotaless_README.md`** — Quotaless backend ops manual
- **`internal/drivers/pixeldrain_README.md`** — Pixeldrain benchmarks (CDN option)

## Build, Test, and Lint Commands

```bash
# Build (stamps git sha + build time into /version, /health, /status via -ldflags)
make build                # -> bin/vaultaire
make version              # print the sha/date the next build would carry

# Test
make test                 # Quick: -short -race -cover ./...
make test-unit            # Unit only: -short -race -cover ./internal/...
make test-all             # Full suite with -race
make test-coverage        # Generate coverage.out and coverage.html

# Run a single test
go test ./internal/auth/... -run TestCreateUserWithTenant -v

# DB-backed tests: create/migrate the local test database first (idempotent).
# Tests default to `vaultaire_test` (internal/testutil); DATABASE_URL overrides
# (CI sets it). Never point tests at the shared dev DB `vaultaire`. Packages run
# in parallel on ONE database: a test may only touch its own tenant's rows —
# no global reconcile, no table-wide DELETE/UPDATE, GCI cleanups bounded to the
# hashes the fixture wrote (`cleanupTenantChunkRows`, Review R15). A job that
# walks every tenant (the deletion runner) runs scoped in its tests (`onlyDue`):
# other packages seed their own past-due accounts on the same database.
make test-db

# Run a specific package
go test -v -race ./internal/api/...

# What CI runs: every package, -race, against the migrated vaultaire_test
make test-integration     # = make test-db + DATABASE_URL=… go test -race ./...
make test-load            # tests/load (env-gated SigV4 load gate, needs VAULTAIRE_LOAD_*)

# Lint + security (configs: .golangci.yml, .github/workflows/security.yml)
make lint                 # golangci-lint run ./...  (errcheck, rowserrcheck, noctx, errorlint, revive ctx-first, …)
make gosec                # the Security workflow's gosec command, verbatim (must stay at 0 issues)
make deadcode             # unreachable functions in cmd/vaultaire (x/tools deadcode; downloads the 1.26 toolchain)
make clean                # bin/, coverage files and every tool binary that used to pile up in the repo root

# Format
make fmt                  # go fmt + gofmt -s -w
```

## Architecture

### Three-Layer Design

```
API Layer (internal/api)      S3 protocol translation, auth middleware, HTTP handlers
Engine Layer (internal/engine) Backend orchestration, breaker-based failover, storage-class placement, smart demotion (flag-gated); the tiering/caching/ML/cost paths are inert (WP-R6-4)
Driver Layer (internal/drivers) Storage provider implementations (local, s3, lyve, quotaless, onedrive, geyser, idrive, r2)
```

### Entry Point

`cmd/vaultaire` is the only product binary; everything under `cmd/tools/` is an operator probe or benchmark (table in `cmd/tools/README.md`), never linked into the product (`go list -deps ./cmd/vaultaire`) and excluded from the Security workflow's gosec run.

`cmd/vaultaire/main.go` — initializes drivers from environment variables, opens PostgreSQL (optional — the DB handle is opened lazily and never pinged at boot (R9-06 / WP-R9-4): a dead Postgres still logs "connected" and every DB call then fails), starts the HTTP server. Storage mode auto-detected: iDrive > Quotaless > S3 > Geyser > local (Lyve, R2 and permafrost are registered when their env vars are set but never auto-selected as primary).

### Dual Terminology

External (S3-compatible): Bucket, Object, Key
Internal: Container, Artifact, Path

The `engine.Driver` interface (in `internal/engine/interface.go`) is the sacred contract all drivers implement: `Name`, `Get`, `Put`, `Delete`, `List`, `Exists`, `HealthCheck`.

### Key Database Tables

Registration persists to **four tables in order**: `users` -> `tenants` -> `api_keys` -> `tenant_quotas`, in one transaction (R10). Missing any causes failures. S3 auth queries `tenants` first (primary key, full access), then falls back to `api_keys` for scoped VLT_ keys (`revoked_at IS NULL` — revocation/rotation/expiry are persisted there, 064), then `sts_tokens` for ASIA-prefixed temporary credentials.

Other critical tables (71 migration files numbered 003–073 (004 twice, 053 never existed) through `073_site_stats.sql`):
- `object_head_cache` — HEAD/GET metadata cache (~1ms), content-type, ETag, metadata JSONB
- `buckets` — bucket registry with visibility, CORS, cache TTL, metadata JSONB, slug
- `multipart_uploads`, `multipart_parts` — in-progress multipart state; the upload row also keeps the attributes sent on CreateMultipartUpload (content type, `x-amz-meta-*`, cache/disposition/encoding headers, `x-amz-storage-class` — 068, R3) because CompleteMultipartUpload carries only the part list
- `object_versions` — versioning support (version_id, is_latest, delete markers)
- `object_locks` — Object Lock / WORM retention and legal holds
- `idempotency_cache` — management API idempotency keys (24h TTL)
- `sts_tokens` — STS temporary credentials (ASIA-prefixed, scope-intersected, hourly cleanup)
- `stripe_events` — webhook event dedup
- `dashboard_sessions` — PostgreSQL-backed sessions with IP/user-agent tracking
- `oauth_accounts` — OAuth provider links (Google, GitHub)
- `smart_demotions` — Smart-tier demotion ledger (5.15.8): one row per hot→cold move, hot copy reclaimed after a grace period under an etag guard; read-time promotion state (063)
- `site_stats_daily`, `site_visitors_daily` — cookieless public-site statistics (073, `internal/sitestats`): daily totals by page/event, referrer host, utm, country, device; visitor hashes (daily-salted, in-memory salt) live 2 days, only the count stays. Read at `/admin/stats`; beacon `POST /api/ping`
- `feature_flags` — runtime flags (1.13): global kill-switches + per-tenant overrides, `'*'` = global row; served via `internal/flags` (~15s cache, admin API + dashboard `/admin/flags`)
- `audit_logs` — the operator audit trail (008), written by `internal/audit` since Review R11 (it had no writer at all before — 0 rows in 30 days of prod): every key create/rotate/revoke/expiry, password change/reset, MFA enable/disable, registration, flag set/unset, account deletion schedule/cancel, data export, admin tenant suspend/enable/quota/tier/bandwidth, primary swap (and refused swaps), waitlist/audit export, admin triggers, STS mint and webhook CRUD, and since Review R12 every dashboard sign-in outcome (`auth.login_failed|login_succeeded|login_locked|mfa_failed|mfa_succeeded`, event_type `auth`, with reason/via/email in metadata) — with actor (`performed_by`), subject, client IP (from `internal/clientip`) and user agent. Read with `GET /api/v1/admin/audit` (admin JWT) or the dashboard's `/admin/audit` page (actor/IP filters, escaped CSV). An account erasure removes the subject's own rows and nulls `ip`/`user_agent` on the tenant-keyed operator rows (the privacy policy's wording), and writes one tenant-keyed `account.erased` row (WP-R10-3)
- `waitlist_signups.referrer/utm_*`, `users.signup_referrer/signup_utm_*` — sign-up attribution (070, R12): `source` is the utm_source, else the referring host, else `landing`
- `job_runs` — one row per background job with `last_success_at` (071, Review R13): the persisted schedule the nightly **retention job** (`internal/api/retention.go`, 03:30 UTC + catch-up at boot, one advisory lock, batched deletes) reads — `s3_access_log` 30 d, `events` / `quota_usage_events` / `stripe_events` 90 d, `webhook_deliveries` 30 d, `cdn_access_log` 2 d after rollup, `waitlist_signups.ip_address/user_agent` cleared after 90 d, `audit_logs` never. Admin `POST /api/v1/admin/retention` (409 while a run holds the lock). The privacy policy and DPA state these periods — change both together
- Account deletion (WP-R10-3, D-15/D-16): `DELETE /api/v1/manage/account`, `DELETE /api/v1/user` and the dashboard all schedule through `internal/account` (30-day grace, `users.status = 'pending_deletion'`, nothing blocked meanwhile); `internal/api/deletion_runner.go` erases on the date — daily 04:30 UTC + boot catch-up (`job_runs`), Stripe cancel (stamp `tenants.deletion_stripe_cancelled_at`, 072), every object deleted on its recorded backend, `account.EraseRows` over every tenant/user table (schema-walk test), sessions revoked, auth cache evicted, `account.erased` audit row; `POST /api/v1/admin/account-deletion` runs it now (409 while running)
- Egress allowance (WP-R10-9): **no table of its own** — derived at read time by `usage.QuotaManager.EgressAllowance` (admin override `tenant_quotas.bandwidth_limit_bytes` > 0, else 0.5 × the `standard` floor + 1 × the `vault` floor, else 0.5 × `storage_limit_bytes`; ratios from `prices.json`), so `SetHouse`/`ClearHouse` never write it and an override survives a webhook replay. "Used" is egress only, per **UTC calendar month**, from `bandwidth_usage_daily` (rows are dated on the UTC day since WP-R10-9) plus the live in-process counter (`internal/api/egress_meter.go`). `bandwidth_alerts` rows are the warning ladder (80 / 95 / 100 %), created when a step is first crossed
- `tenant_floor_quotas` — whole-TB quota per floor (066, dashboard plan Phase 1): one row per (tenant, `standard`|`vault`) for tenants who bought a house; PUT is enforced per floor AND on the `tenant_quotas` total (= sum of floors); `object_head_cache.floor` records where each object is billed (GLACIER/DEEP_ARCHIVE = vault). Tenants without rows keep the single total quota. Written by the Stripe webhook (`billing.WebhookHandler.applyHouse`) and `usage.QuotaManager.SetHouse/ClearHouse`

Migrations are in `internal/database/migrations/`.

## Architecture Decisions (Non-Negotiable)

1. **Authenticated health probes, TCP dial as fallback** — unauthenticated HTTP checks against S3 backends are unreliable (EOF, 403 vary by vendor), so never probe with a bare GET. Backends with a driver are probed with a *signed* HeadBucket (iDrive, Geyser) or the Lyve console action (root key); anything else gets a backend-agnostic TCP dial. Signed probes are what catch a dead key (`internal/api/backend_probes.go`).
2. **HEAD serves from `object_head_cache`** — never fetches from backend. Size/ETag/content-type stored on PUT, queried on HEAD (~1ms).
3. **ETags computed via MD5 stream on upload** — never return MD5 of empty string.
4. **Always stream, never buffer** — use `io.Reader`, never `[]byte` in memory for large data.
5. **Always propagate context** — every service method takes `ctx context.Context` as first parameter.
6. **Always wrap errors with context** — `return fmt.Errorf("create bucket %s: %w", name, err)`
7. **Client IP comes from `internal/clientip` only** — HAProxy appends the real peer as the *last* `X-Forwarded-For` entry; `CF-Connecting-IP` is trusted only behind a Cloudflare edge. Reading those headers anywhere else reopened an API-key IP-allowlist bypass (review R1-01). Shutdown order is fixed too: HTTP drain + tracker flush, then engine (which closes the DB) — `cmd/vaultaire/main.go`.

## Development Methodology

- **TDD is mandatory**: Red -> Green -> Refactor. Write failing test first.
- Tests follow Arrange/Act/Assert pattern with testify (`require` for fatal, `assert` for non-fatal).
- Pre-commit hooks run `go fmt` and `golangci-lint run` on commit; `go test ./... -short` runs on **push** (`pre-commit install && pre-commit install --hook-type pre-push`).

## Git Workflow

```bash
git checkout -b phase-X.Y-feature-name  # Branch from main
# ... TDD cycle ...
git commit -m "feat(scope): description [Phase X]"
git push origin phase-X.Y-feature-name
gh pr create --base main
# Wait for CI (build-and-test) to pass, then:
gh pr merge --squash --delete-branch
```

Commit format: `type(scope): description [Phase NNN]` where type is feat/fix/refactor/test/docs.

Never include `Co-Authored-By` lines mentioning Claude or AI in commit messages.

Branch protection: direct pushes to main are blocked; CI must pass before merge.

**Never use `--admin` to bypass CI.** Wait for `build-and-test` to pass. If CI fails, fix the issue on the branch and push again.

## CI/CD

GitHub Actions CI (`.github/workflows/ci.yml`) runs on every push/PR:
- PostgreSQL 15 service container
- `go build ./...`
- `go test ./...` with DATABASE_URL and JWT_SECRET env vars
- golangci-lint

GitHub Actions Deploy (`.github/workflows/deploy.yml`):
- `main` branch → builds, runs migrations, deploys to prod (SLC)
- `develop` branch → builds, runs migrations, deploys to dev server
- Migrations are idempotent (CREATE IF NOT EXISTS) — safe to re-run

## Environment Variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | 8000 | Server port |
| `VAULTAIRE_ENDPOINT` | http://localhost:PORT | Public S3 endpoint shown to users (registration response, dashboard credentials page); prod `https://stored.ge` |
| `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD` | localhost, 5432, vaultaire, viera, "" | PostgreSQL connection |
| `ENV` | — | Set in the prod `.env` but read by nothing |
| `DATA_PATH` | /tmp/vaultaire-data | Local storage directory |
| `STORAGE_MODE` | auto-detect | Force specific backend |
| `S3_ACCESS_KEY`, `S3_SECRET_KEY` | — | AWS S3 credentials |
| `LYVE_ACCESS_KEY`, `LYVE_SECRET_KEY`, `LYVE_REGION` | region: us-west-1 | Seagate Lyve Cloud 2 — buckets are homed per region; see `internal/drivers/lyve_README.md` |
| `LYVE_PROBE_ACCESS_KEY`, `LYVE_PROBE_SECRET_KEY`, `LYVE_PROBE_CUSTOMER` | unset → the probe is the driver's signed HeadBucket; customer `v01` | Root key for the console `RSCustomerDetails` probe (root-only). It never falls back to `LYVE_*` (R7-19): the data-plane key should be the scoped `vaultaire-prod` user, which the console refuses; see `deploy/monitoring/README.md` |
| `QUOTALESS_ACCESS_KEY`, `QUOTALESS_SECRET_KEY`, `QUOTALESS_ENDPOINT` | — | Quotaless storage |
| `S3COMPAT_INSECURE_TLS` | — | `true`/`1` skips TLS verification on the `s3compat` (Quotaless) driver — self-signed endpoints only |
| `VAULTAIRE_TUNED_TRANSPORT` | true | `false` makes every S3-class driver use `http.DefaultClient` (no pooling tuning, no timeouts) instead of `TunedHTTPClient` (`internal/drivers/transport.go`) |
| `SIGV4_ENFORCE` | true | `false` = emergency key-existence-only S3 auth (no signature check) — WP-R5-4 removes it |
| `EMAIL_PROVIDER`, `EMAIL_FROM`, `RESEND_API_KEY`, `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASSWORD` | — | Outbound email (`resend` \| `smtp`; see `internal/email/CLAUDE.md`). Prod sets none → `LogSender` (logs recipient/subject only, sends nothing) |
| `STRIPE_SECRET_KEY` | — | Stripe API key (sk_test_... or sk_live_...) |
| `STRIPE_WEBHOOK_SECRET` | — | Stripe webhook endpoint secret (whsec_...). **Required with `STRIPE_SECRET_KEY`**: `/webhook/stripe` is mounted only when both are set (an empty secret used to skip signature verification — R10-01). The Stripe endpoint must be created pinned to API version **2023-08-16** (stripe-go v75 rejects any other; R10-03); the handler answers 500 and records nothing when an event cannot be applied, so Stripe retries (R10-02). Contract in `internal/billing/CLAUDE.md` |
| `STRIPE_PRICE_VAULT3`, `STRIPE_PRICE_VAULT9`, `STRIPE_PRICE_VAULT18`, `STRIPE_PRICE_VAULT36`, `STRIPE_PRICE_STANDARD` | — | Stripe Price IDs for the five legacy pack plans (`registerStripePlans` in `internal/api/server.go`) |
| `STRIPE_PRICE_STANDARD_ANNUAL`, `STRIPE_PRICE_STANDARD_MONTHLY`, `STRIPE_PRICE_VAULT_ANNUAL`, `STRIPE_PRICE_VAULT_MONTHLY`, `STRIPE_PRICE_PINHOT_ANNUAL`, `STRIPE_PRICE_PINHOT_MONTHLY` | — | Whole-TB house checkout (dashboard plan Phase 1): one recurring price per line × period, quantity = TB. All six must be set; at boot (and hourly) each is fetched and asserted against `internal/api/landing/prices.json` (amount, USD, interval; Vault monthly = volume tiers with the $4.99 minimum) — a mismatch logs an error and keeps checkout closed. Recipe in `internal/billing/CLAUDE.md`. The page itself is gated by the `quota_checkout` flag (default off); the overview's house is gated by `house_overview` (default off) |
| `STRIPE_METER_STORAGE`, `STRIPE_METER_EGRESS` | — | Stripe Billing Meter event-names for metered tiers (both required to enable metered reporting) |
| `GOOGLE_CLIENT_ID` | — | Google OAuth client ID |
| `GOOGLE_CLIENT_SECRET` | — | Google OAuth client secret |
| `GITHUB_CLIENT_ID` | — | GitHub OAuth App client ID |
| `GITHUB_CLIENT_SECRET` | — | GitHub OAuth App client secret |
| `VAULTAIRE_BASE_URL` | http://localhost:8000 | Base URL for OAuth callbacks |
| `JWT_SECRET` | — | **Required** — JWT signing key for API auth |
| `SIGNUPS_ENABLED` | true | Default for the `signups` feature flag (1.13): `false` closes public signups (web form, `/auth/register` API, OAuth signup — all gated at `auth.CreateUserWithTenant`); a `feature_flags` DB row overrides this env in either direction at runtime. Existing-user login always works |
| `VERIFY_SECRET` | — | HMAC secret for email verification tokens |
| `GEYSER_ACCESS_KEY`, `GEYSER_SECRET_KEY` | — | Geyser tape S3 credentials |
| `GEYSER_BUCKET`, `GEYSER_ENDPOINT` | — | Geyser bucket name and endpoint URL |
| `GEYSER_DATACENTER_ID`, `GEYSER_CUSTOMER_ID`, `GEYSER_TAPE_COLLECTION_ID` | — | Geyser console (`GeyserAdminClient`) identifiers — read only by the `cmd/geyser-admin-test` / `cmd/geyser-smoke` tooling, not the server |
| `IDRIVE_ACCESS_KEY`, `IDRIVE_SECRET_KEY` | — | iDrive E2 S3 credentials |
| `IDRIVE_BUCKET` | `vaultaire` | The single fixed bucket every iDrive driver stores into (tenant-prefixed keys); also the bucket provisioned per enabled region at boot |
| `IDRIVE_ENDPOINT`, `IDRIVE_REGION` | `https://s3.<region>.idrivee2.com`, `us-central-1` | The primary's endpoint and region. `IDRIVE_REGION` is also the **default bucket region** (served by the primary through the engine); prod = `us-central-1` (Dallas) |
| `IDRIVE_<REGION>_ACCESS_KEY`, `IDRIVE_<REGION>_SECRET_KEY`, `IDRIVE_<REGION>_ENDPOINT` | endpoint: `IDriveRegions` table | **Enables** a region (WP-R7-1): an `idrive-<region>` driver is registered only when the region's own key pair is set (region id upper-cased, `-`→`_`, e.g. `IDRIVE_US_WEST_2_ACCESS_KEY`), its fixed `IDRIVE_BUCKET` is created in that region at boot if absent, and it is probed. There is no fallback to the primary pair (403 elsewhere). Regions without a pair cannot be chosen for a bucket (S3 400 `InvalidLocationConstraint`, dashboard option disabled). Account regions: `us-central-1 us-west-2 us-west-4 us-southwest-1 us-southeast-1 us-midwest-1 us-east-1 eu-west-1 eu-west-3 eu-west-4 eu-central-1 eu-south-1 ap-northeast-1` (`internal/drivers/idrive_regions.go`); `deploy/scripts/idrive-region-env.sh` turns the reseller key file into these lines |
| `R2_ACCOUNT_ID`, `R2_ACCESS_KEY`, `R2_SECRET_KEY` | — | Cloudflare R2 S3 credentials. Registers the `r2` driver — **public buckets / CDN origin only, never a tier**: public-read buckets resolve to the internal `PUBLIC` storage class → R2 (`api.resolvePutStorageClass`); no other placement touches it |
| `R2_JURISDICTION`, `R2_BUCKET` | default, `vaultaire-public` | R2 jurisdiction endpoint (`default`\|`eu`\|`us`\|`fedramp`; `us` endpoint fails TLS as of 2026-09-24) and the single fixed bucket public objects live in (tenant-prefixed keys) |
| `TENANT_N_ID`, `TENANT_N_CLIENT_ID`, `TENANT_N_SECRET`, `TENANT_N_USER` | — | OneDrive fleet accounts, N=1..15 (`NewOneDriveFleetDriver`). `TENANT_1_ID` set = the `permafrost` driver is registered and probed. **Never reorder** the N slots — placement is keyed on them; see `internal/drivers/onedrive_README.md` |
| `ENCRYPTION_MASTER_KEY` | — | SSE-S3 master key (64 hex chars = 32 bytes). Absent = encryption disabled |
| `MULTIPART_MAX_UPLOAD_BYTES` | 53687091200 (50 GiB) | Per-upload in-flight byte cap for multipart parts (0 = unlimited) |
| `CHUNK_PUT_CONCURRENCY` | 8 | Parallel chunk-store workers per chunked PUT (1 = sequential stores) |
| `CHUNK_GET_PREFETCH` | 4 | Chunks fetched ahead of the write cursor per chunked GET (1 = sequential) |
| `MULTIPART_ABANDON_HOURS` | 48 | Reaper aborts active multipart uploads idle longer than this |
| `MULTIPART_TERMINAL_RETENTION_DAYS` | 7 | Reaper purges completed/aborted multipart rows older than this |
| `TLS_CERT_PROBE_TARGETS` | — (off) | Comma-separated `sni@host:port` (or bare `sni` = `sni:443`) whose served leaf certificate expiry is exported as `vaultaire_tls_cert_expiry_timestamp_seconds{sni}` (hourly verified TLS handshake; an expired/mis-issued cert still reports via the x509 error's leaf). Prod: `stored.ge@127.0.0.1:443,stored.cloud@127.0.0.1:443` = the origin LE cert behind HAProxy. Rules: `deploy/monitoring/vaultaire-tls.yml` |
| `SECURITY_POLICY_URL` | https://stored.ge/legal/aup | `Policy:` line of `/.well-known/security.txt` (RFC 9116, 5.5.6); contact is fixed to security@stored.ge |
| `SMART_DEMOTION_HOT_FRACTION`, `SMART_DEMOTION_IDLE_DAYS`, `SMART_DEMOTION_MIN_AGE_DAYS`, `SMART_DEMOTION_MAX_GB_PER_RUN`, `SMART_DEMOTION_TIERS` | 0.15, 14, 3, 500, standard | Smart-tier demotion job (5.15.8) knobs; the job itself is gated by the `smart_demotion` feature flag (default OFF). A rejected value is logged at Warn and the default kept (R13-19). **Do not enable the flag for a tenant before WP-R13-1 lands** (demoted objects list as GLACIER and `aws s3 sync` skips them, R13-04) |
| `EGRESS_THROTTLE_MIN_BYTES_PER_SEC`, `EGRESS_THROTTLE_FACTOR`, `EGRESS_THROTTLE_MAX_STREAMS` | 65536, 1.0, 16 | Egress allowance throttle (WP-R10-9, decision D-25). A tenant past its monthly allowance (0.5 × downstairs quota + 1 × attic quota, or the admin override; `internal/usage/egress.go`) has its GetObject and `/cdn` bodies paced through ONE token bucket at `max(MIN, FACTOR × allowance / 2,592,000 s)` — the pace that spends one more allowance in a month; more than `MAX_STREAMS` concurrent paced responses per surface answer 503 `SlowDown` (S3) / 429 (`/cdn`) + `Retry-After`. Uploads, listings, HEAD and errors are never slowed. Enforcement is the `egress_throttle` feature flag (default OFF — the same decision is then only counted in `vaultaire_egress_would_throttle_total`); a per-tenant flag row with the flag off is the exemption. A rejected value is logged at Warn and the default kept. Mind the floor: at 65536 B/s a tenant past its allowance can still download ≈ 158 GiB in a month — 63× the free tier's 2.5 GiB allowance (D-25 asks Isaac to confirm it). Rules: `deploy/monitoring/vaultaire-egress.yml` |
| `SYNTHETIC_CHECK_URL` | — (off) | Public S3 endpoint the in-process customer-path canary signs real requests against (prod: `https://stored.ge` — through Cloudflare + HAProxy, exactly the customer path; Review R13-13, checklist item 4). Every `SYNTHETIC_CHECK_INTERVAL` (default `2m`, min `30s`): PUT → HEAD → GET (bytes compared) → DELETE → GET (404) of a fresh key in `SYNTHETIC_CHECK_BUCKET` (default `synthetic-check`, must already exist) signed with `SYNTHETIC_CHECK_ACCESS_KEY` / `SYNTHETIC_CHECK_SECRET_KEY` (a dedicated tenant; both required or the check stays off) and region `SYNTHETIC_CHECK_REGION` (default `us-east-1`). Exports `vaultaire_synthetic_check_{success,latency_seconds{op},failures_total{op},last_success_timestamp_seconds}`; rules `deploy/monitoring/vaultaire-synthetic.yml` |

## Production

This file is in a public repository (Review R14-20 / decision D-20): the hostnames and paths here are deliberately here and not in README.md.

- Server: `slc-vaultaire-01` (Ubuntu 24.04, Salt Lake City), SSH alias `vaultaire-slc`
- Binary at `/opt/vaultaire/bin/vaultaire`, config at `/opt/vaultaire/configs/.env`
- HAProxy fronts the service; Cloudflare proxies stored.ge
- UFW firewall: ports 22, 80, 443 only
- Daily PostgreSQL backups at 3am UTC (7-day retention) in `/opt/vaultaire/backups/`
- Deploy: push to `main` triggers `.github/workflows/deploy.yml` (build → migrate → swap → health check)
- Health: `curl https://stored.ge/health`
- Auth-failure signal: `/metrics` exports `vaultaire_auth_failures_total{reason,key_known}` and `vaultaire_auth_failures_by_key_total{key_hash}` (Review R11-10) plus the dashboard's `vaultaire_dashboard_login_failures_total{reason}` / `_lockouts_total` (Review R12); rules in `deploy/monitoring/vaultaire-auth.yml` page on a burst against a real key and warn on dashboard stuffing/lockouts. Dashboard accounts lock for 15 min after 10 failed sign-ins in 15 min
- Cross-compile: `GOOS=linux GOARCH=amd64 go build -o vaultaire-bin ./cmd/vaultaire`
