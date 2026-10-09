-- 083: one latest version per key (Prompt 2b 0.5, 2026-10-09). Every write
-- of a key's object_versions rows — a version write, a delete marker, a
-- version delete, a Complete retry's re-assert — now holds one
-- transaction-scoped advisory lock on the key (lockVersionKey); before, two
-- writes of the same key could each clear the latest flag and each insert a
-- latest row. This index makes a second latest row impossible whatever a
-- future writer forgets. Prod had 0 keys with more than one latest row on
-- 2026-10-09 (205,604 rows, 699 latest); the build on a copy took 32 ms, well
-- inside the deploy's lock_timeout. Additive: idx_obj_versions_latest (same
-- columns, same predicate, not unique) stays until a later migration.
CREATE UNIQUE INDEX IF NOT EXISTS idx_object_versions_one_latest
    ON object_versions (tenant_id, bucket, object_key)
    WHERE is_latest;
