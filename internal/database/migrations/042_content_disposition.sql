-- 042_content_disposition.sql: Stored Content-Disposition response header + CDN force-download toggle.

-- Content-Disposition is a stored response header (like content_type), distinct
-- from metadata (x-amz-meta-*) and tags. Set on PUT, returned on GET/HEAD, and
-- overridable per-request via ?response-content-disposition.
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS content_disposition TEXT NOT NULL DEFAULT '';

-- When TRUE, the CDN forces "attachment" disposition for all objects in the
-- bucket regardless of content type — useful for download-only buckets.
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS cdn_force_download BOOLEAN NOT NULL DEFAULT FALSE;
