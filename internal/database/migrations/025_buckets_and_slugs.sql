-- 025_buckets_and_slugs.sql
-- Phase 5.10.4: Bucket registry + tenant slugs for CDN URLs [CD-1]
-- Idempotent — safe to re-run on every deploy

-- Tenant slugs for CDN URLs (cdn.stored.ge/{slug}/{bucket}/{key})
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS slug VARCHAR(63);
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS slug_locked BOOLEAN NOT NULL DEFAULT FALSE;
CREATE UNIQUE INDEX IF NOT EXISTS idx_tenants_slug ON tenants(slug) WHERE slug IS NOT NULL;

-- First-class bucket entity — anchors all per-bucket config
CREATE TABLE IF NOT EXISTS buckets (
    tenant_id           TEXT NOT NULL,
    name                TEXT NOT NULL,
    visibility          TEXT NOT NULL DEFAULT 'private',
    cors_origins        TEXT NOT NULL DEFAULT '*',
    cache_max_age_secs  INT  NOT NULL DEFAULT 3600,
    bandwidth_budget_bytes BIGINT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, name)
);

CREATE INDEX IF NOT EXISTS idx_buckets_public
    ON buckets (visibility) WHERE visibility = 'public-read';

-- Bug fix: backend_name is written by s3_engine_adapter.go:294 and
-- s3_copy.go:152 but was never defined in migration 017.
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS backend_name TEXT;
