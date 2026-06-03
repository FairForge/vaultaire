# internal/database

PostgreSQL connection management and migrations for Vaultaire.

## Migrations

All migrations are in `migrations/` and are idempotent (`CREATE IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`). They run on every deploy via CI/CD.

| File | Purpose |
|------|---------|
| 003-017 | Core tables (tenants, users, api_keys, audit, compliance, object_head_cache) |
| 018 | Dashboard foundation: user status/role columns, Stripe columns on tenants, subscriptions table, bandwidth_usage_daily, dashboard_sessions |
| 019 | Stripe billing: stripe_events idempotency + billing_charges |
| 020 | Bandwidth banking: bandwidth_rollover + bandwidth_alerts tables |
| 021 | OAuth: oauth_accounts linking provider/provider_id to user_id |
| 022 | Email verification: email_verified + email_verify_token + email_verify_sent_at on users |
| 023 | Session hardening: ip_address + user_agent + last_active_at on dashboard_sessions |
| 024 | Multipart uploads |
| 025 | Bucket registry: `buckets` table, tenant `slug`+`slug_locked` columns, `backend_name` on `object_head_cache` |
| 026 | S3 versioning: `versioning_status` on `buckets`, `object_versions` table |
| 027 | Bucket notifications: `bucket_notifications` table for S3 event webhook config |
| 028 | Object Lock: `object_lock_enabled`, `default_retention_mode`, `default_retention_days` on `buckets`; `object_locks` table |
| 029 | Idempotency cache: `idempotency_cache` table for management API request deduplication (24h TTL) |
| 030 | Metadata: `metadata JSONB` column on `buckets` and `object_head_cache` |
| 031 | Scoped API keys: `permissions` (JSONB), `bucket_scope` (TEXT[]), `ip_allowlist` (TEXT[]), `expires_at` (TIMESTAMPTZ), `secret_key` (TEXT) on `api_keys` |
| 032 | STS temporary credentials: `sts_tokens` table (access_key PK, secret_key, tenant_id, parent_key_id, permissions JSONB, bucket_scope TEXT[], ip_restrict TEXT[], expires_at, created_at) |
| 033 | Event log + webhooks: `events` table, `webhook_endpoints` table (with secret, event_filter TEXT[]), `webhook_deliveries` table (status, response_code, latency_ms, retry support) |
| 034 | Free tier defaults: `tenant_quotas` column defaults changed to tier='free', storage_limit_bytes=5368709120 (5 GB). Existing rows unchanged. |
| 035 | CDN analytics: `cdn_access_log` (per-request log), `cdn_stats_daily` (tenant+bucket+date rollup) |
| 036 | MFA Delete: `mfa_delete_enabled` BOOLEAN on `buckets` (default FALSE) |
| 038 | Account deletion: `deletion_scheduled_at` + `deletion_reason` on users, `account_exports` table |
| 040 | S3 access logging + inventory: `logging_enabled`, `logging_target_bucket`, `logging_prefix`, `inventory_enabled`, `inventory_schedule`, `inventory_target_bucket`, `inventory_prefix`, `inventory_format` on `buckets`; `s3_access_log` table |
| 041 | Object tagging: `tags JSONB` (default `{}`) on `object_head_cache` — per-object S3 `?tagging` sub-resource (flat key/value map) |
| 042 | Content-Disposition: `content_disposition TEXT` (default `''`) on `object_head_cache` — stored response header; `cdn_force_download BOOLEAN` (default FALSE) on `buckets` — CDN force-attachment toggle |
| 043 | Metered billing: `metered_usage_reports` table (daily Stripe meter reports + idempotency guard); `spending_cap_cents BIGINT` (default 0) on `tenant_quotas` |
| 044 | Waitlist: `waitlist_signups` table (email UNIQUE, source, ip, user_agent) — pre-launch landing-page email capture |
| 045 | Admin notes: `admin_notes` table (tenant_id, admin_user_id → users, note) — internal support notes on customer accounts |
| 046 | Admin notifications: `admin_notifications` table (type, message, tenant_id, read_at) — admin notification system with partial index on unread |
| 047 | Abuse reports: `abuse_reports` table (reporter, tenant, bucket, key, type, description, status) — public abuse reporting + admin moderation queue |
| 048 | Object location tracking: `object_locations` (durable backend routing), `tiering_policies` (age-based tiering config), `tenant_cost_daily` (per-tenant cost rollup); `last_accessed` column on `object_head_cache` |
| 049 | Bucket tier preference: `tier_preference TEXT NOT NULL DEFAULT 'auto'` on `buckets` — pins a bucket to a specific storage tier (auto/performance/standard/archive) |

## Key Tables

- **users** — `id (UUID)`, `email`, `password_hash`, `company`, `status`, `role`, `stripe_customer_id`, `deletion_scheduled_at`, `deletion_reason`
- **tenants** — `id`, `name`, `email`, `access_key`, `secret_key`, `slug`, `slug_locked`, `stripe_customer_id`, `stripe_subscription_id`, `subscription_status`, `plan`, `suspended_at`
- **api_keys** — `id (UUID)`, `user_id → users`, `name`, `key_id`, `secret_hash`, `secret_key`, `permissions` (JSONB, default `["*"]`), `bucket_scope` (TEXT[]), `ip_allowlist` (TEXT[]), `expires_at`
- **tenant_quotas** — `tenant_id (PK)`, `storage_limit_bytes`, `storage_used_bytes`, `tier`
- **dashboard_sessions** — `id (VARCHAR 64)`, `user_id → users`, `tenant_id → tenants`, `email`, `role`, `ip_address`, `user_agent`, `created_at`, `last_active_at`, `expires_at`
- **subscriptions** — Stripe subscription state tracking
- **bandwidth_usage_daily** — per-tenant daily ingress/egress/requests (unique on tenant_id + date)
- **buckets** — `(tenant_id, name) PK`, `visibility` (private/public-read), `cors_origins`, `cache_max_age_secs`, `bandwidth_budget_bytes`, `versioning_status`, `object_lock_enabled`, `default_retention_mode`, `default_retention_days`, `mfa_delete_enabled`, `logging_enabled`, `logging_target_bucket`, `logging_prefix`, `inventory_enabled`, `inventory_schedule`, `inventory_target_bucket`, `inventory_prefix`, `inventory_format`, `cdn_force_download`, `tier_preference` (auto/performance/standard/archive, default auto)
- **object_head_cache** — HEAD request cache (size, ETag, content-type, backend_name stored on PUT); `tags` JSONB holds per-object S3 tags (separate from `metadata`); `content_disposition` TEXT holds the stored Content-Disposition response header
- **user_mfa** — `user_id (PK)`, `secret`, `enabled`, `backup_codes` (JSON), `created_at`, `updated_at` — TOTP 2FA settings
- **mfa_audit_log** — `id (SERIAL)`, `user_id`, `action`, `success`, `ip_address`, `user_agent`, `created_at`
- **object_versions** — `(tenant_id, bucket, object_key, version_id) PK`, `size_bytes`, `etag`, `content_type`, `is_latest`, `is_delete_marker`, `backend_name`, `created_at`
- **bucket_notifications** — `id (PK)`, `tenant_id`, `bucket`, `event_filter`, `target_type`, `target_url`, `enabled`, `created_at`
- **object_locks** — `(tenant_id, bucket, object_key) PK`, `retention_mode`, `retain_until_date`, `legal_hold`, `created_at`, `updated_at`
- **idempotency_cache** — `(tenant_id, idempotency_key) PK`, `method`, `path`, `response_status`, `response_headers` (JSONB), `response_body` (BYTEA), `created_at` — 24h TTL, hourly cleanup
- **sts_tokens** — `access_key (PK)`, `secret_key`, `tenant_id`, `parent_key_id`, `permissions` (JSONB), `bucket_scope` (TEXT[]), `ip_restrict` (TEXT[]), `expires_at`, `created_at` — short-lived S3 creds, hourly cleanup
- **events** — `id (PK)`, `type`, `tenant_id`, `data` (JSONB), `created_at` — persistent event log
- **webhook_endpoints** — `id (PK)`, `tenant_id`, `url`, `event_filter` (TEXT[]), `secret`, `enabled`, `created_at`, `updated_at` — webhook subscriptions
- **webhook_deliveries** — `id (PK)`, `webhook_id → webhook_endpoints`, `event_id → events`, `status`, `response_code`, `response_body`, `latency_ms`, `retry_count`, `next_retry_at`, `created_at` — delivery tracking
- **cdn_access_log** — `id (BIGSERIAL PK)`, `tenant_id`, `bucket`, `object_key`, `bytes_sent`, `country`, `referer`, `accessed_at` — raw CDN access events
- **cdn_stats_daily** — `(tenant_id, bucket, date) PK`, `requests`, `bytes_sent`, `unique_objects` — daily CDN rollup
- **account_exports** — `id (UUID PK)`, `user_id → users`, `tenant_id`, `status` (pending/processing/completed/failed), `format`, `file_path`, `file_size_bytes`, `error_message`, `created_at`, `completed_at`, `expires_at` — GDPR data export tracking
- **s3_access_log** — `id (BIGSERIAL PK)`, `tenant_id`, `bucket`, `object_key`, `operation`, `status_code`, `bytes_sent`, `bytes_received`, `source_ip`, `user_agent`, `request_id`, `error_code`, `logged_at` — buffered S3 access events, delivered as log objects to target buckets
- **metered_usage_reports** — `id (BIGSERIAL PK)`, `tenant_id`, `meter`, `period_date`, `value`, `stripe_event_id`, `reported_at`, `UNIQUE(tenant_id, meter, period_date)` — daily Stripe Billing Meter reports; the unique constraint is the no-double-billing guard and also gates once-per-month spending-cap alerts (synthetic `alert:80`/`alert:95` meters)
- **admin_notes** — `id (BIGSERIAL PK)`, `tenant_id`, `admin_user_id → users`, `note`, `created_at` — internal support notes on customer accounts, indexed by (tenant_id, created_at DESC)
- **admin_notifications** — `id (BIGSERIAL PK)`, `type`, `message`, `tenant_id` (nullable), `read_at` (nullable), `created_at` — admin notification system; partial index on `(created_at DESC) WHERE read_at IS NULL` for fast unread count
- **abuse_reports** — `id (BIGSERIAL PK)`, `reporter_email`, `reporter_name`, `tenant_id`, `bucket`, `object_key`, `report_type`, `description`, `url`, `status`, `resolved_by`, `resolved_at`, `created_at` — public abuse report intake + admin moderation
- **object_locations** — `(tenant_id, bucket, object_key) PK`, `backend_name`, `storage_class`, `size_bytes`, `stored_at`, `last_accessed` — durable object-to-backend routing; source of truth for which backend holds each object
- **tiering_policies** — `id (BIGSERIAL PK)`, `tenant_id`, `bucket`, `min_age_days`, `target_backend`, `target_class`, `enabled`, `created_at`, `UNIQUE(tenant_id, bucket, target_backend)` — age-based tier migration config (Phase 7.3)
- **tenant_cost_daily** — `(tenant_id, date, backend_name) PK`, `storage_bytes`, `cost_microcents` — daily per-tenant storage cost rollup (Phase 7.4)

## Connection

`NewPostgres(cfg, logger)` returns a `*Postgres` with a `DB() *sql.DB` accessor. Config comes from `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD` env vars.

Pool settings: `MaxOpenConns=50`, `MaxIdleConns=25`, `ConnMaxLifetime=5m`, `ConnMaxIdleTime=1m`. Sized for 100+ concurrent S3 requests (each runs 5-6 DB queries through the auth + head-cache path).
