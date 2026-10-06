-- 078: CORS on the S3 API (queue item 2 of docs/STATUS.md, 2026-10-06).
--
-- Until now `?cors` answered 501 and a browser preflight against the S3
-- endpoint got a 403 with no CORS headers, so no web application could use
-- the S3 API directly (the only CORS was the /cdn path's cors_origins). The
-- bucket's CORS configuration is stored here in the AWS shape — a JSON array
-- of rules, each {id, allowed_origins, allowed_methods, allowed_headers,
-- expose_headers, max_age_seconds} — written by PutBucketCors, read by
-- GetBucketCors, the OPTIONS preflight (answered before SigV4, for the
-- bucket named in the path) and the actual-response headers. NULL = no
-- configuration (GetBucketCors answers 404 NoSuchCORSConfiguration, every
-- cross-origin browser request is refused) — the default for every bucket.
-- cors_origins stays what it was: the /cdn path's allow-list.
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS cors_rules JSONB;
COMMENT ON COLUMN buckets.cors_rules IS 'S3 CORS configuration (PutBucketCors): JSON array of rules in the AWS shape; NULL = none (migration 078)';
