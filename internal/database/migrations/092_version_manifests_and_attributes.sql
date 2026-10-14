-- 092: version-aware manifests (WP-VM-1, Stage 1a Prompt 7) and the
-- attributes a version keeps (R61-8).
--
-- A chunked object's manifest gets an address, as a whole blob did in 086/087:
-- newBlobAddress(key) = <key>@<26 base32>, recorded in the head row's and the
-- version row's storage_key (backend_name stays the 'chunked' sentinel). The
-- address is written in the EXISTING object_key column of object_metadata and
-- tenant_chunk_refs: their unique constraints — UNIQUE(tenant_id,
-- bucket_name, object_key) and UNIQUE(tenant_id, bucket_name, object_key,
-- chunk_index), which the previous build's ON CONFLICT names — are not
-- touched, and the column (VARCHAR(1024)) holds the longest key a write
-- accepts (857 bytes, blob_address.go) plus the 27-byte suffix. A manifest
-- written before this build stays keyed by its object key, its head row's
-- storage_key NULL, and is read by key for ever. So this migration needs no
-- column for the manifest at all.
--
-- What it adds is R61-8: a version row carries what a GET of that version
-- sends — user metadata, Content-Disposition / -Encoding / -Language,
-- Cache-Control, Expires and the website redirect — filled by every version
-- write (the content type was already there). NULL = a version written before
-- this build: a GET of it sends none of them, as before.
--
-- Additive only: the previous build keeps serving this schema until the
-- switch and never names these columns.

ALTER TABLE object_versions ADD COLUMN IF NOT EXISTS metadata JSONB;
ALTER TABLE object_versions ADD COLUMN IF NOT EXISTS content_disposition TEXT;
ALTER TABLE object_versions ADD COLUMN IF NOT EXISTS content_encoding TEXT;
ALTER TABLE object_versions ADD COLUMN IF NOT EXISTS content_language TEXT;
ALTER TABLE object_versions ADD COLUMN IF NOT EXISTS cache_control TEXT;
ALTER TABLE object_versions ADD COLUMN IF NOT EXISTS http_expires TEXT;
ALTER TABLE object_versions ADD COLUMN IF NOT EXISTS website_redirect_location TEXT;

-- The collector's orphan pass looks for addressed manifests no row names,
-- oldest first (retired_blobs.go): object_metadata by its age.
CREATE INDEX IF NOT EXISTS idx_object_metadata_updated_at ON object_metadata (updated_at);
