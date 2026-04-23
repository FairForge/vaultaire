-- 027_bucket_notifications.sql
-- Phase 5.10.12: S3 bucket notification configuration
-- Idempotent — safe to re-run on every deploy

CREATE TABLE IF NOT EXISTS bucket_notifications (
    id              TEXT NOT NULL DEFAULT gen_random_uuid()::text,
    tenant_id       TEXT NOT NULL,
    bucket          TEXT NOT NULL,
    event_filter    TEXT NOT NULL,
    target_type     TEXT NOT NULL,
    target_url      TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_bucket_notif_lookup
    ON bucket_notifications(tenant_id, bucket) WHERE enabled = TRUE;
