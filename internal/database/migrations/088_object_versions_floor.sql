-- 088: retained versions are billed on their own floor (WP-R2-1 Part C,
-- PR C2, 2026-10-12).
--
-- Since C1 (087) a non-current version keeps its own blob
-- (object_versions.storage_key), so it holds bytes and is billed: in
-- tenant_quotas.storage_used_bytes and on its floor in tenant_floor_quotas.
-- The floor is a fact about the write, like the head row's (066): the
-- storage class the write resolved to, decided at write time and never
-- re-derived (a bucket's tier can change between two writes; never bill a
-- vault-floor version as standard). The version write copies it from the
-- head row it is written with; the flip re-asserts it on the version that
-- keeps a displaced blob.
--
-- NULL = not recorded: a version without bytes (never billed), or a version
-- written by the C1 build during the two-slot switch. Readers bill a NULL
-- floor on the key's head row's floor, else on 'vault' when the version's
-- backend is the tape backend, else 'standard' (usage.RetainedVersionFloor).
--
-- Additive and idempotent. The C1 build (087) never names the column: it
-- serves this schema for up to 15 min during the switch, writing versions
-- with a NULL floor and not charging them; the reconcile repairs both.

ALTER TABLE object_versions ADD COLUMN IF NOT EXISTS floor TEXT;

-- The versions C1 wrote: billed on the floor of the head row of their key
-- (every C1 version of a key was written into the same bucket as its head).
UPDATE object_versions v
   SET floor = h.floor
  FROM object_head_cache h
 WHERE v.floor IS NULL AND v.storage_key IS NOT NULL
   AND h.tenant_id = v.tenant_id AND h.bucket = v.bucket AND h.object_key = v.object_key;

-- The reconcile, the house seeding and the dashboard sum a tenant's
-- retained versions: only rows that keep bytes.
CREATE INDEX IF NOT EXISTS idx_object_versions_retained
    ON object_versions (tenant_id)
    WHERE storage_key IS NOT NULL AND NOT is_delete_marker;
