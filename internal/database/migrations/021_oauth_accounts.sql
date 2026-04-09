-- 021_oauth_accounts.sql
-- OAuth provider accounts linked to users.
-- Idempotent — safe to re-run on every deploy.

-- Allow users to authenticate via external OAuth providers (Google, GitHub, etc).
-- Separate table so one user can link multiple providers.
CREATE TABLE IF NOT EXISTS oauth_accounts (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL,
    provider    VARCHAR(50) NOT NULL,       -- 'google', 'github'
    provider_id VARCHAR(255) NOT NULL,      -- provider's unique user ID
    email       VARCHAR(255),               -- email from provider (informational)
    name        VARCHAR(255),               -- display name from provider
    created_at  TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE (provider, provider_id)
);

CREATE INDEX IF NOT EXISTS idx_oauth_accounts_user
    ON oauth_accounts (user_id);
