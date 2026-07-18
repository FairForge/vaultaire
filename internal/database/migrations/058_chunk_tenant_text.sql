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

-- Bounded wait: the ALTERs take ACCESS EXCLUSIVE and run while the previous
-- binary is still serving (deploys migrate before the binary swap). The
-- tables are empty-to-tiny wherever this conversion actually fires, so the
-- rewrite is milliseconds — the timeout only guards against queueing behind
-- a long-running query. On timeout the deploy fails before the swap (safe).
SET lock_timeout = '5s';

-- Note on values: the UUID column stored canonical lowercase text. Real
-- tenant IDs ("tenant-<hex>") are already lowercase; UUID-string tenants
-- were only ever minted via uuid.New().String() (canonical lowercase), so
-- the switch to exact string comparison changes no existing lookups.
DO $$
BEGIN
    IF (SELECT data_type FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'tenant_chunk_refs' AND column_name = 'tenant_id') = 'uuid' THEN
        ALTER TABLE tenant_chunk_refs ALTER COLUMN tenant_id TYPE TEXT USING tenant_id::text;
    END IF;
    IF (SELECT data_type FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'object_metadata' AND column_name = 'tenant_id') = 'uuid' THEN
        ALTER TABLE object_metadata ALTER COLUMN tenant_id TYPE TEXT USING tenant_id::text;
    END IF;
END $$;

RESET lock_timeout;

-- 051 (pre-WP-C) created the UUID overload; 051 now creates the TEXT one.
-- Drop the stale UUID overload so callers bind unambiguously.
DROP FUNCTION IF EXISTS get_tenant_dedup_ratio(UUID);
