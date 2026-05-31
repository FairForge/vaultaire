-- 039_bucket_region.sql: Per-bucket region selection for data residency.
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS region TEXT NOT NULL DEFAULT 'us-west-1';
