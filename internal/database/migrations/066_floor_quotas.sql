-- 066_floor_quotas.sql: whole-TB quotas per floor (dashboard plan, Phase 1).
--
-- The site sells storage as a house: downstairs = Standard, attic = Vault,
-- whole TB per floor. Billing therefore needs a quota per floor, and every
-- object needs to know which floor it stands on so PUT enforcement, deletes
-- and the reconcile job account the right one.
--
--   tenant_floor_quotas   one row per (tenant, floor) — present only for
--                         tenants who bought a house; tenants without rows
--                         keep the single total quota in tenant_quotas.
--   object_head_cache.floor  'standard' | 'vault', set from the storage class
--                         resolved at write time (GLACIER/DEEP_ARCHIVE = vault).
--   tenants.house_period  'annual' | 'monthly' | '' — the Stripe period bought.
--   tenant_quotas.pin_hot_bytes  pin-hot add-on (bytes that must never demote).
--
-- Idempotent — safe to re-run on every deploy.

SET lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS tenant_floor_quotas (
    tenant_id            TEXT        NOT NULL REFERENCES tenant_quotas(tenant_id) ON DELETE CASCADE,
    floor                TEXT        NOT NULL,
    storage_limit_bytes  BIGINT      NOT NULL DEFAULT 0,
    storage_used_bytes   BIGINT      NOT NULL DEFAULT 0,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, floor)
);

ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS floor TEXT NOT NULL DEFAULT 'standard';
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS house_period TEXT NOT NULL DEFAULT '';
ALTER TABLE tenant_quotas ADD COLUMN IF NOT EXISTS pin_hot_bytes BIGINT NOT NULL DEFAULT 0;

-- Backfill: objects already in the attic. An `archive` bucket resolves to
-- GLACIER at write time, and a GLACIER header on any bucket lands on geyser —
-- except objects the Smart-tier demotion job moved there, which stay on the
-- Standard floor (the customer bought downstairs; where we park it is ours).
UPDATE object_head_cache o
   SET floor = 'vault'
  FROM buckets b
 WHERE b.tenant_id = o.tenant_id AND b.name = o.bucket
   AND b.tier_preference = 'archive'
   AND o.floor <> 'vault';

UPDATE object_head_cache o
   SET floor = 'vault'
 WHERE o.backend_name = 'geyser'
   AND o.floor <> 'vault'
   AND NOT EXISTS (
       SELECT 1 FROM smart_demotions d
        WHERE d.tenant_id = o.tenant_id AND d.bucket = o.bucket AND d.object_key = o.object_key);
