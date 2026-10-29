-- 118_members.sql
--
-- WP-TEAM-1 (Stage 1a Prompt 52, plan § WP-STRIPE — teams; docs/ACCOUNTS.md:
-- a team is ONE shared tenant with members, a person may act on several
-- tenants). Additive only: the previous build reads none of it and keeps
-- serving while it applies.
--
-- tenant_members: who may act on a tenant, and as what. A member is a
-- (tenant, user, role) row; nothing about the tenant's data changes — S3
-- keys stay tenant-keyed, only the management surfaces learn who acts.
-- Roles: owner / admin / developer / viewer (auth.Can is the one rule).
-- One owner per tenant (the partial unique index). A child (parent_tenant_id)
-- and a sandbox (sandbox_of) have no user and no member row: the platform's
-- members act on its children through the platform.
--
-- The backfill makes every existing user the owner member of the tenant
-- it registered (users ↔ tenants by e-mail, the registration contract), so
-- nothing changes for anyone until an invitation is sent. Idempotent: the
-- deploy re-applies every file. An account the previous build registers
-- during the deploy overlap has no row; auth.MemberRole reads the same
-- e-mail rule for it.
CREATE TABLE IF NOT EXISTS tenant_members (
    tenant_id   VARCHAR(255) NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role        TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'developer', 'viewer')),
    invited_by  UUID,
    accepted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tenant_members_one_owner ON tenant_members (tenant_id) WHERE role = 'owner';

-- A user's tenants (the sign-in's list, the switcher).
CREATE INDEX IF NOT EXISTS idx_tenant_members_user ON tenant_members (user_id);

INSERT INTO tenant_members (tenant_id, user_id, role)
SELECT t.id, u.id, 'owner'
  FROM users u
  JOIN tenants t ON t.email = u.email
 WHERE t.parent_tenant_id IS NULL AND t.sandbox_of IS NULL
ON CONFLICT DO NOTHING;

-- tenant_invitations: an e-mailed, signed, single-use link (7 days). Only
-- the SHA-256 of the token is stored. `kind` leaves room for WP-FAMILY-1
-- (a family is a pool of personal tenants that reuses these invitations,
-- docs/ACCOUNTS.md); this WP accepts 'member' only. The role is never
-- 'owner': ownership is not given by an invitation.
CREATE TABLE IF NOT EXISTS tenant_invitations (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   VARCHAR(255) NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL DEFAULT 'member' CHECK (kind IN ('member', 'family')),
    email       TEXT NOT NULL,
    role        TEXT NOT NULL CHECK (role IN ('admin', 'developer', 'viewer')),
    token_hash  TEXT NOT NULL UNIQUE,
    invited_by  UUID,
    expires_at  TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_tenant_invitations_tenant ON tenant_invitations (tenant_id);
