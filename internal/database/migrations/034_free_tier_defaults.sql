-- Phase 5.11.10: Free Tier Definition
-- New registrations default to 'free' tier with 5 GB storage.
-- Does NOT update existing rows — existing tenants keep their current tier/limits.

ALTER TABLE tenant_quotas ALTER COLUMN tier SET DEFAULT 'free';
ALTER TABLE tenant_quotas ALTER COLUMN storage_limit_bytes SET DEFAULT 5368709120;
