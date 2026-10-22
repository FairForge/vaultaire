-- 102: the bucket's mode (keystone K3, Stage 1a Prompt 26; plan Phase 41.0,
-- docs/ARCHITECTURE_CONTRACT.md lines 1 and 3).
--
-- A mode is a property of the surface a bucket is reached through: Private
-- (keys only on the customer's side — today an SSE-C-required bucket), Standard
-- (keys we hold) or Public (plaintext, public-read). Its names and sentences
-- live in internal/api/landing/modes.json; this column is the one place the
-- database states which mode a bucket is in.
--
-- buckets.mode  GENERATED from the two columns that already decide it, so it
--               can never drift from them and the previous build — which never
--               names it — cannot leave it wrong during the two-slot overlap:
--                 visibility = 'public-read'      → 'public'
--                 ssec_required                   → 'private'
--                 otherwise ('private' visibility) → 'standard'
--               NAME TRAP (docs/reviews/K3-PROBE.md): visibility 'private'
--               means "not public-read", which is STANDARD mode.
--               public-read + ssec_required is refused by the one write door
--               (internal/modes SetBucket, the 096 rule) and would read 'public'.
--               A surface changes a bucket's mode only through that door.
--
-- Additive; the table is small (prod 2026-10-22: 7 rows) — a STORED generated
-- column rewrites it under ACCESS EXCLUSIVE, bounded by the deploy's
-- lock_timeout. Re-applied on every deploy: IF NOT EXISTS.

SET lock_timeout = '5s';

ALTER TABLE buckets ADD COLUMN IF NOT EXISTS mode TEXT GENERATED ALWAYS AS (
    CASE
        WHEN visibility = 'public-read' THEN 'public'
        WHEN ssec_required THEN 'private'
        ELSE 'standard'
    END
) STORED;

RESET lock_timeout;
