-- 081: vault_parity_orphans — first sightings of parity shard folders no row
-- names (Prompt 2a PR 5, 2026-10-08). The vault_parity job walks
-- `<tenant>__parity/` on every registered leg; a `<digest>/<etag>/` folder
-- that no vault_parity row names (shard_prefix) is a candidate. It is erased
-- only once it has been a candidate on two passes at least an hour apart
-- (VAULT_PARITY_ORPHAN_GRACE): a row is inserted before the first shard byte,
-- so a real write always has its row, and a leg's stale listing (one Sync
-- bridge behind the others) cannot cost a shard. A candidate that gains its
-- row, or whose folder is gone, loses its sighting.
CREATE TABLE IF NOT EXISTS vault_parity_orphans (
    tenant_id   TEXT        NOT NULL,
    leg         TEXT        NOT NULL,
    prefix      TEXT        NOT NULL,   -- <digest>/<etag> inside the tenant's _parity container
    first_seen  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, leg, prefix)
);
