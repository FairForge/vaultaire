-- 122: Stage 1a Prompt 55 — one file for the prompt (D-STRIPE-43's rule).
--
-- Part 0 (RP88 Part 2, RB12-9): who wrote a row is looked up per key by
-- usage.KeyHeldBytes, the reconcile's ReassertHolders (a correlated sum for
-- every key row) and the drift gauge. Partial indexes: only rows a scoped key
-- wrote carry the column (the primary's and the background writes' are NULL),
-- so the index is the size of what is counted. CONCURRENTLY: the migration set
-- runs statement-by-statement outside a transaction (internal/database/CLAUDE.md).
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_head_cache_written_by_key
    ON object_head_cache (written_by_key) WHERE written_by_key IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_versions_written_by_key
    ON object_versions (written_by_key) WHERE written_by_key IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_multipart_uploads_written_by_key
    ON multipart_uploads (written_by_key) WHERE written_by_key IS NOT NULL;
