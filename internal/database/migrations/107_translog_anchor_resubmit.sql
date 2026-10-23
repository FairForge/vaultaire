-- 107: the anchors' re-submission state (Stage 1a Prompt 36 Part 0,
-- RP48-2; docs/TRANSPARENCY.md § Anchors).
--
-- A pending proof completes only if the calendar kept the commitment. One it
-- lost would stay pending for ever, so a pending row past the horizon (48 h;
-- Bitcoin confirms in hours) is re-submitted to the same calendar ONCE: the
-- fresh pending proof is kept as a row with state 'resubmitted' beside the
-- original. Migration 106's unique index (checkpoint_size, kind, service,
-- state) then admits exactly one re-submission per calendar and checkpoint —
-- "once" is enforced by the schema — and stays the ON CONFLICT target the
-- previous build's INSERT infers, which an attempt column in the key could
-- not keep (D-P36-2). The upgrade pass asks pending AND resubmitted rows;
-- whichever completes writes the one 'complete' row.
--
-- Additive and idempotent: the CHECK is widened (the previous build never
-- writes 'resubmitted', and its reads filter on 'pending'/'complete'); the
-- constraint is dropped and re-created on every deploy.

SET lock_timeout = '5s';

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_constraint
               WHERE conrelid = 'translog_anchors'::regclass
                 AND conname = 'translog_anchors_state_check') THEN
        ALTER TABLE translog_anchors DROP CONSTRAINT translog_anchors_state_check;
    END IF;
    ALTER TABLE translog_anchors ADD CONSTRAINT translog_anchors_state_check
        CHECK (state IN ('pending', 'complete', 'resubmitted'));
END;
$$;
