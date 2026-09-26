-- 064: API key revocation is persisted (review R5-01).
--
-- Before this, RevokeAPIKey / RotateAPIKey only flipped an in-memory field:
-- the S3 auth path reads api_keys per request and never saw it, and a restart
-- forgot it. revoked_at is now the source of truth; both credential lookups
-- (header SigV4 and presigned) filter on revoked_at IS NULL.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_api_keys_active ON api_keys(key_id) WHERE revoked_at IS NULL;
