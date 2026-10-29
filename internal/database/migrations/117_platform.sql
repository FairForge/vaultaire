-- 117_platform.sql
--
-- WP-PLAT-1 (Stage 1a Prompt 51, plan § WP-STRIPE — API-created accounts,
-- the Connect primitive). Additive only: the previous build reads none of
-- it.
--
-- tenants.platform: a PLATFORM tenant may create child tenants by API
-- (/api/v1/platform/*). Set only by an admin (PUT
-- /api/v1/admin/tenants/{id}/platform, audited admin.platform_granted);
-- FALSE on every tenant until the owner grants one.
--
-- tenants.parent_tenant_id: a CHILD tenant names the platform that made it.
-- A child has no users row and no e-mail of its own (a
-- platform+<id>@platform.invalid placeholder, as a sandbox's), lives on the
-- real backends and is billed through its parent's POOL: its own
-- tenant_quotas row counts its bytes, the quota check sums the pool
-- (internal/usage). Never a sandbox: sandbox_of is the other kind of
-- parent, and a sandbox is never in a pool. One level only (a child of a
-- child is refused by the code that makes it).
--
-- tenants.external_id / label: what the platform's app calls its child —
-- an optional id the app owns (unique per parent) and a display label.
--
-- tenants.deletion_scheduled_at: a tenant WITHOUT a user (a child) is
-- scheduled for erasure here (account.ScheduleTenant, the 30-day grace);
-- a user's account keeps users.deletion_scheduled_at.

ALTER TABLE tenants ADD COLUMN IF NOT EXISTS parent_tenant_id VARCHAR(255) REFERENCES tenants(id);
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS platform BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS external_id TEXT;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS label TEXT;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS deletion_scheduled_at TIMESTAMPTZ;

CREATE UNIQUE INDEX IF NOT EXISTS idx_tenants_parent_external ON tenants (parent_tenant_id, external_id)
    WHERE parent_tenant_id IS NOT NULL AND external_id IS NOT NULL;

-- The pool walk (every reservation of a pooled tenant) and the platform's
-- list read the children by their parent.
CREATE INDEX IF NOT EXISTS idx_tenants_parent ON tenants (parent_tenant_id) WHERE parent_tenant_id IS NOT NULL;

-- The deletion runner's due children.
CREATE INDEX IF NOT EXISTS idx_tenants_deletion_due ON tenants (deletion_scheduled_at) WHERE deletion_scheduled_at IS NOT NULL;
