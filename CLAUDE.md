# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Vaultaire is a universal storage orchestration engine providing a unified S3-compatible API across multiple storage backends (local, iDrive e2 (primary, per-region), Seagate Lyve Cloud, Geyser (tape), Cloudflare R2 (public buckets only), OneDrive fleet (permafrost, internal), Sync.com via its encrypted WebDAV bridge (`sync`, target-only, flag-gated; generic WebDAV driver), Quotaless/S3-compat (dormant)). It is the core of FairForge's commercial product stored.ge — prices live in `internal/api/landing/prices.json`.

**Language**: Go 1.25 | **Database**: PostgreSQL 15+ | **Router**: chi/v5 | **Logging**: Uber Zap

## Strategic Reference Documents

- **`.private/LAUNCH_STRATEGY.md`** — Launch day offerings, 18-month roadmap, self-hosted fleet buying sequence, Quotaless exit plan
- **`.private/VAULT_SERIES_ECONOMICS.md`** — Vault1/3/5/10/18/50/100 tiers with overselling, LET marketing, per-tier COGS
- **`.private/ADVANCED_ARCHITECTURE.md`** — FastCDC, global content index, convergent encryption, federation protocol, seamless node addition
- **`.private/PRODUCT_LINEUP.md`** — Full 7-product catalog, COGS, margins, use cases, pricing strategy
- **`.private/IDRIVE_RESELLER_API.md`** — Complete iDrive e2 Reseller API reference, all endpoints, pricing, 6-phase integration plan
- **`.private/TIER_STRATEGY.md`** — Three-tier GTM: Vault (archive), Standard (smart), Performance (B2 killer). Selling model, use cases, novel features
- **`.private/PERMAFROST_TESTING_RESULTS.md`** — OneDrive benchmark results v1→v2→v3 (HTTP/1.1 + Range = 214 MB/s fleet)
- **`docs/CLOUDFLARE.md`** — the Cloudflare operator page: which account holds what, tokens, zone settings applied 2026-10-04, what the edge Workers do, rules learned
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
Driver Layer (internal/drivers) Storage provider implementations (local, s3, lyve, quotaless, onedrive, geyser, idrive, r2, wasabi, webdav)
```

### Entry Point

`cmd/vaultaire` is the only product binary; everything under `cmd/tools/` is an operator probe or benchmark (table in `cmd/tools/README.md`), never linked into the product (`go list -deps ./cmd/vaultaire`) and excluded from the Security workflow's gosec run.

`cmd/vaultaire/main.go` — initializes drivers from environment variables, opens PostgreSQL (optional — the DB handle is opened lazily and never pinged at boot (R9-06 / WP-R9-4): a dead Postgres still logs "connected" and every DB call then fails), starts the HTTP server. Storage mode: `STORAGE_MODE`, else auto-detected iDrive > Wasabi > Quotaless > S3 > Geyser > local (`config.DetectStorageMode`; Lyve, R2, permafrost and sync are registered when their env vars are set but never auto-selected as primary; `STORAGE_MODE=sync` is refused at boot). A `STORAGE_MODE` naming an unregistered driver is a fatal boot error. Prod runs `STORAGE_MODE=idrive` again since 2026-10-04 15:08 UTC (the new reseller account; the Wasabi interim of 2026-10-03 lasted one day — the `wasabi` driver stays registered and dormant).

### Dual Terminology

External (S3-compatible): Bucket, Object, Key
Internal: Container, Artifact, Path

The `engine.Driver` interface (in `internal/engine/interface.go`) is the sacred contract all drivers implement: `Name`, `Get`, `Put`, `Delete`, `List`, `Exists`, `HealthCheck`.

### Key Database Tables

Registration persists to **four tables in order**: `users` -> `tenants` -> `api_keys` -> `tenant_quotas`, in one transaction (R10). Missing any causes failures. **The `api_keys` row is the credential (WP-R5-14, migration 076):** the primary pair is an `is_primary` row with its `tenant_id` on it, every lookup (S3 SigV4, presigned URLs, STS minting — one function, `auth.Auth.LookupCredential`) resolves `api_keys` by `key_id` with `revoked_at` honoured, then `sts_tokens` for ASIA-prefixed temporary credentials joined to the parent key (a token dies with its parent). `tenants.access_key/secret_key` are a mirror of the live primary pair, rewritten by a rotation in the same transaction and read by no lookup. The primary is rotated, never revoked (`auth.ErrPrimaryKeyRevoke`, 409). A JWT issued before `users.password_changed_at` is refused.

Other critical tables (78 migration files numbered 003–080 (004 twice, 053 never existed) through `080_pack_store.sql`; the next number is in `docs/STATUS.md`):
- `object_head_cache` — HEAD/GET metadata cache (~1ms), content-type, ETag, metadata JSONB
- `buckets` — bucket registry with visibility, cache TTL, metadata JSONB, slug, and two CORS columns: `cors_origins` (the `/cdn` allow-list) and `cors_rules` (078: the S3 API's `?cors` configuration in the AWS shape, NULL = none — the OPTIONS preflight is answered before SigV4 from the rules of every bucket of that name, a signed response carries only the caller's own bucket's rule, `internal/api/s3_cors.go`; partial index `idx_buckets_name_with_cors` on `name`, 079)
- `multipart_uploads`, `multipart_parts` — in-progress multipart state; the upload row also keeps the attributes sent on CreateMultipartUpload (content type, `x-amz-meta-*`, cache/disposition/encoding headers, `x-amz-storage-class` — 068, R3) because CompleteMultipartUpload carries only the part list
- `object_versions` — versioning support (version_id, is_latest, delete markers)
- `object_locks` — Object Lock / WORM retention and legal holds
- `account_exports` — the GDPR export (WP-R10-3b, 075): `pending` → the `account_export` job streams one JSON document (`internal/api/account_export_sections.go`, keyset-paged, never a secret column — `TestExportSectionsCoverSchema`) through `generatedObjectWriter` into the tenant's **system bucket `_exports`** (a `buckets` row whose name no S3 bucket name can take; to the S3 API it does not exist except GetObject/HeadObject of its keys; hidden from every listing, count and dashboard page; never versioned or lock-enabled; written even over quota, audited) → `completed` with `file_path`/`etag`/`expires_at = +7 d`; `GET /api/v1/manage/account/export/{id}` and the dashboard mint a 1 h presigned GET on the tenant's primary pair; 410 after expiry; three failed attempts = `failed`; one pending per user (partial unique index). Erased with the account (rows by user, the object by the runner's walk)
- `idempotency_cache` — management API idempotency keys (24h TTL)
- `sts_tokens` — STS temporary credentials (ASIA-prefixed, scope-intersected, hourly cleanup)
- `stripe_events` — webhook event dedup
- `dashboard_sessions` — PostgreSQL-backed sessions with IP/user-agent tracking
- `oauth_accounts` — OAuth provider links (Google, GitHub)
- `smart_demotions` — Smart-tier demotion ledger (5.15.8): one row per hot→cold move, hot copy reclaimed after a grace period under an etag guard; read-time promotion state (063)
- `vault_parity` — the Vault parity second copy (077, WP-VAULT-1, flag `vault_parity` default OFF): one row per vault-floor object, bound to the etag its shards were computed from. RS 4+4 over the object (`internal/api/vault_parity_codec.go`, 1 MiB per shard per stripe): the four data shards are NOT stored (tape holds them), the four parity shards go to the free leg (`permafrost`, else `lyve`) as `<tenant>__parity/<digest>/<etag>/p<j>` by the `vault_parity` job (every 2 min, registered only with a leg). Any four of the eight shards rebuild the object, so the parity alone is a complete copy: a GET of a vault object whose backend fails (error, not found through the failover chain, open breaker — never `ErrArchived`, which keeps the restore semantics) is rebuilt from the parity (`x-vaultaire-served-from: parity`), and from the data ranges Geyser can still serve when a parity shard is gone too. `state` = `complete` | `partial` (which shards landed, `last_error`, retried up to 20 attempts); an overwrite or delete makes the row stale and the job erases the shards; DeleteObject erases them at once (best effort); an account erasure erases every row's shards before the sweep and defers the tenant when the leg is not registered or its breaker is open. A chunked object is never vault-floor (asserted, not handled)
- `packs`, `pack_members` — the pack store (080, `internal/packstore`, Phase 37): small immutable objects batched into content-addressed `<aa>/<sha256>.pack` files on a slow per-file backend (`sync`); `pack_members` is tenant data (erased by `account.EraseRows`; `pack_gc` rewrites a pack whose rows were erased), `packs` is infrastructure. GC ages are Postgres timestamps only — Sync reports a 1970 modtime for every file
- `site_stats_daily`, `site_visitors_daily` — cookieless public-site statistics (073, `internal/sitestats`): daily totals by page/event, referrer host, utm, country, device; visitor hashes (daily-salted, in-memory salt) live 2 days, only the count stays. Read at `/admin/stats`; beacon `POST /api/ping`
- `feature_flags` — runtime flags (1.13): global kill-switches + per-tenant overrides, `'*'` = global row; served via `internal/flags` (~15s cache, admin API + dashboard `/admin/flags`)
- `audit_logs` — the operator audit trail (008), written by `internal/audit` since Review R11 (it had no writer at all before — 0 rows in 30 days of prod): every key create/rotate/revoke/expiry, password change/reset, MFA enable/disable, registration, flag set/unset, account deletion schedule/cancel, data export, admin tenant suspend/enable/quota/tier/bandwidth, primary swap (and refused swaps), waitlist/audit export, admin triggers, STS mint and webhook CRUD, and since Review R12 every dashboard sign-in outcome (`auth.login_failed|login_succeeded|login_locked|mfa_failed|mfa_succeeded`, event_type `auth`, with reason/via/email in metadata) — with actor (`performed_by`), subject, client IP (from `internal/clientip`) and user agent. Read with `GET /api/v1/admin/audit` (admin JWT) or the dashboard's `/admin/audit` page (actor/IP filters, escaped CSV). An account erasure removes the subject's own rows and nulls `ip`/`user_agent` on the tenant-keyed operator rows (the privacy policy's wording), and writes one tenant-keyed `account.erased` row (WP-R10-3); an **operator's** erasure keeps what they did to other tenants and to the service (`event_type = 'admin'`), with `user_id`/`performed_by`/`ip`/`user_agent` nulled, their id and e-mail dropped from the metadata and `actor_erased` set (WP-R10-3d)
- `waitlist_signups.referrer/utm_*`, `users.signup_referrer/signup_utm_*` — sign-up attribution (070, R12): `source` is the utm_source, else the referring host, else `landing`
- `job_runs` — one row per background job (071): start, finish, `last_success_at`, outcome (`ok` | `error` | `interrupted` | `running`), error text or note, rows. Written by **the one scheduler** (`internal/api/jobs.go`, WP-R13-3), which runs every job under one advisory lock per job. Daily jobs (UTC; a catch-up check a few minutes after every start, then hourly — a run happens when there is no success since the last scheduled time, because a 24 h ticker never fires on a box redeployed several times a day): `inventory` 00:30, `dedup_gc` 02:30, `retention` 03:30, `account_deletion` 04:30, `routing_truth` 05:30, `smart_demotion` 06:30. Interval jobs: `multipart_reaper`, `cdn_rollup`, `idempotency_cleanup`, `sts_cleanup`, `session_cleanup`, `bandwidth_alerts` (hourly), `access_log_delivery` (5 min), `account_export` (1 min — renders pending GDPR exports, WP-R10-3b), `vault_parity` (2 min — parity shards of vault-floor objects, WP-VAULT-1; registered only when a parity leg is), `pack_gc` and `stripe_gc` (hourly — the pack store's GC and the reaper of orphan pieces of striped `sync` objects; registered only with the `sync` backend). A run is `error` — and a daily job retried hourly — only when the run itself failed; one object/chunk/report that failed is a note on an `ok` run (account deletion is the exception: a deferred tenant fails the run on purpose). Metrics: `vaultaire_job_last_success_timestamp_seconds{job_name}` (read from this table, so it is right after a restart) and `vaultaire_job_runs_total{job_name,outcome}` — the label is `job_name` because Prometheus reserves `job`; rules `deploy/monitoring/vaultaire-jobs.yml`. Admin: `GET /api/v1/admin/jobs`, `POST /api/v1/admin/jobs/{job}/run` (202; 409 while a run holds the lock), and the System page of the dashboard. In tests a job uses its own name (`JobName`) — the rows are global. `job_runs.result` (074) is a job's structured last result as JSON, read back by collectors/admin (the R13 lesson: never an in-process value)
- **Routing truth** (WP-R7-5, `internal/api/routing_truth.go`): `object_head_cache.backend_name` is what GET/DELETE hint the engine with and what the demotion ledger, the deletion runner and the erasure sweep act on — and nothing checked it (prod 2026-10-02: 2,613 rows on `onedrive`, a name no driver has had since the fleet became `permafrost`; 451 on `local` for a DATA_PATH holding ten files; 62 NULL; 2,007 of 2,037 `idrive` rows in the reseller account replaced on 2026-09-21 — HEAD 200 for all). Now: a **boot check** (every distinct name in `object_head_cache` / `smart_demotions` / `object_versions` must be a registered driver, and two registered names must never share a store — `engine.SharedStores`, drivers' `StoreID`; one Error line per finding, never blocks boot), the daily **`routing_truth` job** (05:30 UTC; `ROUTING_TRUTH_SAMPLE` rows per backend asked of the RECORDED backend only — the driver, with the row's tenant in the context; never the fan-out, and the breaker is observed, never charged; a miss re-reads the row so a concurrent delete/overwrite/demotion is `changed`, not `missing`; a driver error or an open breaker is `error`, never `missing`; rows on an unregistered name are `unknown_backend`; chunked rows are checked chunk by chunk through the chunk store, a legacy-address hit is present + `legacy`; writes nothing), metrics `vaultaire_routing_truth_checks_total{backend,result}`, `_chunk_checks_total`, `vaultaire_routing_unknown_backend_rows{table,backend}` and `vaultaire_routing_truth_last_run_missing_ratio{backend}` (both read from the tables on scrape), `vaultaire_routing_shared_store_backends`, `vaultaire_routing_unknown_backend_reads_total{op,backend}` (GET/DELETE/copy/restore/CDN of a row on an unregistered name — the request still falls back to the fan-out; WP-R6-1 removes it); rules `deploy/monitoring/vaultaire-routing.yml`. Admin `GET /api/v1/admin/routing-truth`, `POST /api/v1/admin/routing-truth/resolve-null` (NULL rows: every driver asked once — the one justified fan-out — written only when exactly one holds the bytes; dry run by default), the System page line. **HEAD stays cache-only** (non-negotiable 2); the truth is the job + the admin endpoint. **`local` is no tier**: `REDUCED_REDUNDANCY` is unmapped (degrades to the primary at STANDARD) and `local` reports STANDARD. The prod backfill is a read-only plan the operator runs (`cmd/tools/routing-truth`, `docs/reviews/WP-R7-5.md`)
- The **retention job** (`internal/api/retention.go`, the `retention` job: batched deletes) keeps `s3_access_log` 30 d, `events` / `quota_usage_events` / `stripe_events` 90 d, `webhook_deliveries` 30 d, `cdn_access_log` 2 d after rollup, `waitlist_signups.ip_address/user_agent` cleared after 90 d, GDPR export objects 7 d (deleted through the customer delete path, the `account_exports` row stays as `expired`), `audit_logs` never. Admin `POST /api/v1/admin/retention` (202; 409 while a run holds the lock). The privacy policy and DPA state these periods — change both together
- Account deletion (WP-R10-3, D-15/D-16): `DELETE /api/v1/manage/account`, `DELETE /api/v1/user` and the dashboard all schedule through `internal/account` (30-day grace, `users.status = 'pending_deletion'`, nothing blocked meanwhile); `internal/api/deletion_runner.go` erases on the date — the `account_deletion` job, daily 04:30 UTC + boot catch-up (`job_runs`), Stripe cancel (stamp `tenants.deletion_stripe_cancelled_at`, 072), every object deleted on its recorded backend, then **the sweep** (`internal/api/erasure_sweep.go`, WP-R10-3c): every registered backend is asked what it still holds for the tenant — blobs behind a delete marker, blobs no row names, stale copies — and it is deleted: one walk of the tenant's own prefix on drivers that implement `engine.TenantWalker` (idrive + regions, lyve, geyser, r2, s3compat, quotaless, local), `List` + `Delete` per remembered bucket on the rest (permafrost); a listing or delete that fails, an open breaker or a shutdown **defers** the tenant (never "erased" while bytes may remain); the shared chunk container `_global` is never swept (counted as `chunk_blobs_left`; WP-R8-7: on a fixed-bucket backend chunk blobs sit under the uploader's prefix and dedup GC misses them); an unregistered backend cannot be swept and is named in the record; then `account.EraseRows` over every tenant/user table (schema-walk test), one more sweep pass for bytes a write in flight put down, sessions revoked, auth cache evicted, `account.erased` audit row (counts per stage, `swept` per backend); `POST /api/v1/admin/account-deletion` starts a run now (202; 409 while running)
- Egress allowance (WP-R10-9): **no table of its own** — derived at read time by `usage.QuotaManager.EgressAllowance` (admin override `tenant_quotas.bandwidth_limit_bytes` > 0, else 0.5 × the `standard` floor + 1 × the `vault` floor, else 0.5 × `storage_limit_bytes`; ratios from `prices.json`), so `SetHouse`/`ClearHouse` never write it and an override survives a webhook replay. "Used" is egress only, per **UTC calendar month**, from `bandwidth_usage_daily` (rows are dated on the UTC day since WP-R10-9) plus the live in-process counter (`internal/api/egress_meter.go`). `bandwidth_alerts` rows are the warning ladder (80 / 95 / 100 %), created when a step is first crossed
- `tenant_floor_quotas` — whole-TB quota per floor (066, dashboard plan Phase 1): one row per (tenant, `standard`|`vault`) for tenants who bought a house; PUT is enforced per floor AND on the `tenant_quotas` total (= sum of floors); `object_head_cache.floor` records where each object is billed (GLACIER/DEEP_ARCHIVE = vault) and is what the customer-visible storage class is derived from (`engine.CustomerStorageClass(floor, backend)`: a `standard`-floor object is never reported in an archive class, wherever the Smart tier parked it — WP-R13-1). Tenants without rows keep the single total quota. Written by the Stripe webhook (`billing.WebhookHandler.applyHouse`) and `usage.QuotaManager.SetHouse/ClearHouse`

- `global_content_index`, `tenant_chunk_refs`, `object_metadata` — the dedup index of chunked objects (>64 MiB, `chunking` flag). **A chunk blob has one address (WP-R8-7):** the row's `backend_id` + container `_global` + the reserved tenant `_global` in the driver context — `t-_global/_global/_chunks/<hash>` on prod's iDrive primary (`engine.ChunkContext`, `internal/api/chunk_store.go`; before 2026-10-02 it sat under its first uploader's prefix and a second tenant's deduplicated object was unreadable). Blobs written before are read from the uploader's prefix until the operator's chunk move (`POST /api/v1/admin/chunk-move?backend=idrive`, dry run by default) has brought them over; dedup GC (`dedup_gc`, 02:30 UTC, 7-day grace) deletes at the one address and keeps the row of a blob still at a legacy one. A fixed-bucket driver refuses a call whose context names no tenant (`drivers.ErrNoTenant`; it used to land under `t-default/`)

Migrations are in `internal/database/migrations/`.

## Architecture Decisions (Non-Negotiable)

1. **Authenticated health probes, TCP dial as fallback** — unauthenticated HTTP checks against S3 backends are unreliable (EOF, 403 vary by vendor), so never probe with a bare GET. Backends with a driver are probed with a *signed* HeadBucket (iDrive, Geyser) or the Lyve console action (root key); anything else gets a backend-agnostic TCP dial. Signed probes are what catch a dead key (`internal/api/backend_probes.go`).
2. **HEAD serves from `object_head_cache`** — never fetches from backend. Size/ETag/content-type stored on PUT, queried on HEAD (~1ms). One exception: an object that REPORTS an archive class (a `vault`-floor object on Geyser) gets its live `x-amz-restore` state from the backend; a Smart-demoted `standard`-floor object reports STANDARD and makes no call (WP-R13-1).
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
| `STORAGE_MODE` | auto-detect | The primary backend by driver name (`idrive`, `wasabi`, `quotaless`, `s3`, `geyser`, `local`); must be registered or boot fails loudly. Prod: `idrive` (since 2026-10-04; `wasabi` was the interim for one day) |
| `S3_ACCESS_KEY`, `S3_SECRET_KEY` | — | AWS S3 credentials |
| `LYVE_ACCESS_KEY`, `LYVE_SECRET_KEY`, `LYVE_REGION` | region: us-west-1 | Seagate Lyve Cloud 2 — buckets are homed per region; see `internal/drivers/lyve_README.md` |
| `LYVE_PROBE_ACCESS_KEY`, `LYVE_PROBE_SECRET_KEY`, `LYVE_PROBE_CUSTOMER` | unset → the probe is the driver's signed HeadBucket; customer `v01` | Root key for the console `RSCustomerDetails` probe (root-only). It never falls back to `LYVE_*` (R7-19): the data-plane key should be the scoped `vaultaire-prod` user, which the console refuses; see `deploy/monitoring/README.md` |
| `QUOTALESS_ACCESS_KEY`, `QUOTALESS_SECRET_KEY`, `QUOTALESS_ENDPOINT` | — | Quotaless storage |
| `S3COMPAT_INSECURE_TLS` | — | `true`/`1` skips TLS verification on the `s3compat` (Quotaless) driver — self-signed endpoints only |
| `VAULTAIRE_TUNED_TRANSPORT` | true | `false` makes every S3-class driver use `http.DefaultClient` (no pooling tuning, no timeouts) instead of `TunedHTTPClient` (`internal/drivers/transport.go`) |
| `FIXED_BUCKET_PUT_TIMEOUT` | 60s | Per-PUT deadline of the fixed-bucket driver (`idrive`, `idrive-<region>`, `wasabi`; WP-VAULT-1): the time one `PutObject` or one `UploadPart` may take **per 64 MiB of its body** (a 16 MiB part gets 60 s). A request with no answer by then is retried ONCE on a connection opened for it — the same request, so a multipart part is re-sent under its own part number and upload id, never doubled; a caller that has gone and an error answer are not retried. `0` / `off` disables it; a Go duration 1s–1h, a rejected value is logged at Warn and the default kept. Counted in `vaultaire_driver_put_retries_total{driver}` (Wasabi stalled ~4 % of PUTs 9–123 s in the 2026-10-04 bench) |
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
| `JWT_SECRET` | — | **Required** — JWT signing key for API auth. The dashboard's CSRF key is derived from it under its own label (`middleware.DeriveCSRFKey`, WP-R12-5): rotating it invalidates the token of every open dashboard page (one reload) |
| `SIGNUPS_ENABLED` | true | Default for the `signups` feature flag (1.13): `false` closes public signups (web form, `/auth/register` API, OAuth signup — all gated at `auth.CreateUserWithTenant`); a `feature_flags` DB row overrides this env in either direction at runtime. Existing-user login always works |
| `VERIFY_SECRET` | — | HMAC secret for email verification tokens |
| `GEYSER_ACCESS_KEY`, `GEYSER_SECRET_KEY` | — | Geyser tape S3 credentials |
| `GEYSER_BUCKET`, `GEYSER_ENDPOINT` | — | Geyser bucket name and endpoint URL |
| `GEYSER_GET_CONCURRENCY` | 8 | Parallel range streams per Geyser `Get` (WP-VAULT-1): the first 8 MiB range is a probe (one request — an object on tape answers it with one `InvalidObjectState`, a backend that ignores `Range` answers 200 and is served as is), the rest is fetched by this many streams in order, at most `concurrency × 8 MiB` buffered. `1` = the plain single-stream GET. 1–64; a rejected value is logged at Warn and the default kept. Measured on the landing zone: 1 stream 5.4 MB/s, 8 ≈ 22, 16 ≈ 42 (bench 2026-10-04 §16.1). Metrics `vaultaire_geyser_get_ranges_total`, `vaultaire_geyser_get_bytes_per_second` |
| `GEYSER_DATACENTER_ID`, `GEYSER_CUSTOMER_ID`, `GEYSER_TAPE_COLLECTION_ID` | — | Geyser console (`GeyserAdminClient`) identifiers — read only by the `cmd/geyser-admin-test` / `cmd/geyser-smoke` tooling, not the server |
| `IDRIVE_ACCESS_KEY`, `IDRIVE_SECRET_KEY` | — | iDrive E2 S3 credentials |
| `IDRIVE_BUCKET` | `vaultaire` | The single fixed bucket every iDrive driver stores into (tenant-prefixed keys); also the bucket provisioned per enabled region at boot |
| `IDRIVE_ENDPOINT`, `IDRIVE_REGION` | `https://s3.<region>.idrivee2.com`, `us-central-1` | The primary's endpoint and region. `IDRIVE_REGION` is also the **default bucket region** (served by the primary through the engine); prod = `us-central-1` (Dallas) |
| `IDRIVE_<REGION>_ACCESS_KEY`, `IDRIVE_<REGION>_SECRET_KEY`, `IDRIVE_<REGION>_ENDPOINT` | endpoint: `IDriveRegions` table | **Enables** a region (WP-R7-1): an `idrive-<region>` driver is registered only when the region's own key pair is set (region id upper-cased, `-`→`_`, e.g. `IDRIVE_US_WEST_2_ACCESS_KEY`), its fixed `IDRIVE_BUCKET` is created in that region at boot if absent, and it is probed. There is no fallback to the primary pair (403 elsewhere). Regions without a pair cannot be chosen for a bucket (S3 400 `InvalidLocationConstraint`, dashboard option disabled). Account regions: `us-central-1 us-west-2 us-west-4 us-southwest-1 us-southeast-1 us-midwest-1 us-east-1 eu-west-1 eu-west-3 eu-west-4 eu-central-1 eu-south-1 ap-northeast-1` (`internal/drivers/idrive_regions.go`); `deploy/scripts/idrive-region-env.sh` turns the reseller key file into these lines |
| `WASABI_ACCESS_KEY`, `WASABI_SECRET_KEY`, `WASABI_REGION`, `WASABI_ENDPOINT`, `WASABI_BUCKET` | region `us-west-1`, endpoint `https://s3.<region>.wasabisys.com`, bucket `vaultaire` | **Interim Standard-tier primary** (owner decision 2026-10-03: the iDrive prod key answers 403 on object calls while the account is repaired; the partner account is free). The pair registers the `wasabi` driver — the fixed-bucket driver (`internal/drivers/wasabi.go` → `NewFixedBucketS3Driver`, same `t-<tenant>/…` keys as iDrive) — creates the bucket in the region at boot if absent, and probes it with a signed HeadBucket. It becomes the primary only with `STORAGE_MODE=wasabi` (auto-detect still prefers an iDrive pair). STANDARD is no longer pinned to `idrive`: it is the primary's class (`engine.ResolveStorageClass`), so every Standard PUT follows the switch while rows already on iDrive stay readable (keep `IDRIVE_*` set). Wasabi bills a 90-day minimum per object on a paid account; the dashboard costs it at list ($7.99/TB) and lists it as subsidized |
| `R2_ACCOUNT_ID`, `R2_ACCESS_KEY`, `R2_SECRET_KEY` | — | Cloudflare R2 S3 credentials. Registers the `r2` driver — **public buckets / CDN origin only, never a tier**: public-read buckets resolve to the internal `PUBLIC` storage class → R2 (`api.resolvePutStorageClass`); no other placement touches it |
| `R2_JURISDICTION`, `R2_BUCKET` | default, `vaultaire-public` | R2 jurisdiction endpoint (`default`\|`eu`\|`us`\|`fedramp`; `us` endpoint fails TLS as of 2026-09-24) and the single fixed bucket public objects live in (tenant-prefixed keys) |
| `SYNC_WEBDAV_PASSWORD`, `SYNC_WEBDAV_URL`, `SYNC_WEBDAV_USER`, `SYNC_WEBDAV_ROOT` | password: — (required); `http://127.0.0.1:4918`, `sync`, `vaultaire` | Sync.com's encrypted WebDAV bridge (`sync-webdav`, runs on the box, localhost only). The password (the bridge's generated one, `sync-webdav credentials`) registers the `sync` driver (`internal/drivers/webdav.go`, a generic WebDAV driver) and its authenticated PROPFIND probe. **Target-only, never the primary** (`STORAGE_MODE=sync` is a boot Fatal; never a failover destination): objects land there only for a bucket with `tier_preference = 'sync'` (operator-set) of a tenant with the `sync_backend` flag (default off, per tenant). Sync's terms forbid reselling the service without its written consent — customer data only with that consent; our own data is fine. Ops manual + systemd unit: `internal/drivers/webdav_README.md` |
| `SYNC_WEBDAV_URLS`, `SYNC_WEBDAV_PASSWORDS`, `SYNC_WEBDAV_LARGE_CONCURRENCY` | — (single bridge), —, 3 | **Several Sync bridges** (one `sync-webdav` process per Sync device profile, all mounted on the same folder; live 2026-10-07: 5 bridges × 3 concurrent = 156/450 MB/s PUT/GET of 256 MiB objects vs 30/84 for one). Comma-separated, same order, 1..16, distinct URLs, one password each (secrets: never logged; a password cannot contain a comma); set = `SYNC_WEBDAV_URL`/`SYNC_WEBDAV_PASSWORD` ignored (warning); a list that does not validate keeps `sync` unregistered (boot Error). The one backend `sync` routes every key to ONE bridge by rendezvous hashing of `t-<tenant>/<container>/<key>` (`internal/drivers/webdav_multi.go`); reads of a down bridge's keys fall back to the next bridge, and a fallback's miss is `ErrWebDAVBridgeStale` (503, never 404 — bridges see each other's writes after 1.5 s–5 min); writes never fail over; the erasure walk unions every bridge. LARGE_CONCURRENCY (1..256) = transfers ≥ 16 MiB per bridge and direction. Health: up while one bridge is; `vaultaire_webdav_bridge_up{backend,bridge}`, alert `SyncBridgeDown` |
| `SYNC_WEBDAV_MAX_CONCURRENCY`, `SYNC_WEBDAV_IDLE_TIMEOUT` | 8, 60s | Bounds on the `sync` driver, per bridge (`internal/drivers/webdav_resilience.go`; live bench 2026-10-06: the bridge 500'd some requests at 32 concurrent and once stopped reading a 256 MiB PUT body after 62 KB and never answered). MAX_CONCURRENCY (1..256) caps requests in flight to the bridge (a GET frees its slot at the headers); IDLE_TIMEOUT (1s..1h) cancels a PUT whose body the bridge stops consuming, a GET with no answer, or a GET body that yields nothing while being read, with `ErrWebDAVStalled`. Fixed: a known-length PUT deadline of 2 min per 64 MiB (≤ 6 h); 3 attempts with jittered backoff on 500/502/503/504/423/reset/stall/timeout for PROPFIND/MKCOL/DELETE/GET-before-body and for a PUT whose body is seekable or ≤ 8 MiB (held in memory). A rejected value is logged at Warn and the default kept. Metrics `vaultaire_webdav_{requests_total{backend,bridge,method,outcome},stalls_total{backend,bridge,direction},retries_total{backend,bridge,method}}` (all per bridge, see SYNC_WEBDAV_URLS) |
| `SYNC_WEBDAV_STRIPE_MIN`, `SYNC_WEBDAV_STRIPE_PIECE`, `SYNC_WEBDAV_STAGING_DIR` | 512MiB, 256MiB, `<tmp>/vaultaire-stripes` | **Striped large objects** on the multi-bridge `sync` driver (`internal/drivers/webdav_stripe.go`): one object is one PUT to one bridge (~30 MB/s up), so a Put of a KNOWN length ≥ STRIPE_MIN is cut into STRIPE_PIECE pieces routed by HRW on their own names (spread over every bridge), staged on disk under STAGING_DIR (≤ bridges × LARGE_CONCURRENCY at once) and uploaded in parallel; a manifest at the key's leaf (`<leaf>%s`) is written LAST. Reads stream the pieces in order with one prefetched, ranges open only the pieces they span, whole pieces are sha256-verified. Orphan generations (a failed upload, an overwrite whose old pieces could not be deleted) are reaped by the `stripe_gc` job after 6 h (age from the generation's name — never a Sync modtime). `STRIPE_MIN=off` disables; unknown-length bodies are never striped |
| `TENANT_N_ID`, `TENANT_N_CLIENT_ID`, `TENANT_N_SECRET`, `TENANT_N_USER` | — | OneDrive fleet accounts, N=1..15 (`NewOneDriveFleetDriver`). `TENANT_1_ID` set = the `permafrost` driver is registered and probed. **Never reorder** the N slots — placement is keyed on them; see `internal/drivers/onedrive_README.md` |
| `ENCRYPTION_MASTER_KEY` | — | SSE-S3 master key (64 hex chars = 32 bytes). Absent = encryption disabled |
| `MULTIPART_MAX_UPLOAD_BYTES` | 53687091200 (50 GiB) | Per-upload in-flight byte cap for multipart parts (0 = unlimited) |
| `CHUNK_PUT_CONCURRENCY` | 8 | Parallel chunk-store workers per chunked PUT (1 = sequential stores) |
| `CHUNK_GET_PREFETCH` | 4 | Chunks fetched ahead of the write cursor per chunked GET (1 = sequential) |
| `PACKSTORE_STAGING_DIR` | `<tmp>/vaultaire-packs` | Staging directory of the pack store (`internal/packstore`): one file per open pack writer, at most 256 MiB + footer |
| `MULTIPART_ABANDON_HOURS` | 48 | Reaper aborts active multipart uploads idle longer than this |
| `MULTIPART_TERMINAL_RETENTION_DAYS` | 7 | Reaper purges completed/aborted multipart rows older than this |
| `TLS_CERT_PROBE_TARGETS` | — (off) | Comma-separated `sni@host:port` (or bare `sni` = `sni:443`) whose served leaf certificate expiry is exported as `vaultaire_tls_cert_expiry_timestamp_seconds{sni}` (hourly verified TLS handshake; an expired/mis-issued cert still reports via the x509 error's leaf). Prod: `stored.ge@127.0.0.1:443,stored.cloud@127.0.0.1:443` = the origin LE cert behind HAProxy. Rules: `deploy/monitoring/vaultaire-tls.yml` |
| `SECURITY_POLICY_URL` | https://stored.ge/legal/aup | `Policy:` line of `/.well-known/security.txt` (RFC 9116, 5.5.6); contact is fixed to security@stored.ge |
| `SMART_DEMOTION_HOT_FRACTION`, `SMART_DEMOTION_IDLE_DAYS`, `SMART_DEMOTION_MIN_AGE_DAYS`, `SMART_DEMOTION_MAX_GB_PER_RUN`, `SMART_DEMOTION_TIERS` | 0.15, 14, 3, 500, standard | Smart-tier demotion job (5.15.8) knobs; the job itself is gated by the `smart_demotion` feature flag (default OFF). A rejected value is logged at Warn and the default kept (R13-19). A demoted object stays `STANDARD` to the customer (WP-R13-1: the class comes from `object_head_cache.floor`, never from the backend — `engine.CustomerStorageClass`); a read of one that has gone to tape answers 503 + `Retry-After` while the restore we submit runs. The job runs daily at 06:30 UTC through the scheduler (WP-R13-3; `SMART_DEMOTION_MAX_GB_PER_RUN` is a budget per 24 h, whatever number of runs a day of deploys makes). Both gates (WP-R13-1, WP-R13-3) are closed: enabling is now dry-run → one canary tenant → global |
| `EGRESS_THROTTLE_MIN_BYTES_PER_SEC`, `EGRESS_THROTTLE_FACTOR`, `EGRESS_THROTTLE_MAX_STREAMS` | 65536, 1.0, 16 | Egress allowance throttle (WP-R10-9, decision D-25). A tenant past its monthly allowance (0.5 × downstairs quota + 1 × attic quota, or the admin override; `internal/usage/egress.go`) has its GetObject and `/cdn` bodies paced through ONE token bucket at `max(MIN, FACTOR × allowance / 2,592,000 s)` — the pace that spends one more allowance in a month; more than `MAX_STREAMS` concurrent paced responses per surface answer 503 `SlowDown` (S3) / 429 (`/cdn`) + `Retry-After`. Uploads, listings, HEAD and errors are never slowed. Enforcement is the `egress_throttle` feature flag (default OFF — the same decision is then only counted in `vaultaire_egress_would_throttle_total`); a per-tenant flag row with the flag off is the exemption. A rejected value is logged at Warn and the default kept. Mind the floor: at 65536 B/s a tenant past its allowance can still download ≈ 158 GiB in a month — 63× the free tier's 2.5 GiB allowance (D-25 asks Isaac to confirm it). Rules: `deploy/monitoring/vaultaire-egress.yml` |
| `ROUTING_TRUTH_SAMPLE`, `ROUTING_TRUTH_PERMAFROST_SAMPLE`, `ROUTING_TRUTH_CHUNKED_SAMPLE` | 300, 20, 20 | The daily `routing_truth` job (WP-R7-5): whole-object head rows asked of their recorded backend per run and backend; the smaller sample on the OneDrive fleet (one `Exists` there probes every account); chunked objects whose chunks are checked at the one address. Never writes. A rejected value is logged at Warn and the default kept |
| `SYNTHETIC_CHECK_URL` | — (off) | Public S3 endpoint the in-process customer-path canary signs real requests against (prod: `https://stored.ge` — through Cloudflare + HAProxy, exactly the customer path; Review R13-13, checklist item 4). Every `SYNTHETIC_CHECK_INTERVAL` (default `2m`, min `30s`): PUT → HEAD → GET (bytes compared) → DELETE → GET (404) of a fresh key in `SYNTHETIC_CHECK_BUCKET` (default `synthetic-check`, must already exist) signed with `SYNTHETIC_CHECK_ACCESS_KEY` / `SYNTHETIC_CHECK_SECRET_KEY` (a dedicated tenant; both required or the check stays off) and region `SYNTHETIC_CHECK_REGION` (default `us-east-1`). Exports `vaultaire_synthetic_check_{success,latency_seconds{op},failures_total{op},last_success_timestamp_seconds}`; rules `deploy/monitoring/vaultaire-synthetic.yml` |

## Production

This file is in a public repository (Review R14-20 / decision D-20): the hostnames and paths here are deliberately here and not in README.md.

- Server: `slc-vaultaire-01` (Ubuntu 24.04, Salt Lake City), SSH alias `vaultaire-slc`
- Two app slots `vaultaire@8000` / `vaultaire@8001` behind HAProxy, one active (`/opt/vaultaire/ACTIVE_PORT`; binaries `/opt/vaultaire/bin/vaultaire-800x`); config at `/opt/vaultaire/configs/.env`. Deploys, `.env` restarts and rollbacks go through `sudo vaultaire-switch deploy|restart|rollback|status` (zero downtime; `docs/DEPLOY.md`); the old `vaultaire.service` is masked. Logs: `journalctl -u 'vaultaire@*'`
- HAProxy fronts the service; Cloudflare proxies stored.ge
- UFW firewall: ports 22, 80, 443 only
- Daily PostgreSQL backups at 3am UTC (7-day retention) in `/opt/vaultaire/backups/`
- Deploy: push to `main` (merge queue; docs-only pushes skip it) triggers `.github/workflows/deploy.yml` (build → migrate with `lock_timeout` → `vaultaire-switch deploy` → public `/version` check). Migrations must stay additive: the previous build keeps serving the new schema until the switch
- Health: `curl https://stored.ge/health`
- Auth-failure signal: `/metrics` exports `vaultaire_auth_failures_total{reason,key_known}` and `vaultaire_auth_failures_by_key_total{key_hash}` (Review R11-10) plus the dashboard's `vaultaire_dashboard_login_failures_total{reason}` / `_lockouts_total` (Review R12); rules in `deploy/monitoring/vaultaire-auth.yml` page on a burst against a real key and warn on dashboard stuffing/lockouts. Dashboard accounts lock for 15 min after 10 failed sign-ins in 15 min
- Cross-compile: `GOOS=linux GOARCH=amd64 go build -o vaultaire-bin ./cmd/vaultaire`
