-- 106: the transparency log's anchors (K4c-3a, Stage 1a Prompt 35; plan
-- Phase 43.6 / decision F-16: "signed statements + OpenTimestamps + a Rekor
-- mirror"; docs/TRANSPARENCY.md § Anchors).
--
-- The translog_anchor job submits every signed checkpoint to each service of
-- TRANSLOG_ANCHORS and keeps what it answers: a public, independent timestamp
-- of the checkpoint nobody has to approve. The digest anchored is SHA-256 of
-- the checkpoint note exactly as the log signed it (translog_checkpoints.note:
-- the body and the log's signature line, no cosignature lines).
--
-- translog_anchors  one row per answer kept:
--   checkpoint_size  the checkpoint anchored (translog_checkpoints.size)
--   kind             'ots' (an OpenTimestamps calendar) or 'rekor' (a Rekor
--                    log; not built — D-P35-2: Rekor v2 accepts only
--                    prehashed signatures, the log's key signs nothing else)
--   service          the calendar's (or log's) URL, as configured
--   state            'pending' (a calendar's promise) or 'complete' (the
--                    proof reaches a Bitcoin block header attestation)
--   proof            the proof as served: for ots a whole .ots file (the
--                    calendar's bytes, verbatim, after the file header)
--   digest           the 32-byte SHA-256 anchored
--   received_at      when the log received it
--
-- An OTS upgrade is a NEW row with state 'complete', never an UPDATE: the
-- pending row stays. Public INFRASTRUCTURE like the log's other tables: no
-- tenant or user column, never erased, never exported, never pruned
-- (internal/account TestTranslogTablesAreInfrastructure), and append-only by
-- migration 103's trigger function (UPDATE/DELETE/TRUNCATE raise). Additive
-- and idempotent: re-applied on every deploy; the previous build never names
-- this table.

SET lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS translog_anchors (
    checkpoint_size BIGINT NOT NULL REFERENCES translog_checkpoints (size),
    kind            TEXT NOT NULL CHECK (kind IN ('ots', 'rekor')),
    service         TEXT NOT NULL CHECK (service ~ '^https?://[^[:space:]]+$' AND length(service) <= 512),
    state           TEXT NOT NULL CHECK (state IN ('pending', 'complete')),
    proof           BYTEA NOT NULL CHECK (octet_length(proof) BETWEEN 1 AND 65536),
    digest          BYTEA NOT NULL CHECK (octet_length(digest) = 32),
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One pending and one complete answer per checkpoint and service: a retry
-- of a submission or an upgrade keeps the first.
CREATE UNIQUE INDEX IF NOT EXISTS idx_translog_anchors_one
    ON translog_anchors (checkpoint_size, kind, service, state);
CREATE INDEX IF NOT EXISTS idx_translog_anchors_pending
    ON translog_anchors (kind, service, received_at) WHERE state = 'pending';

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'translog_anchors_append_only'
                    AND tgrelid = 'translog_anchors'::regclass) THEN
        CREATE TRIGGER translog_anchors_append_only BEFORE UPDATE OR DELETE ON translog_anchors
            FOR EACH ROW EXECUTE FUNCTION translog_append_only();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'translog_anchors_no_truncate'
                    AND tgrelid = 'translog_anchors'::regclass) THEN
        CREATE TRIGGER translog_anchors_no_truncate BEFORE TRUNCATE ON translog_anchors
            FOR EACH STATEMENT EXECUTE FUNCTION translog_append_only();
    END IF;
END;
$$;
