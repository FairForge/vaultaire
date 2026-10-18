-- 093: a chunk index row has a state (WP-R8-2 B.1, Stage 1a Prompt 12).
--
-- A new chunk's store used to hold a pooled connection and the chunk's
-- advisory lock across the backend PUT (up to the PUT deadline + one retry):
-- 8 workers x 6 chunked PUTs pinned 48 of the pool's 50 connections to
-- backend latency (R8-09a). The store is now three short steps: claim the row
-- 'pending' with this writer's encoding (one short transaction under the
-- lock), PUT the blob with no lock and no connection held, flip the row to
-- 'ready' (under the lock again, only if the claim is still this writer's —
-- pending_since is the claim's identity). A pending row holds NO reference
-- (ref_count 0); the writer's reference is added by the flip. Every reader
-- (the dedup lookup, IncrementRef, the GET plan, dedup GC, key re-encrypt,
-- the chunk move, the byte counts) reads 'ready' rows only. A pending row
-- whose writer died is taken over by the next writer of the chunk once its
-- pending_since is older than the takeover window, or erased with its blob
-- by the chunk orphan sweep (B.2).
--
-- Additive: every existing row is 'ready' (the column default), and the
-- previous build, which never names these columns, keeps serving this schema
-- until the switch (its overlap with a pending row is the note's residual).

ALTER TABLE global_content_index
    ADD COLUMN IF NOT EXISTS state TEXT NOT NULL DEFAULT 'ready'
        CONSTRAINT global_content_index_state_check CHECK (state IN ('pending', 'ready'));
ALTER TABLE global_content_index ADD COLUMN IF NOT EXISTS pending_since TIMESTAMPTZ;

-- The takeover and the orphan sweep find pending rows by age.
CREATE INDEX IF NOT EXISTS idx_gci_pending ON global_content_index (pending_since) WHERE state = 'pending';
