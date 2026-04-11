-- 024_multipart_uploads.sql
-- Phase 5.10.1: Database-backed multipart upload tracking
-- Replaces in-memory activeUploads map with persistent storage

CREATE TABLE IF NOT EXISTS multipart_uploads (
    upload_id   TEXT        NOT NULL,
    tenant_id   TEXT        NOT NULL,
    bucket      TEXT        NOT NULL,
    object_key  TEXT        NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'active',  -- active, completed, aborted
    created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (upload_id)
);

CREATE INDEX IF NOT EXISTS idx_multipart_uploads_tenant
    ON multipart_uploads (tenant_id, bucket, status);

CREATE INDEX IF NOT EXISTS idx_multipart_uploads_cleanup
    ON multipart_uploads (status, created_at)
    WHERE status = 'active';

CREATE TABLE IF NOT EXISTS multipart_parts (
    upload_id   TEXT    NOT NULL REFERENCES multipart_uploads(upload_id) ON DELETE CASCADE,
    part_number INT     NOT NULL,
    etag        TEXT    NOT NULL,
    size_bytes  BIGINT  NOT NULL,
    created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (upload_id, part_number)
);
