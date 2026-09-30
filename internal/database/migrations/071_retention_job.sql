-- 071: Retention job support (Review R13-14 / WP-R9-2, checklist item 5).
--
-- job_runs: one row per background job, the persisted "when did this last
-- succeed" the daily runners need to catch up after a restart (every deploy
-- restarts the process and a plain 24 h ticker never fires — R13-07). The
-- retention job is the first user; WP-R13-3 moves the other daily jobs onto it.
--
-- The four indexes serve the retention job's `column < cutoff` range scans on
-- the tables that had no time index at all; the other retained tables
-- (s3_access_log via (tenant_id, bucket, logged_at) is NOT usable for a global
-- range, events/cdn_access_log/waitlist already index their timestamp).

CREATE TABLE IF NOT EXISTS job_runs (
    job              TEXT        PRIMARY KEY,
    last_started_at  TIMESTAMPTZ,
    last_finished_at TIMESTAMPTZ,
    last_success_at  TIMESTAMPTZ,
    last_outcome     TEXT        NOT NULL DEFAULT '',   -- 'ok' | 'error' | 'running'
    last_error       TEXT        NOT NULL DEFAULT '',
    rows_affected    BIGINT      NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_s3_access_log_logged_at ON s3_access_log (logged_at);
CREATE INDEX IF NOT EXISTS idx_stripe_events_processed_at ON stripe_events (processed_at);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_created_at ON webhook_deliveries (created_at);
CREATE INDEX IF NOT EXISTS idx_access_patterns_last_seen ON access_patterns (last_seen);
CREATE INDEX IF NOT EXISTS idx_quota_usage_events_timestamp ON quota_usage_events ("timestamp");
