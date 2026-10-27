-- 111_edge_codec.sql
--
-- WP-EDGE-2 Part A (Stage 1a Prompt 46, plan 40.10 slice 2 — the codec and
-- the control plane): a PutObject admitted at the edge of a tenant with the
-- edge_codec flag is chunked and sealed BY THE DATA-PLANE HOST (format 0x06,
-- a random per-chunk key wrapped to the tenant's codec public key) and
-- indexed by a KEYED hash — HMAC-SHA256(the tenant key version's index key,
-- sha256(chunk)) — never the plain hash (contract lines 4, 5, 6).
--
-- Additive only; the previous build ignores every column below (a codec
-- admit comes only from the new build, and the edge_codec flag defaults OFF).
--
-- global_content_index.keyed_hash: the keyed identity of a codec chunk's
-- row. A codec row carries the keyed hash hex in BOTH columns —
-- plaintext_hash (the table's identity, so every claim, reference, lock, GC
-- sweep and reconcile keyed on (dedup_scope, plaintext_hash) works
-- untouched) and keyed_hash (the typed marker: set = this row's identity is
-- keyed, no column of it is sha256(chunk); the partial unique index states
-- the constraint). NULL on every row written before and on every
-- convergent (AES256-CE) and plaintext chunk for ever.
ALTER TABLE global_content_index ADD COLUMN IF NOT EXISTS keyed_hash TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_gci_scope_keyed_hash
    ON global_content_index (dedup_scope, keyed_hash) WHERE keyed_hash IS NOT NULL;

-- edge_admits.codec: the admit answered the codec shape (no presigned PUT
-- for the body; the host chunks, asks the index, seals and commits). The
-- reaper's expiry drops the claims such an admit's index calls took
-- (attrs.codec_claims names them).
ALTER TABLE edge_admits ADD COLUMN IF NOT EXISTS codec BOOLEAN NOT NULL DEFAULT FALSE;

-- A codec multipart upload: one upload is one path (D-EDGE-3) — the first
-- codec part's admit sets it (under the upload row's lock, when no staged,
-- edge-whole or backend part exists); a staged or edge-whole part on a
-- codec upload, and a codec part on any other upload, is then refused.
-- No backend multipart upload exists for it: the parts' chunks go to the
-- chunk store and Complete concatenates the parts' chunk lists into one
-- manifest.
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS codec BOOLEAN NOT NULL DEFAULT FALSE;

-- multipart_parts.part_chunks: a codec part's ordered chunk list
-- ([{h: keyed hash hex, s: plaintext size, z: sealed size}...]), written by
-- the part's commit with its references taken; Complete concatenates them.
-- NULL on every staged and edge-whole part.
ALTER TABLE multipart_parts ADD COLUMN IF NOT EXISTS part_chunks JSONB;
