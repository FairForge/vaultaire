-- 121_management_keys.sql
--
-- WP-KEY-1 (Stage 1a Prompt 54, plan § WP-STRIPE; D-STRIPE-29;
-- docs/ACCOUNTS.md "Principal"): the management key, the per-key and
-- per-bucket budget columns (F-39), and the audit trail's principal
-- (RP86's correction). ONE file for the whole WP, landed with PR A, so no
-- later PR of the WP edits a migration already on main (D-STRIPE-43's
-- rule). Additive: the previous build reads none of these.
--
-- management_keys: an opaque bearer key (sk_live_… for the account,
-- sk_test_… for its sandbox — the tenant's livemode at mint) a server
-- presents to /api/v1/manage and /api/v1/platform instead of a person's
-- JWT. Tied to the TENANT, never to a person: a member's removal or erasure
-- never revokes it (created_by is a plain uuid with NO foreign key — a
-- cascade from users would revoke the platform's server key with its
-- maker's erasure, RP84-1's lesson; the erasure hands it over or nulls it
-- ByUser, account.Deleted). Stored as the sha256 of the whole secret (hex,
-- unique); the secret is shown once. scopes ⊆ {manage:read, manage:write,
-- platform, apps}. s3_ceiling bounds what the key MINTS (RP82-6: its own
-- field, never a bucket scope of the key's): {"permissions": [...],
-- "bucket_scope": [...], "ip_allowlist": [...], "max_expiry_seconds": N},
-- every member optional, NULL = the account's own authority. Revoked =
-- revoked_at set; a lookup reads the row on every request (no cache).
--
-- api_keys / buckets .storage_limit_bytes, .egress_limit_bytes (F-39): a
-- scoped key's and a bucket's own caps inside the tenant's and the pool's.
-- NULL = inherit (today). The egress cap is per UTC month.
--
-- expires_at (RP88-2), and the attribution and counter columns of PR B
-- (D-RP88-2), are below.
--
-- audit_logs.principal (and the archive's): WHO acted, as text — mk:<id>
-- for a management key, the user id for a person. user_id / performed_by
-- are uuid columns, and audit.Record writes a non-uuid actor as NULL, so a
-- key's act would otherwise read as "the system did it".

CREATE TABLE IF NOT EXISTS management_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    VARCHAR(255) NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    prefix       TEXT NOT NULL CHECK (prefix IN ('sk_live_', 'sk_test_')),
    key_hash     TEXT NOT NULL UNIQUE CHECK (key_hash ~ '^[0-9a-f]{64}$'),
    -- the first characters of the secret after the prefix, for the list
    -- (never enough to use)
    hint         TEXT NOT NULL DEFAULT '',
    name         TEXT NOT NULL DEFAULT '',
    scopes       TEXT[] NOT NULL CHECK (cardinality(scopes) > 0
                                       AND scopes <@ ARRAY['manage:read', 'manage:write', 'platform', 'apps']::text[]),
    s3_ceiling   JSONB,
    created_by   UUID,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    -- RP88-2: every principal has an expiry (docs/ACCOUNTS.md § Principal;
    -- WP-AGENT-1's agent grant lives 30 days by default). NULL = none, a
    -- server key's default; the lookup refuses a key past it.
    expires_at   TIMESTAMPTZ
);

ALTER TABLE management_keys ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_management_keys_tenant ON management_keys (tenant_id);
CREATE INDEX IF NOT EXISTS idx_management_keys_created_by ON management_keys (created_by) WHERE created_by IS NOT NULL;

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS storage_limit_bytes BIGINT CHECK (storage_limit_bytes IS NULL OR storage_limit_bytes >= 0);
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS egress_limit_bytes BIGINT CHECK (egress_limit_bytes IS NULL OR egress_limit_bytes >= 0);
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS storage_limit_bytes BIGINT CHECK (storage_limit_bytes IS NULL OR storage_limit_bytes >= 0);
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS egress_limit_bytes BIGINT CHECK (egress_limit_bytes IS NULL OR egress_limit_bytes >= 0);

-- D-RP88-2 (RP88 Part 1): a key's storage cap is the bytes its writes still
-- HOLD, so a row names the key that wrote it and two counters hold the sum.
-- Columns only here; WP-KEY-1 PR B writes them.
--   written_by_key: the api_keys.id of the credential that wrote the row (an
--     STS token: its parent key's). NULL = written before 121, or by the
--     primary pair (F-39 (c): the primary is never capped), counted to no
--     key. No foreign key: a revoked key's rows still name it, and the
--     counter it leaves is released by whoever displaces or deletes them.
--   api_keys.storage_used_bytes: the bytes the key's rows hold, moved by the
--     one ledger (internal/usage) in the statement that moves the tenant's.
--   buckets.storage_used_bytes: NULL = not yet counted; seeded under the
--     bucket's lock at the first cap set and by the daily reconcile.
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS written_by_key UUID;
ALTER TABLE object_versions ADD COLUMN IF NOT EXISTS written_by_key UUID;
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS written_by_key UUID;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS storage_used_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS storage_used_bytes BIGINT;

-- F-39 (a), decided by the owner 2026-10-31: an egress cap on a key or a
-- bucket REFUSES when spent (no pacing at the key or the bucket; pacing
-- stays the tenant allowance's, D-25), with key.cap_warning events at 80
-- and 95 %. The month's bytes are a durable counter on the row, so both
-- slots and a restart see one number: egress_used_bytes counts the UTC
-- month egress_month names (another month = 0), moved by each response of
-- a capped key or bucket; egress_warned_pct is the highest step warned
-- this month (the warning's once-only guard, an UPDATE … RETURNING).
-- Columns only here; PR B writes them.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS egress_used_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS egress_month DATE;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS egress_warned_pct SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS egress_used_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS egress_month DATE;
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS egress_warned_pct SMALLINT NOT NULL DEFAULT 0;

ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS principal TEXT;
ALTER TABLE audit_logs_archive ADD COLUMN IF NOT EXISTS principal TEXT;
