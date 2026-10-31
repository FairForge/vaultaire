-- 122: Stage 1a Prompt 55 — one file for the prompt (D-STRIPE-43's rule).
--
-- Part 0 (RP88 Part 2, RB12-9): who wrote a row is looked up per key by
-- usage.KeyHeldBytes, the reconcile's ReassertHolders (a correlated sum for
-- every key row) and the drift gauge. Partial indexes: only rows a scoped key
-- wrote carry the column (the primary's and the background writes' are NULL),
-- so the index is the size of what is counted. Not CONCURRENTLY: a file of more
-- than one statement runs as one batch (an implicit transaction, which
-- CONCURRENTLY refuses; 069 is a one-statement file), and prompt 55 keeps one
-- file (D-STRIPE-43). The three tables are small on prod (thousands of rows),
-- the build is milliseconds, and the deploy's lock_timeout bounds the wait.
CREATE INDEX IF NOT EXISTS idx_object_head_cache_written_by_key
    ON object_head_cache (written_by_key) WHERE written_by_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_object_versions_written_by_key
    ON object_versions (written_by_key) WHERE written_by_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_multipart_uploads_written_by_key
    ON multipart_uploads (written_by_key) WHERE written_by_key IS NOT NULL;

-- PR A (WP-AGENT-1): agents as principals (docs/ACCOUNTS.md § Agents; F-41).
-- An agent grant IS a management key (WP-KEY-1) with a person behind it: the
-- grant row names the key (and its sandbox test key, RP88-1's rehearsal),
-- the agent, the OAuth client that connected it, the member who granted it
-- (granted_by: a plain uuid, no foreign key — RP84-1's lesson; the erasure
-- revokes the grant's keys and nulls it, account.Deleted), the scope the
-- consent approved and the caps every S3 key it mints carries. The key's
-- expires_at is the grant's lifetime (30 days by default; a renewal pushes
-- it and never widens anything). Additive: the previous build reads none of
-- this, and a grant's key is to it a plain manage:write key with an expiry.
CREATE TABLE IF NOT EXISTS agent_grants (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              VARCHAR(255) NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    management_key_id      UUID NOT NULL UNIQUE REFERENCES management_keys(id) ON DELETE CASCADE,
    test_management_key_id UUID REFERENCES management_keys(id) ON DELETE SET NULL,
    agent_name             TEXT NOT NULL DEFAULT '',
    client_id              TEXT NOT NULL DEFAULT '',
    granted_by             UUID,
    approved_scopes        TEXT[] NOT NULL DEFAULT ARRAY[]::text[],
    storage_limit_bytes    BIGINT CHECK (storage_limit_bytes IS NULL OR storage_limit_bytes >= 0),
    egress_limit_bytes     BIGINT CHECK (egress_limit_bytes IS NULL OR egress_limit_bytes >= 0),
    workspace_tenant_id    VARCHAR(255),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    renewed_at             TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_agent_grants_tenant ON agent_grants (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_agent_grants_granted_by ON agent_grants (granted_by) WHERE granted_by IS NOT NULL;

-- api_keys.agent_grant_id: the grant whose key minted this S3 key (NULL = a
-- person's or a server's). Its keys carry the grant's caps, never outlive it
-- (expires_at clamped, revoked with it), may be revoked by the agent itself,
-- and never reach a Private bucket over S3 either (line 8). A plain uuid, no
-- foreign key: a key row outlives nothing it names.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS agent_grant_id UUID;
CREATE INDEX IF NOT EXISTS idx_api_keys_agent_grant ON api_keys (agent_grant_id) WHERE agent_grant_id IS NOT NULL;

-- approvals: what an agent may not do by default (deleting a bucket, making
-- anything Public, minting beyond its scope, …) asks the person once. One
-- row names ONE action on ONE resource; approved, it is used once (used_at)
-- within 24 h of its request (expires_at). decided_by: no foreign key.
CREATE TABLE IF NOT EXISTS approvals (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    VARCHAR(255) NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    grant_id     UUID NOT NULL REFERENCES agent_grants(id) ON DELETE CASCADE,
    action       TEXT NOT NULL,
    resource     TEXT NOT NULL DEFAULT '',
    requested_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '24 hours',
    decided_at   TIMESTAMPTZ,
    decided_by   UUID,
    decision     TEXT CHECK (decision IN ('approved', 'denied')),
    used_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_approvals_grant ON approvals (grant_id, action, resource);
CREATE INDEX IF NOT EXISTS idx_approvals_pending ON approvals (tenant_id, requested_at) WHERE decided_at IS NULL;

-- The MCP authorization flow (OAuth 2.1 + PKCE, RFC 7591 dynamic client
-- registration). A client is infrastructure (no tenant: anyone may register
-- one, it holds no authority until a person consents); a code is a person's
-- consent, single-use, ten minutes, only its sha256 stored.
CREATE TABLE IF NOT EXISTS agent_oauth_clients (
    client_id     TEXT PRIMARY KEY,
    client_name   TEXT NOT NULL DEFAULT '',
    redirect_uris TEXT[] NOT NULL CHECK (cardinality(redirect_uris) BETWEEN 1 AND 8),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at  TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS agent_oauth_codes (
    code_hash           TEXT PRIMARY KEY CHECK (code_hash ~ '^[0-9a-f]{64}$'),
    client_id           TEXT NOT NULL REFERENCES agent_oauth_clients(client_id) ON DELETE CASCADE,
    tenant_id           VARCHAR(255) NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id             UUID NOT NULL,
    redirect_uri        TEXT NOT NULL,
    code_challenge      TEXT NOT NULL,
    agent_name          TEXT NOT NULL DEFAULT '',
    storage_limit_bytes BIGINT,
    egress_limit_bytes  BIGINT,
    lifetime_days       INT NOT NULL DEFAULT 30 CHECK (lifetime_days BETWEEN 1 AND 30),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at          TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '10 minutes',
    used_at             TIMESTAMPTZ,
    grant_id            UUID
);

-- PR D (WP-APP-1, claimable apps; F-42): a partner (tenants.partner, an
-- operator's grant like tenants.platform) mints with a management key of
-- scope 'partner' a provisional tenant (provisional_until: 7 days; 1 GiB;
-- never Public) and a single-use claim link (claims: only its sha256). A
-- claim moves the tenant under the claimer as an app child, its data with it.
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS partner BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS provisional_until TIMESTAMPTZ;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS provisional_partner VARCHAR(255);
CREATE INDEX IF NOT EXISTS idx_tenants_provisional ON tenants (provisional_until) WHERE provisional_until IS NOT NULL;
CREATE TABLE IF NOT EXISTS claims (
    id                VARCHAR(255) PRIMARY KEY,
    tenant_id         VARCHAR(255) NOT NULL UNIQUE REFERENCES tenants(id) ON DELETE CASCADE,
    partner_tenant_id VARCHAR(255) NOT NULL,
    token_hash        TEXT NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    label             TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at        TIMESTAMPTZ NOT NULL,
    claimed_at        TIMESTAMPTZ,
    claimed_by        VARCHAR(255)
);
CREATE INDEX IF NOT EXISTS idx_claims_partner ON claims (partner_tenant_id, created_at);

-- The 'partner' scope joins the management key's list. The constraint is
-- 121's inline one (its default name); replaced only while it lacks the new
-- scope, so a re-run of the migration set is a no-op.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'management_keys_scopes_check'
                      AND pg_get_constraintdef(oid) LIKE '%partner%') THEN
        ALTER TABLE management_keys DROP CONSTRAINT IF EXISTS management_keys_scopes_check;
        ALTER TABLE management_keys ADD CONSTRAINT management_keys_scopes_check
            CHECK (cardinality(scopes) > 0
                   AND scopes <@ ARRAY['manage:read', 'manage:write', 'platform', 'apps', 'partner']::text[]);
    END IF;
END $$;
