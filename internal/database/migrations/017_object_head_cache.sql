-- Migration: 017_object_head_cache.sql
-- Purpose: Lightweight cache for HEAD requests and ETag storage.
-- Avoids fetching full objects from backends for HEAD operations.
-- Uses TEXT for tenant_id (not UUID) to match the auth system's
-- "tenant-xxx" style IDs.

CREATE TABLE IF NOT EXISTS object_head_cache (
    tenant_id   TEXT NOT NULL,
    bucket      TEXT NOT NULL,
    object_key  TEXT NOT NULL,
    size_bytes  BIGINT NOT NULL,
    etag        TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, bucket, object_key)
);

CREATE INDEX IF NOT EXISTS idx_ohc_lookup
    ON object_head_cache(tenant_id, bucket, object_key);
