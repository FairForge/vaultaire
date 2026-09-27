-- 066_bucket_region_default.sql: the bucket region default becomes the
-- primary's real region (Review R7-01 / WP-R7-1).
--
-- Migration 039 defaulted buckets.region to 'us-west-1', a region id the iDrive
-- reseller account does not have. Every such bucket was in fact stored by the
-- PRIMARY driver (us-central-1, Dallas); the label was never a placement.
-- Relabel those rows and move the column default so new rows are honest.
-- Idempotent: the UPDATE matches nothing on a second run.
UPDATE buckets SET region = 'us-central-1' WHERE region = 'us-west-1';
ALTER TABLE buckets ALTER COLUMN region SET DEFAULT 'us-central-1';
