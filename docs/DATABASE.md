# Database

PostgreSQL schema, migration runner and invariants for Vaultaire. The schema
tables are merged from the database review (`docs/reviews/R9-database.md`,
2026-09-26) and brought up to migration 071; runner rules come from
`internal/database/CLAUDE.md`. Prod is PostgreSQL 16.13; local dev and CI use
15.x.

## Migrations

All migrations live in `internal/database/migrations/` as plain SQL. There is
**no Go runner and no `schema_migrations` table**: every deploy
(`.github/workflows/deploy.yml`), CI (`ci.yml`) and `make test-db` run

```
for f in migrations/*.sql; do psql -v ON_ERROR_STOP=1 -f "$f"; done
```

— the **whole set, in lexical order, every time, before the binary swap**.
"Already applied" is decided purely by idempotency, so every statement must be
re-runnable (`TestMigrations_Reapply` double-applies the set; `TestMigrations_FreshDatabaseBootstrap`
proves `createdb` + all migrations yields a schema where registration works).
Because psql executes the file statement by statement outside a transaction,
`CREATE INDEX CONCURRENTLY` is allowed.

**Numbering.** 80 files, `003`–`082` (2026-10-09). `001`, `002` and `053` never existed;
there are two `004_*` files (`004_backend_health.sql`, `004_mfa.sql`); `066`
and `067` collided once — the bucket-region default shipped as `066` in #502 the
same day `066_floor_quotas.sql` landed in #504 and was renumbered to `067`
(safe because the runner has no tracking table and the statements are
idempotent). The next free number is in `docs/STATUS.md` — check with
`ls internal/database/migrations | tail -1` before creating a file.

Rules for a new migration:

- next free number; one concern per file; header says what it changes and why
- guard everything: `IF NOT EXISTS`, `IF EXISTS`, `DO $$ IF … THEN` for
  constraint or type changes
- safe while the previous binary is still serving: no `NOT NULL` without a
  `DEFAULT` on a live table, no renames, `SET lock_timeout = '5s'` around any
  `ALTER TABLE` on a populated table (see 058, 066)
- indexes on the big tables (`events`, `s3_access_log`, `object_versions`,
  `object_head_cache`, `quota_usage_events`) use
  `CREATE INDEX CONCURRENTLY IF NOT EXISTS` (069 is the model)
- the migration set is the **sole schema owner** — never `CREATE TABLE` from Go
- `TestSQLLiteralsMatchSchema` (`internal/database/sql_schema_audit_test.go`)
  prepares every static SQL literal in `internal/` and `cmd/vaultaire` against
  the migrated test database, so a query naming a column the migrations do not
  create fails in CI, not in prod

### Migrations after the R9 review (065–081)

| File | Change |
|------|--------|
| `065_house_intent.sql` | What a visitor built in the landing page's house: `waitlist_signups.plan_std_tb`, `plan_vault_tb`, `room`; `tenants.intent_std_tb`, `intent_vault_tb`, `intent_room`. A hint for onboarding and demand reporting; nothing is billed from it |
| `066_floor_quotas.sql` | `tenant_floor_quotas` (PK `(tenant_id, floor)`, FK → `tenant_quotas` CASCADE, `storage_limit_bytes`, `storage_used_bytes`) — one row per floor (`standard`/`vault`) for tenants who bought a house; `object_head_cache.floor` (default `standard`); `tenants.house_period`; `tenant_quotas.pin_hot_bytes`. Backfills `floor = 'vault'` for archive-bucket objects and geyser-backed objects not moved there by smart demotion. `lock_timeout 5s` |
| `067_bucket_region_default.sql` | `buckets.region` default becomes the primary's real region `us-central-1`; rows carrying the old `us-west-1` placeholder (a region the account never had) are relabelled — they were always stored by the primary |
| `068_multipart_upload_attrs.sql` | `multipart_uploads` gains `content_type`, `metadata JSONB`, `storage_class`, `content_disposition`, `content_encoding`, `content_language`, `cache_control`, `http_expires`, `website_redirect_location` — CreateMultipartUpload is where clients send them; Complete carries only the part list and now copies them to the head row |
| `069_head_cache_byte_order_index.sql` | `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_head_cache_key_c ON object_head_cache (tenant_id, bucket, object_key COLLATE "C")` — S3 listings are UTF-8 byte order, prod's collation is `en_US.UTF-8`; listing queries now order and range on `object_key COLLATE "C"` and this index serves both |
| `082_s3_long_op_incidents.sql` | `s3_long_op_incidents` (`id` BIGSERIAL, `outcome` `abandoned`\|`error_after_commit`, `op`, `tenant_id`, `bucket`, `object_key`, `age_seconds`, `slot`, `version`, `created_at`; index on `tenant_id`) — one row per long S3 operation a STOPPING slot cut at its bound or that failed inside its committed 200 while it drained, written before the engine closes the DB (Prompt 2a.2 G1). The active slot exports `vaultaire_s3_long_ops_abandoned_total{op}` / `_drain_errors_total{op}` from it: the stopping slot is never scraped. An account erasure blanks its rows (`tenant_id`, `bucket`, `object_key` = '') instead of deleting them, so the exported counts never fall (Prompt 2a.3 H3); excluded from the export and the erasure sweep's bucket sources |
| `083_object_versions_one_latest.sql` | unique partial index `idx_object_versions_one_latest` on `object_versions (tenant_id, bucket, object_key) WHERE is_latest` — one latest version per key (Prompt 2b 0.5). Every writer of a key's version rows holds `pg_advisory_xact_lock` on the key (`lockVersionKey`); prod had 0 duplicates on 2026-10-09 and the build took 32 ms on a copy of its 205,604 rows. `idx_obj_versions_latest` (same columns, not unique) is kept |
| `081_vault_parity_orphans.sql` | `vault_parity_orphans` (PK `(tenant_id, leg, prefix)`, `first_seen`, `last_seen`) — first sightings of parity shard folders (`<digest>/<etag>` in the tenant's `_parity` container) that no `vault_parity` row names, found by the `vault_parity` job's reconcile pass (Prompt 2a, #638/#640); a folder is erased only after two sightings at least `VAULT_PARITY_ORPHAN_GRACE` (1 h) apart. Erased with the account; in the export |
| `080_pack_store.sql` | `packs` (one row per content-addressed `<aa>/<sha256>.pack` file on a slow per-file backend: inserted before the upload, `sealed_at` with its members, `retired_at` before GC deletes the file; no tenant id — infrastructure) and `pack_members` (`id`, `pack_id` → `packs` CASCADE, `tenant_id`, `member_key` — one live member per (tenant, key) —, `byte_offset`/`byte_length`, `sha256`, `deleted_at`) — the pack store (`internal/packstore`, Phase 37, #617). No writer is wired yet; `pack_members` is erased with the account, `packs` rows are kept |
| `079_buckets_name_cors_idx.sql` | `idx_buckets_name_with_cors` — partial index on `buckets (name) WHERE cors_rules IS NOT NULL`: the OPTIONS preflight looks rules up by bucket name across tenants (#612) |
| `078_bucket_cors.sql` | `buckets.cors_rules JSONB` — the S3 API's `?cors` configuration in the AWS shape (`PutBucketCors`), NULL = none; `cors_origins` stays the `/cdn` allow-list (#603) |
| `077_vault_parity.sql` | `vault_parity` — the Vault parity second copy (WP-VAULT-1): one row per vault-floor object (PK `(tenant_id, bucket, object_key)`), `etag` the shards were computed from, `size_bytes`, `data_shards`/`parity_shards` (4+4), `stripe_bytes`, `shard_bytes`, `shard_prefix` (artifact prefix in the tenant's `_parity` container), `legs TEXT[]` (backend per parity shard, `''` = not written), `state` (`complete`/`partial`, CHECK), `last_error`, `attempts`, `written_at`; partial index on the incomplete rows. Written by the `vault_parity` job; erased with the account |
| `074_job_runs_result.sql` | `job_runs.result JSONB` — the structured outcome of a job's last run that wrote one (WP-R7-5: the `routing_truth` job's per-backend counts). The admin API, the dashboard's System page and the `vaultaire_routing_truth_last_run_*` collector read the last run from here, so a freshly started process reports it rather than 0 (Review R13-14). A failed run keeps the previous result |
| `071_retention_job.sql` | `job_runs` (`job` PK, `last_started_at`, `last_finished_at`, `last_success_at`, `last_outcome`, `last_error`, `rows_affected`) — the persisted state of every background job (Review R13-14 created it for the retention job; since WP-R13-3 the one scheduler in `internal/api/jobs.go` writes a row per job — five daily ones, seven interval ones — and reads `last_success_at` to decide whether a daily run is owed and to export `vaultaire_job_last_success_timestamp_seconds{job_name}`; `GET /api/v1/admin/jobs` is the table as JSON) — and the time indexes the job's range deletes need: `s3_access_log (logged_at)`, `stripe_events (processed_at)`, `webhook_deliveries (created_at)`, `access_patterns (last_seen)`, `quota_usage_events ("timestamp")` |
| `070_signup_attribution.sql` | `waitlist_signups.referrer` (host only), `utm_source`, `utm_medium`, `utm_campaign`; `users.signup_referrer`, `signup_utm_source`, `signup_utm_medium`, `signup_utm_campaign` — so LET/Reddit sign-ups can be told apart |

The per-file purpose table for 003–069 is in `internal/database/CLAUDE.md`.

## Test databases

- `make test-db` creates and migrates `vaultaire_test` (idempotent). Run it
  before any DB-backed test.
- `internal/testutil.DSN()` resolves `DATABASE_URL` > `TEST_DB_*` >
  `vaultaire_test`; every DB-backed test goes through it. CI sets
  `DATABASE_URL`.
- **Never point tests at the shared dev DB `vaultaire`** — several tests
  `DROP` and recreate tables.
- Tests must not `DROP TABLE` or `DELETE` without a tenant/key predicate: CI
  runs packages in parallel against one database. Known exception still open:
  `internal/crypto/gci_test.go` (R0-18 / WP-R0-11).

## Connection

`database.NewPostgres` (`internal/database/postgres.go`) reads `DB_HOST`,
`DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD`; pool `MaxOpenConns 50`,
`MaxIdleConns 25`, `ConnMaxLifetime 5m`, `ConnMaxIdleTime 1m`;
`sslmode=disable` (loopback only). `sql.Open` is lazy — a down Postgres is
noticed on the first query, not at boot (R9-06, WP-R9-4). Prod
`max_connections = 200`, no pgbouncer; `statement_timeout`/`lock_timeout` are
0 and nothing sets them per session. All 13 `BeginTx` sites `defer
tx.Rollback()`.

## Schema by domain

Legend: **W** written by, **R** read by (packages), **Ret** retention/cleanup,
**T?** tenant-scoped. Package names are under `internal/`.

| Table | Owner mig. | W | R | Ret | T? |
|-------|-----------|---|---|-----|----|
| **Identity & auth** | | | | | |
| users | 006 (+018/022/038/070) | auth, dashboard | auth, dashboard, api | account deletion only | — |
| tenants | 005 (+018/025/039/065/066) | auth, dashboard/admin, billing (house) | auth, api (every S3 req), dashboard | account deletion | is the tenant |
| api_keys | 006 (+031/064/076) | auth | auth (every S3 req — the primary pair is an `is_primary` row, WP-R5-14), dashboard, api | `revoked_at` soft-delete; deletion | `tenant_id` on the row (076; one live primary per tenant) |
| sts_tokens | 032 | auth | auth, api/presign | hourly `expires_at < NOW()` | yes |
| user_mfa | 004 | auth | auth (boot load) | — | via user |
| oauth_accounts | 021 | dashboard/auth | dashboard/auth | deletion | via user |
| dashboard_sessions | 018 (+023) | dashboard/auth | dashboard/auth (every dashboard req: UPDATE) | hourly `expires_at < NOW()` | yes |
| feature_flags | 059 | admin API/dashboard, flags | flags (15 s cache) | — | `'*'` global + per-tenant |
| audit_logs | 008 | audit (`Record`, 27 sites since R11: key create/rotate/revoke/expiry, password + MFA changes, registration, flag set/unset, deletion schedule/cancel, export, admin tenant actions, primary swap, STS mint, webhook CRUD, admin triggers) | api `GET /api/v1/admin/audit`, dashboard `/admin/audit` (R12, #520) | — (records) | yes (+ actor) |
| **Tenancy, quota, billing** | | | | | |
| tenant_quotas | 056 (+034/043/066) | auth (register), usage, api/quota, dashboard/admin, billing | api (every PUT), billing/metered, dashboard | deletion | PK |
| tenant_floor_quotas | 066 | usage only (`floor.go`, `quota_manager.go`: reserve/release per floor, `SetHouse`/`ClearHouse` — the Stripe webhook's `applyHouse` calls these) | api (PUT enforcement per floor, through usage), api/smart_demotion, billing/metered, dashboard | cascade tenant_quotas | PK (tenant_id, floor) |
| quota_usage_events | 056 | usage (one row per reserve) | usage (history), dashboard/overview | **none** (WP-R9-2) | yes |
| subscriptions | 018 | billing/webhook | dashboard/billing | cascade tenants | yes |
| stripe_events | 019 | billing/webhook | billing (dedup), dashboard/admin_support | — (ledger) | yes |
| billing_charges | 019 | — | — | — | orphan (ledger by design) |
| metered_usage_reports | 043 | billing/metered | billing/metered (UNIQUE guard) | — (ledger) | yes |
| bandwidth_usage_daily | 018 | api/bandwidth (flush upsert) | dashboard, billing/metered, api/alerts | cascade tenants | yes |
| backend_bandwidth_daily | 060 | api/bandwidth | dashboard/admin_costs | — | no (per backend) |
| bandwidth_alerts | 020 | api/bandwidth_alerts | api/bandwidth_alerts | — | yes |
| bandwidth_rollover | 020 | — | — | — | orphan |
| **Object metadata (the S3 truth)** | | | | | |
| object_head_cache | 017 (+025/030/037/041/042/048/051/061/066/069) | api/s3 (PUT upsert, DELETE), smart tier | api (HEAD/GET/LIST/COPY/…), usage reconcile, dashboard | DELETE on object delete; deletion | yes |
| buckets | 025 (+026/027/028/036/039/040/042/049/050/067) | api/s3_buckets, dashboard | api (every request), auth backfill | deletion | PK (tenant_id, name) |
| object_versions | 026 | api/s3 (PUT/DELETE) | api (GET ?versionId, ListObjectVersions) | deletion; never trimmed (versioning metadata-only, WP-R2-1) | yes |
| object_locks | 028 | api/s3_lock | api (PUT/DELETE guard) | deletion | yes |
| object_locations | 048 | engine (Put, incl. per chunk) | engine (map miss), dashboard overview/costs | deletion | yes (`bucket` holds the container `<tenant>_<bucket>`, R6-14) |
| vault_parity | 077 | api/vault_parity (the `vault_parity` job, DeleteObject/DeleteObjects aftermath) | api/vault_parity (GET fallback, stale pass, reconcile) | row deleted with its shards when stale; deletion | yes |
| vault_parity_orphans | 081 | api/vault_parity (reconcile) | same | sighting dropped when the folder is erased, gains its row or is gone; deletion | yes |
| smart_demotions | 062 (+063) | api/smart_demotion, smart_promotion | same | reclaim after grace; not in deletion | yes |
| multipart_uploads / multipart_parts | 024 (+068) | api/s3_multipart | api, reaper | reaper: abort 48 h idle, purge 7 d terminal; not in deletion | yes |
| idempotency_cache | 029 | api/idempotency | same | hourly, 24 h | yes |
| bucket_notifications | 027 | api/s3_notifications | api/events | not in deletion | yes |
| **Chunking / dedup** | | | | | |
| global_content_index | 016 (+051/052/054) | crypto/gci, dedup_gc, deletion | crypto/gci (GET), dedup_gc | GC sweep (marked + ref 0 + grace) | no (`dedup_scope` = tenant or `_global`) |
| tenant_chunk_refs | 016 (+054/058) | crypto/gci | crypto/gci, dedup_gc reconcile, deletion | with object delete; deletion | yes |
| object_metadata | 016 (+058) | crypto/gci | crypto/gci | deletion | yes |
| tenant_encryption_keys | 037 | crypto/sse | crypto/sse | deletion | yes |
| dedup_statistics | 016 | — | — | — | orphan (UUID tenant_id) |
| **Pack store** | | | | | |
| packs / pack_members | 080 | packstore (no writer wired yet) | packstore, api/pack_gc | pack GC (retired/expired/orphan, compaction); `pack_members` in deletion | packs no, pack_members yes |
| **Events, webhooks, logs** | | | | | |
| events | 033 (+057) | api/events (13 emit sites), billing/metered | api/events, dashboard, admin_support, export | deletion only; no retention (WP-R9-2) | yes |
| webhook_endpoints / webhook_deliveries | 033 (+056) | api/webhooks | api/webhooks, events | cascade endpoint→deliveries, event→deliveries; deletion | yes |
| s3_access_log | 040 | api/access_log (every S3 request) | delivery (logging-enabled buckets), dashboard/admin_support | delivered rows deleted; others never (WP-R9-2) | yes |
| cdn_access_log / cdn_stats_daily | 035 | api/cdn_analytics | dashboard | rollup; not in deletion | yes |
| access_patterns | 055 | — (writer `internal/intelligence` deleted in R15, WP-R6-5) | — | orphan → D-12 drop list | yes |
| **Admin / support / public** | | | | | |
| admin_notes, admin_notifications, abuse_reports | 045/046/047 | dashboard/admin, api | dashboard/admin | — | notes/abuse yes |
| waitlist_signups | 044 (+065/070) | api/waitlist | dashboard/admin (CSV export) | — | no |
| account_exports | 038, 075 | api/account_export (`Request`, the `account_export` job) | same + dashboard settings | object purged by the retention job 7 d after completion, row → `expired` | via user (the export object itself: the runner's object walk) |
| breach_records, breach_notifications, breach_affected_users | 014 | compliance (`NewBreachPgStore`, `/api/v1/admin/breach*`) | same | — | no (operator log) |

**Orphan tables** (no reader or writer in the binary; decision D-12 = drop by
migration, still pending): the 056 runtime set — `upgrade_triggers`,
`upgrade_suggestions`, `grace_periods`, `usage_reports`,
`usage_daily_snapshots`, `report_schedules`, `billing_policies`,
`billing_credits`, `invoices`, `user_activities`, `audit_logs_archive`,
`artifacts` — plus `dedup_statistics`, `bandwidth_rollover`,
`billing_charges`, `tenant_cost_daily` and `tiering_policies` (048), and the
compliance/RBAC scaffolding from 003–015 (`roles`, `user_roles`,
`change_history`, `backend_health`, `backend_capabilities`, `gdpr_*`,
`retention_*`, `deletion_requests`, `deletion_proofs`, `legal_holds`,
`portability_*`, `consent_*`, `purpose_bindings`, `privacy_controls`,
`pseudonym_mappings`, `data_minimization_policies`, `mfa_audit_log`). Two
corrections to the R9 draft: `audit_logs` is **live** — `internal/audit` has
written it since R11 (#518) — and `internal/rbac` was deleted in R11 (D-11),
so `roles`/`user_roles` have no code at all behind them. `breach_*` are the only compliance tables with a
store.

## Hot queries and the indexes that serve them

From the R9 `EXPLAIN` pass on a seeded scratch DB; every path below is
index-served.

| Query (site) | Index |
|--------------|-------|
| head-cache lookup on HEAD/GET/PUT/DELETE (`api/s3_engine_adapter.go`) | `object_head_cache_pkey (tenant_id, bucket, object_key)` |
| ListObjects, prefix range + order (`api/s3_list.go`, since 069) | `idx_object_head_cache_key_c (tenant_id, bucket, object_key COLLATE "C")` |
| ListObjectVersions (`api/s3_list_versions.go`); latest version | `object_versions_pkey`; partial `idx_obj_versions_latest` |
| chunk manifest (`crypto/gci.go`) | UNIQUE `(tenant_id, bucket_name, object_key, chunk_index)` on `tenant_chunk_refs` |
| GCI lookup / sweep / reconcile (`crypto/gci.go`, `api/dedup_gc.go`) | `global_content_index_pkey (dedup_scope, plaintext_hash)`; partial `idx_gci_ref_count`; `idx_gci_last_accessed` |
| tenant by access key (`auth/handlers.go`) | UNIQUE `tenants_access_key_key` |
| any key (`auth.LookupCredential`, used by `api/s3_presign.go` too) | `api_keys_key_id_key` (unique) — no join since 076 |
| STS token | `sts_tokens_pkey` → `api_keys_key_id_key` (the parent) |
| dashboard session get / cleanup | `dashboard_sessions_pkey`; `idx_sessions_expires_at` |
| idempotency | `idempotency_cache_pkey (tenant_id, idempotency_key)` |
| bandwidth upsert / month sum (`api/bandwidth.go`) | UNIQUE `(tenant_id, date)` |
| smart-demotion candidates / reclaim | pkey prefix + filter; partial `idx_smart_demotions_pending` |
| multipart parts / reaper | `multipart_parts_pkey`; `idx_multipart_uploads_cleanup` |
| access-log delivery, support errors | `idx_s3_access_log_tenant_bucket` |
| events by tenant / admin audit | `idx_events_created`, `idx_events_tenant` |
| usage history / activity | `idx_usage_events_tenant_time` |
| object_locations lookup (`engine/routing.go`) | pkey |

The log tables are bounded by the nightly retention job since Review R13
(`internal/api/retention.go`, a daily job of the shared scheduler: 03:30 UTC with a catch-up at boot, one
`pg_try_advisory_lock` per job, batched `ctid` deletes): `s3_access_log` 30 d (and
success rows are only recorded for `logging_enabled` buckets — error rows for
every bucket, the admin support page reads them), `events` 90 d,
`quota_usage_events` 90 d, `stripe_events` 90 d, `cdn_access_log` 2 d (the
hourly rollup re-rolls yesterday, R13-03), `webhook_deliveries` 30 d,
`waitlist_signups.ip_address/user_agent`
blanked after 90 d (row kept). `audit_logs` is never pruned. The privacy
policy and the DPA state the same numbers.

## Invariants

1. **Migrations are idempotent** — the full set applies twice with zero errors
   (`TestMigrations_Reapply`).
2. **Prod schema == migrations** — `pg_dump -s` of prod differs from a
   freshly migrated database only in one column's ordinal position (R9).
3. **Fresh DB → registration works** — `users` → `tenants` → `api_keys` →
   `tenant_quotas`, in that order, with free-tier defaults
   (`TestMigrations_FreshDatabaseBootstrap`).
4. **HEAD is served from `object_head_cache`** — one read, the primary key;
   the backend is never consulted.
5. **`object_head_cache.backend_name` is the routing truth** —
   `object_locations` and the engine's in-memory map are caches of it.
6. **Every transaction rolls back on early return** — 13/13 `BeginTx` sites
   `defer tx.Rollback()`.
7. **Chunk refcounts cannot leak via FK cascades** — no FK cascades into
   `tenant_chunk_refs`; release is explicit and set-based. The object tables
   (`object_head_cache`, `object_versions`, `object_locks`,
   `object_locations`, `multipart_uploads`, `buckets`, `smart_demotions`) have
   no FKs at all: tenant deletion is the explicit list in `ExecuteDeletion`.
8. **Metered-billing tenant scan prepares against the migrated schema**
   (`TestSQLLiteralsMatchSchema` covers every static literal).
9. **STS, session and idempotency cleanup loops exist** and are index-served
   (hourly).
10. **Backups run daily and are non-empty** — `0 3 * * *
    /opt/vaultaire/bin/pg-backup.sh` on prod, 7-day retention, size and DDL
    asserts, the dump copied off-box to Sync.com right after them (30 days
    there), `BackupStale` / `BackupOffboxStale` after 26 h without a success
    (`docs/DEPLOY.md` § Backups); plain-SQL format and no in-repo restore
    runbook drill yet (WP-R9-7). A restore loses everything on `DATA_PATH`
    (the `local` driver) and up to 24 h of DB writes.
11. **No unguarded DDL** in any migration.
12. **Byte counts are `bigint`**; `tenant_id` is TEXT on every live table
    (058 converted the chunk tables); JSONB columns are unconstrained and
    validated at read time (`api_keys.permissions` fails closed).
