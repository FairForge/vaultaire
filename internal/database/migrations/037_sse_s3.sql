-- 037_sse_s3.sql: Server-side encryption with ML-KEM-768 + AES-256-GCM
CREATE TABLE IF NOT EXISTS tenant_encryption_keys (
    tenant_id    TEXT PRIMARY KEY,
    algorithm    TEXT NOT NULL DEFAULT 'ML-KEM-768+AES-256-GCM',
    seed         BYTEA NOT NULL,
    public_key   BYTEA NOT NULL,
    key_version  INTEGER NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    rotated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE buckets ADD COLUMN IF NOT EXISTS sse_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS encryption_algorithm TEXT NOT NULL DEFAULT '';
