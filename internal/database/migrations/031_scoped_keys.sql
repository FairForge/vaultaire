-- Phase 5.11.4: Scoped API Keys
-- Adds permission scoping, bucket restrictions, IP allowlists, and expiration to API keys.
-- Defaults preserve backward compatibility: existing keys get full ["*"] access.

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS permissions JSONB NOT NULL DEFAULT '["*"]';
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS bucket_scope TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS ip_allowlist TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS secret_key TEXT;
