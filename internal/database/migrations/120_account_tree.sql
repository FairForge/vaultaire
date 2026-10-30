-- 120_account_tree.sql
--
-- WP-ACCT-1 (Stage 1a Prompt 53, plan § WP-STRIPE; docs/ACCOUNTS.md): the
-- account tree, and Part 0's two schema pieces (RP82-3, RP84-5). ONE file
-- for the whole WP, landed with Part 0, so no later PR of the WP edits a
-- migration already on main. Additive: the previous build reads none of
-- these columns, and the trigger below fills the two it does not write.
--
-- tenants.child_kind: the edge a child hangs off its parent by — managed
-- (a platform's customer: the parent mints its keys, its erasure cascades),
-- app (managed, plus children of its own: depth 2, the only depth beyond 1)
-- or sponsored (a person's own account a sponsor pays for: never erased or
-- suspended with the sponsor — it DETACHES). NULL on a root. Today's
-- children are managed.
--
-- tenants.pool_root_id: the root of the tenant's pool, denormalised on
-- every tenant of a tree (a root names itself), so the quota and egress
-- pools resolve at any depth with one indexed read. The ONE resolver is
-- internal/usage (PoolRoot, lockPoolOwner, poolUsed, lockPoolFloor,
-- poolFloorUsed); TestTree_PoolRootIsTheOneResolver keeps it so.
--
-- tenants.residency: where the tenant's bytes may be stored at rest — eu,
-- uk or us; NULL = anywhere (every tenant today). Set at creation, never
-- changed except by an admin while every bucket complies; a child inherits
-- its parent's and may narrow it, never widen it.
--
-- The trigger: a tenants row inserted without pool_root_id / child_kind —
-- by the previous build during the deploy overlap (registration, a
-- platform's child), by a test or a tool — gets them from its parent, so
-- the NOT NULL holds for every writer and no reader meets a NULL.
--
-- tenant_quotas.pool_used_bytes (RP82-3): on a pool's ROOT row, the sum of
-- storage_used_bytes over every tenant of the pool (0 on any other row).
-- Moved by the ledger in the statement that moves the member's
-- (internal/usage), so a pooled quota check is one row, not a sum over
-- the tree. Backfilled ONCE, when the column is added: a re-applied file
-- never rewrites it (a drift is the metric vaultaire_pool_used_drift_bytes,
-- never a silent fix).
--
-- tenant_quotas.egress_cap_bytes: a child's own monthly egress cap inside
-- its pool (0 = the pool decides), min(cap, pool) like storage_limit_bytes.
--
-- tenant_invitations (RP84-5): one PENDING invitation per tenant and
-- address — a new invitation of the address replaces the pending one.

ALTER TABLE tenants ADD COLUMN IF NOT EXISTS child_kind TEXT;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS pool_root_id VARCHAR(255) REFERENCES tenants(id);
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS residency TEXT;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenants_child_kind_check') THEN
        ALTER TABLE tenants ADD CONSTRAINT tenants_child_kind_check
            CHECK (child_kind IN ('managed', 'app', 'sponsored'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tenants_residency_check') THEN
        ALTER TABLE tenants ADD CONSTRAINT tenants_residency_check
            CHECK (residency IN ('eu', 'uk', 'us'));
    END IF;
END $$;

CREATE OR REPLACE FUNCTION tenants_tree_defaults() RETURNS trigger AS $$
BEGIN
    IF NEW.parent_tenant_id IS NOT NULL AND NEW.child_kind IS NULL THEN
        NEW.child_kind := 'managed';
    END IF;
    IF NEW.pool_root_id IS NULL THEN
        NEW.pool_root_id := COALESCE(
            (SELECT p.pool_root_id FROM tenants p WHERE p.id = NEW.parent_tenant_id),
            NEW.parent_tenant_id, NEW.id);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS tenants_tree_defaults ON tenants;
CREATE TRIGGER tenants_tree_defaults BEFORE INSERT ON tenants
    FOR EACH ROW EXECUTE FUNCTION tenants_tree_defaults();

UPDATE tenants SET child_kind = 'managed' WHERE parent_tenant_id IS NOT NULL AND child_kind IS NULL;
UPDATE tenants SET pool_root_id = COALESCE(parent_tenant_id, id) WHERE pool_root_id IS NULL;
ALTER TABLE tenants ALTER COLUMN pool_root_id SET NOT NULL;

CREATE INDEX IF NOT EXISTS idx_tenants_pool_root ON tenants (pool_root_id);

ALTER TABLE tenant_quotas ADD COLUMN IF NOT EXISTS egress_cap_bytes BIGINT NOT NULL DEFAULT 0;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                    WHERE table_schema = current_schema() AND table_name = 'tenant_quotas'
                      AND column_name = 'pool_used_bytes') THEN
        ALTER TABLE tenant_quotas ADD COLUMN pool_used_bytes BIGINT NOT NULL DEFAULT 0;
        UPDATE tenant_quotas r SET pool_used_bytes = COALESCE((
            SELECT SUM(q.storage_used_bytes)
              FROM tenant_quotas q LEFT JOIN tenants t ON t.id = q.tenant_id
             WHERE COALESCE(t.pool_root_id, q.tenant_id) = r.tenant_id), 0);
    END IF;
END $$;

-- RP84-5: duplicates first (none on prod: 0 invitations on 2026-10-30), the
-- newest pending invitation of an address kept.
DELETE FROM tenant_invitations i
 WHERE i.accepted_at IS NULL
   AND EXISTS (SELECT 1 FROM tenant_invitations n
                WHERE n.tenant_id = i.tenant_id AND lower(n.email) = lower(i.email)
                  AND n.accepted_at IS NULL
                  AND (n.created_at, n.id) > (i.created_at, i.id));
CREATE UNIQUE INDEX IF NOT EXISTS idx_tenant_invitations_one_pending
    ON tenant_invitations (tenant_id, lower(email)) WHERE accepted_at IS NULL;
