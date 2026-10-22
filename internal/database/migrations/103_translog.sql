-- 103: the transparency log (keystone K4, Stage 1a Prompt 28; plan Phase 43.6,
-- decision F-16, docs/ARCHITECTURE_CONTRACT.md rule 7; docs/TRANSPARENCY.md).
--
-- An append-only Merkle log (internal/translog: RFC 6962 hashing by
-- golang.org/x/mod/sumdb/tlog) whose entries become PUBLIC once the
-- translog_public flag is on. INFRASTRUCTURE, never tenant data: no tenant or
-- user column (an entry is a typed struct with an allow-list of fields,
-- internal/translog/entries.go), so account.EraseRows and the GDPR export
-- never walk them (TestTranslogTablesAreInfrastructure pins it), and the
-- retention job never names them.
--
-- translog_entries      one row per entry: idx 0, 1, 2, … with no gap (the
--                       appender holds pg_advisory_xact_lock on 'translog');
--                       body = the exact bytes hashed (≤ 65535: a tile-bundle
--                       entry has a uint16 length); dedup_key lets a producer
--                       append once per fact (a server build: git sha +
--                       binary sha256) — NULL = no dedup.
-- translog_hashes       the stored hashes tlog needs, by tlog storage index
--                       (tlog.StoredHashIndex(level, n)): written with the
--                       entry in the same transaction.
-- translog_checkpoints  every signed checkpoint (a C2SP signed note), by tree
--                       size; the newest is GET /translog/checkpoint.
--
-- All three refuse UPDATE, DELETE and TRUNCATE (translog_append_only): a
-- rewrite would be detected by a consistency proof anyway; the trigger makes
-- it a loud error instead of a silent one. Additive and idempotent: re-applied
-- on every deploy; the previous build never names these tables.

SET lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS translog_entries (
    idx        BIGINT PRIMARY KEY CHECK (idx >= 0),
    kind       TEXT NOT NULL,
    body       BYTEA NOT NULL CHECK (octet_length(body) <= 65535),
    dedup_key  TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_translog_entries_dedup
    ON translog_entries (kind, dedup_key) WHERE dedup_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS translog_hashes (
    idx  BIGINT PRIMARY KEY CHECK (idx >= 0),
    hash BYTEA NOT NULL CHECK (octet_length(hash) = 32)
);

CREATE TABLE IF NOT EXISTS translog_checkpoints (
    size      BIGINT PRIMARY KEY CHECK (size >= 0),
    root      BYTEA NOT NULL CHECK (octet_length(root) = 32),
    note      TEXT NOT NULL,
    signed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION translog_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'translog: % is append-only (% refused)', TG_TABLE_NAME, TG_OP;
END;
$$;

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['translog_entries', 'translog_hashes', 'translog_checkpoints'] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = t || '_append_only' AND tgrelid = t::regclass) THEN
            EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION translog_append_only()',
                           t || '_append_only', t);
        END IF;
        IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = t || '_no_truncate' AND tgrelid = t::regclass) THEN
            EXECUTE format('CREATE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION translog_append_only()',
                           t || '_no_truncate', t);
        END IF;
    END LOOP;
END;
$$;
