-- Phase 5.11.5: STS Temporary Credentials
-- Short-lived S3 credentials with scoped-down permissions.
-- Secret stored in plaintext (required for SigV4 signature verification).

CREATE TABLE IF NOT EXISTS sts_tokens (
    access_key  VARCHAR(64)   PRIMARY KEY,
    secret_key  TEXT          NOT NULL,
    tenant_id   TEXT          NOT NULL,
    parent_key_id TEXT        NOT NULL,
    permissions JSONB         NOT NULL DEFAULT '["*"]',
    bucket_scope TEXT[]       NOT NULL DEFAULT '{}',
    ip_restrict  TEXT[]       NOT NULL DEFAULT '{}',
    expires_at  TIMESTAMPTZ   NOT NULL,
    created_at  TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_sts_tokens_expires ON sts_tokens(expires_at);
CREATE INDEX IF NOT EXISTS idx_sts_tokens_tenant ON sts_tokens(tenant_id);
