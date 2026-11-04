-- Migration 126: one scrub finding per (tenant, address, verdict, file)
-- (Stage 1a Prompt 59 Part 0, RP96-15 of docs/reviews/WP-STRIPE-10-REVIEW.md).
--
-- The blob scrub wrote a row per cycle for every blob still wrong (× up to
-- 1,000 for a chunk referenced by many files), for ever. A finding found
-- again is now the same row: found_at = the LAST sighting (the bucket
-- page's verified line reads it, its query unchanged), first_found_at = the
-- first, times = how many cycles found it.
--
-- Additive and idempotent (the deploy re-applies every file): the columns
-- IF NOT EXISTS; duplicates folded into their newest row before the unique
-- index (a no-op once it exists). The previous build's plain INSERT of a
-- duplicate fails on the index during a switch: a note on its run, the row
-- already there.

ALTER TABLE scrub_findings ADD COLUMN IF NOT EXISTS first_found_at TIMESTAMPTZ;
ALTER TABLE scrub_findings ADD COLUMN IF NOT EXISTS times INTEGER NOT NULL DEFAULT 1;
UPDATE scrub_findings SET first_found_at = found_at WHERE first_found_at IS NULL;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'uq_scrub_findings_once') THEN
        WITH grouped AS (
            SELECT id,
                   MAX(id) OVER w AS keep,
                   MIN(COALESCE(first_found_at, found_at)) OVER w AS first,
                   COUNT(*) OVER w AS n
              FROM scrub_findings
            WINDOW w AS (PARTITION BY tenant_id, address, verdict, bucket, object_key)
        ), upd AS (
            UPDATE scrub_findings s SET first_found_at = g.first, times = g.n
              FROM grouped g WHERE s.id = g.id AND g.id = g.keep AND g.n > 1
        )
        DELETE FROM scrub_findings s USING grouped g WHERE s.id = g.id AND g.id <> g.keep;
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_scrub_findings_once
    ON scrub_findings (tenant_id, address, verdict, bucket, object_key);
