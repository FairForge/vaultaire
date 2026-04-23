-- 026_versioning.sql
-- Phase 5.10.11: S3 bucket versioning support
-- Idempotent — safe to re-run on every deploy

-- Per-bucket versioning status: 'disabled', 'Enabled', 'Suspended'
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS versioning_status TEXT NOT NULL DEFAULT 'disabled';

-- Version history for all objects when versioning is enabled
CREATE TABLE IF NOT EXISTS object_versions (
    tenant_id        TEXT NOT NULL,
    bucket           TEXT NOT NULL,
    object_key       TEXT NOT NULL,
    version_id       TEXT NOT NULL,
    size_bytes       BIGINT NOT NULL,
    etag             TEXT NOT NULL,
    content_type     TEXT NOT NULL DEFAULT 'application/octet-stream',
    is_latest        BOOLEAN NOT NULL DEFAULT TRUE,
    is_delete_marker BOOLEAN NOT NULL DEFAULT FALSE,
    backend_name     TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, bucket, object_key, version_id)
);

CREATE INDEX IF NOT EXISTS idx_obj_versions_latest
    ON object_versions(tenant_id, bucket, object_key) WHERE is_latest = TRUE;
