-- 090: retired_blobs looked up by address (WP-R6-1 Part A).
--
-- A DELETE removes every known copy of the blob it deletes (R6-24), and a
-- retired_blobs row naming the same address on another backend is one (a
-- CompleteMultipartUpload retry that reused its recorded address after the
-- first attempt's blob was queued as an orphan elsewhere). The primary key
-- leads with backend_id, so the per-DELETE lookup by (container,
-- storage_key) needs its own index. Additive: the previous build ignores it.
CREATE INDEX IF NOT EXISTS idx_retired_blobs_address ON retired_blobs (container, storage_key);
