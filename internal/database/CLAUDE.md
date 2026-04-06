# internal/database

PostgreSQL connection management and migrations for Vaultaire.

## Migrations

All migrations are in `migrations/` and are idempotent (`CREATE IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`). They run on every deploy via CI/CD.

| File | Purpose |
|------|---------|
| 003-017 | Core tables (tenants, users, api_keys, audit, compliance, object_head_cache) |
| 018 | Dashboard foundation: user status/role columns, Stripe columns on tenants, subscriptions table, bandwidth_usage_daily, dashboard_sessions |

## Key Tables

- **users** — `id (UUID)`, `email`, `password_hash`, `company`, `status`, `role`, `stripe_customer_id`
- **tenants** — `id`, `name`, `email`, `access_key`, `secret_key`, `stripe_customer_id`, `stripe_subscription_id`, `subscription_status`, `plan`, `suspended_at`
- **api_keys** — `id (UUID)`, `user_id → users`, `name`, `key_id`, `secret_hash`
- **tenant_quotas** — `tenant_id (PK)`, `storage_limit_bytes`, `storage_used_bytes`, `tier`
- **dashboard_sessions** — `id (VARCHAR 64)`, `user_id → users`, `tenant_id → tenants`, `email`, `role`, `expires_at`
- **subscriptions** — Stripe subscription state tracking
- **bandwidth_usage_daily** — per-tenant daily ingress/egress/requests (unique on tenant_id + date)
- **object_head_cache** — HEAD request cache (size, ETag, content-type stored on PUT)

## Connection

`NewPostgres(cfg, logger)` returns a `*Postgres` with a `DB() *sql.DB` accessor. Config comes from `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD` env vars.
