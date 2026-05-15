-- 035_cdn_analytics.sql
-- Phase 5.11.12: CDN access analytics and bandwidth budgets [CD-8]
-- Idempotent — safe to re-run on every deploy

CREATE TABLE IF NOT EXISTS cdn_access_log (
    id BIGSERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    bucket TEXT NOT NULL,
    object_key TEXT NOT NULL,
    bytes_sent BIGINT NOT NULL DEFAULT 0,
    country TEXT NOT NULL DEFAULT '',
    referer TEXT NOT NULL DEFAULT '',
    accessed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_cdn_access_log_tenant_bucket
    ON cdn_access_log (tenant_id, bucket, accessed_at DESC);

CREATE INDEX IF NOT EXISTS idx_cdn_access_log_accessed
    ON cdn_access_log (accessed_at);

CREATE TABLE IF NOT EXISTS cdn_stats_daily (
    tenant_id TEXT NOT NULL,
    bucket TEXT NOT NULL,
    date DATE NOT NULL DEFAULT CURRENT_DATE,
    requests BIGINT NOT NULL DEFAULT 0,
    bytes_sent BIGINT NOT NULL DEFAULT 0,
    unique_objects INT NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, bucket, date)
);
