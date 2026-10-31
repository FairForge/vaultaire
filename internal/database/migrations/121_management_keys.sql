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
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_management_keys_tenant ON management_keys (tenant_id);
CREATE INDEX IF NOT EXISTS idx_management_keys_created_by ON management_keys (created_by) WHERE created_by IS NOT NULL;

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS storage_limit_bytes BIGINT CHECK (storage_limit_bytes IS NULL OR storage_limit_bytes >= 0);
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS egress_limit_bytes BIGINT CHECK (egress_limit_bytes IS NULL OR egress_limit_bytes >= 0);
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS storage_limit_bytes BIGINT CHECK (storage_limit_bytes IS NULL OR storage_limit_bytes >= 0);
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS egress_limit_bytes BIGINT CHECK (egress_limit_bytes IS NULL OR egress_limit_bytes >= 0);

ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS principal TEXT;
ALTER TABLE audit_logs_archive ADD COLUMN IF NOT EXISTS principal TEXT;
