-- 125: the blob scrub's findings (WP-VAULT-2 Part 2, Stage 1a Prompt 58 PR A).
--
-- The blob_scrub job (internal/api/scrub.go) reads every hot blob WHOLE on
-- its recorded backend once per SCRUB_CYCLE — head rows with bytes, retained
-- versions, ready chunk blobs — and verifies it against what its row
-- recorded. Only what is not ok is written here: a mismatch, an unreadable
-- or missing blob, and a repair from a second copy (verdict 'repaired', with
-- the copy's leg in `source`). An `unverifiable` blob (a multipart ETag with
-- no body_md5, a sealed object whose stored bytes' MD5 was never recorded)
-- is counted in the job's result and its metric, never written.
--
-- A chunk is shared: its finding is written once per object that references
-- it (tenant_chunk_refs), so every row names a tenant, a bucket and a key.
--
-- Tenant data: erased with the account (account.EraseRows), never exported
-- (a finding is ours — what we checked and repaired, not what the customer
-- stored). The cursor of the walk is the job's own job_runs.result: no
-- column of its own.

CREATE TABLE IF NOT EXISTS scrub_findings (
    id         BIGSERIAL PRIMARY KEY,
    tenant_id  TEXT NOT NULL,
    bucket     TEXT NOT NULL,
    object_key TEXT NOT NULL,
    address    TEXT NOT NULL,
    backend    TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('whole', 'version', 'chunk')),
    verdict    TEXT NOT NULL CHECK (verdict IN ('ok', 'mismatch', 'unreadable', 'missing', 'unverifiable', 'repaired')),
    source     TEXT,
    detail     TEXT NOT NULL DEFAULT '',
    found_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_scrub_findings_bucket ON scrub_findings (tenant_id, bucket, found_at);
