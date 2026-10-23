-- 104: the per-tenant salt of the transparency log's admin-read entries
-- (K4c-1, Stage 1a Prompt 33; docs/ARCHITECTURE_CONTRACT.md rule 7 — "an
-- admin read of customer metadata" writes to the log; docs/TRANSPARENCY.md
-- § Admin reads).
--
-- An admin_read entry is PUBLIC and permanent. It names no tenant: it carries
-- a random nonce and commitment = HMAC-SHA256(salt, "stored.cloud/translog/
-- admin_read" ‖ nonce), where salt is this table's row for the tenant read.
-- The public sees how many admin reads happened and of which scope, but
-- cannot link two entries to one tenant; the tenant (the server, for it) can
-- find its own entries by recomputing the HMAC; after an erasure — the row
-- is deleted by account.EraseRows — no one can link past entries, the
-- entries' coalescing keys included (they are keyed by the same salt).
--
-- translog_subjects   one row per tenant that was ever read by an admin;
--                     created at the first read. Tenant data: erased with the
--                     account, excluded from the GDPR export (key material,
--                     never shown — the tenant is given the entries it finds).
-- Additive: the previous build never reads it.
CREATE TABLE IF NOT EXISTS translog_subjects (
    tenant_id  TEXT        PRIMARY KEY,
    salt       BYTEA       NOT NULL CHECK (octet_length(salt) = 32),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
