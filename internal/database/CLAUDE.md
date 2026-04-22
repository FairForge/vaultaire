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

## Key Tables

- **users** — `id (UUID)`, `email`, `password_hash`, `company`, `status`, `role`, `stripe_customer_id`
- **tenants** — `id`, `name`, `email`, `access_key`, `secret_key`, `slug`, `slug_locked`, `stripe_customer_id`, `stripe_subscription_id`, `subscription_status`, `plan`, `suspended_at`
- **api_keys** — `id (UUID)`, `user_id → users`, `name`, `key_id`, `secret_hash`
- **tenant_quotas** — `tenant_id (PK)`, `storage_limit_bytes`, `storage_used_bytes`, `tier`
- **dashboard_sessions** — `id (VARCHAR 64)`, `user_id → users`, `tenant_id → tenants`, `email`, `role`, `ip_address`, `user_agent`, `created_at`, `last_active_at`, `expires_at`
- **subscriptions** — Stripe subscription state tracking
- **bandwidth_usage_daily** — per-tenant daily ingress/egress/requests (unique on tenant_id + date)
- **buckets** — `(tenant_id, name) PK`, `visibility` (private/public-read), `cors_origins`, `cache_max_age_secs`, `bandwidth_budget_bytes`
- **object_head_cache** — HEAD request cache (size, ETag, content-type, backend_name stored on PUT)
- **user_mfa** — `user_id (PK)`, `secret`, `enabled`, `backup_codes` (JSON), `created_at`, `updated_at` — TOTP 2FA settings
- **mfa_audit_log** — `id (SERIAL)`, `user_id`, `action`, `success`, `ip_address`, `user_agent`, `created_at`

## Connection

`NewPostgres(cfg, logger)` returns a `*Postgres` with a `DB() *sql.DB` accessor. Config comes from `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD` env vars.
