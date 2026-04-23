-- 028_object_lock.sql
-- Phase 5.10.13: S3 Object Lock / WORM support
-- Idempotent — safe to re-run on every deploy

-- Per-bucket object lock configuration
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS object_lock_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS default_retention_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS default_retention_days INT NOT NULL DEFAULT 0;

-- Per-object lock state (retention + legal hold)
CREATE TABLE IF NOT EXISTS object_locks (
    tenant_id         TEXT NOT NULL,
    bucket            TEXT NOT NULL,
    object_key        TEXT NOT NULL,
    retention_mode    TEXT NOT NULL DEFAULT '',
    retain_until_date TIMESTAMPTZ,
    legal_hold        BOOLEAN NOT NULL DEFAULT FALSE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, bucket, object_key)
);
