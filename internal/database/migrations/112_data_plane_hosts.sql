-- 112_data_plane_hosts.sql
--
-- WP-EDGE-3 (Stage 1a Prompt 47, plan 40.10 — the data node) and its Part 0
-- (RP72-1). Additive only: the previous build reads none of it, and every
-- column has a DEFAULT or is NULLable, so the previous build's INSERTs into
-- edge_admits and object_head_cache keep working while it serves the new
-- schema until the switch.
--
-- data_plane_hosts: the registry of the hosts that move customer bytes for
-- the control plane (plan 40.10: "the control plane picks a host per
-- request — the first hosts are the client and SLC itself").
--   host        the public hostname (the PK; 'slc' for the origin itself)
--   kind        edge (the Cloudflare Worker), node (this binary with
--               VAULTAIRE_ROLE=node), slc (the origin's own data path)
--   address     the peer IP the control plane accepts a node's token from;
--               NULL for edge (the Cloudflare peer rule) and slc (in-process)
--   token_hash  sha256 hex of a node's token (minted once, shown once);
--               NULL for edge (the service token) and slc
--   tenant_id   NULL = a platform host; set = a tenant-scoped (BYO) host
--               that admits ONLY that tenant's requests and whose reports
--               are never trusted for integrity or metering (RP68-3)
--   weight      steering weight (0 = never steered to)
--   health      up | down | draining (a node unseen 90 s is marked down)
--   last_seen, version   the heartbeat's
-- A tenant-scoped host is that tenant's configuration: erased with the
-- account (account.EraseRows), excluded from the export (a hostname and an
-- IP the customer gave us, no object data).
CREATE TABLE IF NOT EXISTS data_plane_hosts (
    host        TEXT PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('edge', 'node', 'slc')),
    address     TEXT,
    token_hash  TEXT,
    tenant_id   TEXT,
    weight      INTEGER NOT NULL DEFAULT 0,
    health      TEXT NOT NULL DEFAULT 'down' CHECK (health IN ('up', 'down', 'draining')),
    last_seen   TIMESTAMPTZ,
    version     TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_data_plane_hosts_token_hash
    ON data_plane_hosts (token_hash) WHERE token_hash IS NOT NULL;

-- The two hosts that exist: the Worker on s3.stored.cloud and SLC itself
-- (weight 0: never steered to until the owner says).
INSERT INTO data_plane_hosts (host, kind, address, token_hash, tenant_id, weight, health)
VALUES ('s3.stored.cloud', 'edge', NULL, NULL, NULL, 1, 'up'),
       ('slc', 'slc', NULL, NULL, NULL, 0, 'up')
ON CONFLICT DO NOTHING;

-- edge_admits.host: which data-plane host an admit was made for (edge for
-- the Worker's service token, a node's registered host, slc in-process).
-- NULL on every row written before = the Worker.
ALTER TABLE edge_admits ADD COLUMN IF NOT EXISTS host TEXT;

-- An edge upload's Complete on a backend that does not enforce
-- x-amz-decoded-content-length (RP72-2, Lyve) asks whether any of the
-- upload's parts was an aws-chunked write (attrs.check_length): by upload.
CREATE INDEX IF NOT EXISTS idx_edge_admits_upload ON edge_admits (upload_id) WHERE upload_id IS NOT NULL;

-- object_head_cache.backend_etag (RP72-1, RB-2 of WP-EDGE-1 Part B): the
-- ETag the BACKEND answered for this row's blob, recorded at the write — a
-- mover commit (the Worker's report, the backend's Complete answer) and an
-- origin write whose driver reports it. An edge read signs If-Match from it
-- where set, never by inference from the size. NULL = written before, or by
-- a writer whose backend answer is unknown: the size rule with the cutoff
-- in force when the row was written (api/edge_get.go).
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS backend_etag TEXT;
