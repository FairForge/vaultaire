-- 040_access_logging.sql: S3 server access logging and inventory report configuration.

-- Per-bucket logging configuration.
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS logging_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS logging_target_bucket TEXT;
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS logging_prefix TEXT NOT NULL DEFAULT '';

-- Per-bucket inventory report configuration.
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS inventory_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS inventory_schedule TEXT NOT NULL DEFAULT 'daily';
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS inventory_target_bucket TEXT;
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS inventory_prefix TEXT NOT NULL DEFAULT '';
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS inventory_format TEXT NOT NULL DEFAULT 'csv';

-- S3 access log table: buffered writes from S3AccessLogTracker, delivered as
-- log objects to the target bucket by the log delivery goroutine.
CREATE TABLE IF NOT EXISTS s3_access_log (
    id BIGSERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    bucket TEXT NOT NULL,
    object_key TEXT NOT NULL DEFAULT '',
    operation TEXT NOT NULL,
    status_code INTEGER NOT NULL,
    bytes_sent BIGINT NOT NULL DEFAULT 0,
    bytes_received BIGINT NOT NULL DEFAULT 0,
    source_ip TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    error_code TEXT NOT NULL DEFAULT '',
    logged_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_s3_access_log_tenant_bucket ON s3_access_log (tenant_id, bucket, logged_at);
