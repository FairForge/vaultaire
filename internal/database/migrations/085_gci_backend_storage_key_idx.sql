-- 085: an index for the key_reencrypt job's liveness check (2026-10-12,
-- Stage 1a Prompt 2A / WP-R8-1 follow-up A4).
--
-- deleteRetired asks, for every retired chunk address past its grace,
-- whether an index row names that address again (dedup GC swept the row and
-- a PUT re-stored the chunk there): `WHERE backend_id = $1 AND storage_key =
-- $2`. The primary key is (dedup_scope, plaintext_hash), so without this the
-- check was a sequential scan of global_content_index per retired address
-- per run. Additive; the previous build never reads it.
CREATE INDEX IF NOT EXISTS idx_gci_backend_storage_key ON global_content_index (backend_id, storage_key);
