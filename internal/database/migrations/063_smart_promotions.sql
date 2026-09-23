-- 063: Smart-tier read-time promotion state on the demotion ledger (Phase 5.15.8, PR B).
--
-- promote_requested_at: a read of a demoted object whose hot copy is already
--   reclaimed asked for a copy back to the hot backend (async; the daily job
--   retries anything left over).
-- restore_requested_at: the cold backend had evicted the object to tape, so a
--   RestoreObject was submitted on the reader's behalf (auto-restore); the
--   copy back completes once the restore lands.
ALTER TABLE smart_demotions ADD COLUMN IF NOT EXISTS promote_requested_at TIMESTAMPTZ;
ALTER TABLE smart_demotions ADD COLUMN IF NOT EXISTS restore_requested_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_smart_demotions_promote_pending
    ON smart_demotions (promote_requested_at)
    WHERE promote_requested_at IS NOT NULL;
