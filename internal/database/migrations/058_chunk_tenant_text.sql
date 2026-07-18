-- 058_chunk_tenant_text.sql (WP-C, item 1.12)
-- Idempotent — safe to re-run on every deploy.
--
-- tenant_chunk_refs.tenant_id and object_metadata.tenant_id were UUID columns
-- (migration 051) while every other table — and, critically, registration —
-- uses string tenant IDs ("tenant-<hex>"). The adapter therefore gated the
-- whole chunked path on uuid.Parse(tenantID) succeeding and silently fell
-- through to the plain path for every real tenant: no dedup, no compression,
-- zero is_chunked objects in prod, ever. Flip both columns to TEXT so real
-- tenants can chunk. UUID values already stored (bench tenants) survive as
-- their text form.
--
-- These tables are FIRST created by migration 016 (which sorts before 051 —
-- 051's CREATE IF NOT EXISTS has always been a no-op), so 016 and 051 both
-- carry the TEXT definition now. This migration converts databases whose
-- tables were created by the pre-fix 016 (UUID) in place, and is therefore
-- load-bearing for every database created before 2026-07-18 — not a one-time
-- backfill. The DO-block guards make re-runs free (no table rewrite when
-- already TEXT). The fresh-DB test asserts the resulting column types, so a
-- future migration reintroducing UUID here fails CI.

DO $$
BEGIN
    IF (SELECT data_type FROM information_schema.columns
        WHERE table_name = 'tenant_chunk_refs' AND column_name = 'tenant_id') = 'uuid' THEN
        ALTER TABLE tenant_chunk_refs ALTER COLUMN tenant_id TYPE TEXT USING tenant_id::text;
    END IF;
    IF (SELECT data_type FROM information_schema.columns
        WHERE table_name = 'object_metadata' AND column_name = 'tenant_id') = 'uuid' THEN
        ALTER TABLE object_metadata ALTER COLUMN tenant_id TYPE TEXT USING tenant_id::text;
    END IF;
END $$;

-- 051 (pre-WP-C) created the UUID overload; 051 now creates the TEXT one.
-- Drop the stale UUID overload so callers bind unambiguously.
DROP FUNCTION IF EXISTS get_tenant_dedup_ratio(UUID);
