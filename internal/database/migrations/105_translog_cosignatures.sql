-- 105: the transparency log's collected cosignatures (K4c-2, Stage 1a
-- Prompt 34b; plan Phase 43.6: "at least one external witness co-signing
-- every checkpoint"; docs/TRANSPARENCY.md § The witness).
--
-- After the translog_checkpoint job signs, it submits the checkpoint to every
-- witness of TRANSLOG_WITNESSES (c2sp.org/tlog-witness add-checkpoint). Each
-- answer is verified against that witness's pinned cosignature/v1 key
-- (c2sp.org/tlog-cosignature) and every verified line is kept here; the read
-- side serves GET /translog/checkpoint with the newest line of each witness
-- appended (extra signature lines, c2sp.org/signed-note).
--
-- translog_cosignatures  one row per verified cosignature line:
--   checkpoint_size  the checkpoint it signs (translog_checkpoints.size)
--   witness          the operator's name for the witness (TRANSLOG_WITNESSES; metrics label)
--   key_id           the cosigner's key: "<key name>+<8 hex key ID>" (stable across a rename)
--   line             the signature line exactly as served: "— <name> <base64>\n"
--   cosigned_at      the cosignature's own timestamp (seconds, from the line)
--   received_at      when the log received it
--
-- A later cosignature of the same checkpoint by the same witness (a refresh,
-- the job's 6 h rule) is a new row; the read side serves the newest. Public
-- INFRASTRUCTURE like the log's other tables: no tenant or user column, never
-- erased, never exported, never pruned (internal/account
-- TestTranslogTablesAreInfrastructure), and append-only by migration 103's
-- trigger function (UPDATE/DELETE/TRUNCATE raise). Additive and idempotent:
-- re-applied on every deploy; the previous build never names this table.

SET lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS translog_cosignatures (
    checkpoint_size BIGINT NOT NULL REFERENCES translog_checkpoints (size),
    witness         TEXT NOT NULL CHECK (witness ~ '^[a-z0-9][a-z0-9._-]{0,62}$'),
    key_id          TEXT NOT NULL CHECK (key_id ~ '^[^[:space:]+]+\+[0-9a-f]{8}$'),
    line            TEXT NOT NULL CHECK (line LIKE '— % %' AND right(line, 1) = E'\n' AND octet_length(line) <= 8192),
    cosigned_at     TIMESTAMPTZ NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One row per distinct line: an ML-DSA-44 line (~3.3 KB) is past a btree
-- row's limit, so the uniqueness is on its digest (a line is kept only
-- after it verified under a pinned key; md5 is a dedup key here, nothing more).
CREATE UNIQUE INDEX IF NOT EXISTS idx_translog_cosignatures_line
    ON translog_cosignatures (checkpoint_size, md5(line));
CREATE INDEX IF NOT EXISTS idx_translog_cosignatures_key
    ON translog_cosignatures (key_id, checkpoint_size DESC, cosigned_at DESC);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'translog_cosignatures_append_only'
                    AND tgrelid = 'translog_cosignatures'::regclass) THEN
        CREATE TRIGGER translog_cosignatures_append_only BEFORE UPDATE OR DELETE ON translog_cosignatures
            FOR EACH ROW EXECUTE FUNCTION translog_append_only();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'translog_cosignatures_no_truncate'
                    AND tgrelid = 'translog_cosignatures'::regclass) THEN
        CREATE TRIGGER translog_cosignatures_no_truncate BEFORE TRUNCATE ON translog_cosignatures
            FOR EACH STATEMENT EXECUTE FUNCTION translog_append_only();
    END IF;
END;
$$;
