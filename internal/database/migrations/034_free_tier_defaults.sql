-- Phase 5.11.10: Free Tier Definition
-- New registrations default to 'free' tier with 5 GB storage.
-- Does NOT update existing rows — existing tenants keep their current tier/limits.
--
-- WP-8: guarded — on a FRESH database tenant_quotas doesn't exist yet (it is
-- created by migration 056 with these defaults already baked in), and an
-- unguarded ALTER here aborts the whole set under ON_ERROR_STOP=1. On existing
-- databases the ALTERs still apply as before.

DO $$
BEGIN
    IF to_regclass('tenant_quotas') IS NOT NULL THEN
        ALTER TABLE tenant_quotas ALTER COLUMN tier SET DEFAULT 'free';
        ALTER TABLE tenant_quotas ALTER COLUMN storage_limit_bytes SET DEFAULT 5368709120;
    END IF;
END $$;
