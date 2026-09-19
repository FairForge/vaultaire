-- 061: store Content-Encoding and Content-Language on objects (A1 conformance
-- fix, WP-VG1 gap report). Mirrors content_disposition (migration 042): headers
-- a client sends on PUT must come back on GET/HEAD, or clients lose them
-- (gzip uploads lose their encoding; localized content loses its language).
-- The aws-chunked transport token is stripped from Content-Encoding first.
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS content_encoding TEXT NOT NULL DEFAULT '';
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS content_language TEXT NOT NULL DEFAULT '';
-- Same store-and-echo family: Cache-Control and Expires (raw header string,
-- hence http_expires — this is NOT an object TTL), and the
-- x-amz-website-redirect-location value (stored metadata only; no website
-- endpoint exists, so no redirect behavior is implied).
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS cache_control TEXT NOT NULL DEFAULT '';
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS http_expires TEXT NOT NULL DEFAULT '';
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS website_redirect_location TEXT NOT NULL DEFAULT '';
