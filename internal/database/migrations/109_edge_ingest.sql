-- 109_edge_ingest.sql
--
-- WP-EDGE-1 Part A (Stage 1a Prompt 44, plan 40.10 slice 1 — the mover): a
-- PutObject or UploadPart admitted at the edge is streamed by the Cloudflare
-- Worker on s3.stored.cloud straight to the backend and committed by the
-- control plane. What the control plane needs to remember between the
-- admit and the commit lives here.
--
-- Additive only: the previous build ignores every column and table below
-- (a two-slot deploy keeps serving this schema until the switch; the
-- edge_ingest flag stays OFF until the new build is the active slot — the
-- previous build cannot Complete an edge upload, it knows no
-- backend_upload_id; docs/DEPLOY.md).

-- One row per admitted write: the reservation, the minted address and what
-- the presigned link signed. pending → committed | failed | expired. Tenant
-- data: erased with the account; excluded from the export (transient
-- admission rows). The address of a failed or expired PUT goes to
-- retired_blobs (orphan_write), so the table is no sweep bucket source.
CREATE TABLE IF NOT EXISTS edge_admits (
    id               TEXT        NOT NULL PRIMARY KEY, -- 26 Crockford base32 chars, like a blob address's id
    tenant_id        TEXT        NOT NULL,
    bucket           TEXT        NOT NULL,
    object_key       TEXT        NOT NULL,
    op               TEXT        NOT NULL CHECK (op IN ('put', 'part')),
    upload_id        TEXT,                              -- a part: the multipart upload
    part_number      INT,                               -- a part: its number
    backend          TEXT        NOT NULL,              -- the driver the link was signed for
    storage_key      TEXT        NOT NULL,              -- the minted blob address (a part: the upload's recorded address)
    content_length   BIGINT      NOT NULL,              -- the DECODED length the link signs
    content_md5      TEXT,                              -- base64, as signed (NULL = the client sent none)
    content_type     TEXT        NOT NULL DEFAULT '',
    attrs            JSONB       NOT NULL DEFAULT '{}', -- what handlePutObject stores: metadata, cache headers, disposition, tags …
    storage_class    TEXT        NOT NULL DEFAULT '',
    floor            TEXT        NOT NULL DEFAULT 'standard',
    reserved_bytes   BIGINT      NOT NULL DEFAULT 0,    -- what the admit reserved (a PUT: on floor; a part: on the upload's floor)
    reserved_floor   TEXT,                              -- the floor the reservation charged (a PUT)
    floor_reserved   BOOLEAN     NOT NULL DEFAULT FALSE, -- writeBill.FloorReserved
    credited         BIGINT      NOT NULL DEFAULT 0,    -- writeBill.Credited (the net reservation's credit)
    client_ip        TEXT        NOT NULL DEFAULT '',
    request_id       TEXT        NOT NULL DEFAULT '',
    state            TEXT        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'committed', 'failed', 'expired')),
    backend_status   INT,                               -- what the backend answered the Worker
    backend_etag     TEXT,                              -- the ETag the backend answered (quoted, as given)
    result           JSONB,                             -- the answer the commit gave (replayed to a retried commit)
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at       TIMESTAMPTZ NOT NULL,
    finished_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_edge_admits_state_expires ON edge_admits (state, expires_at);
CREATE INDEX IF NOT EXISTS idx_edge_admits_tenant ON edge_admits (tenant_id);

-- An edge multipart upload: the control plane owns the mapping upload id →
-- backend upload id. edge is set by the FIRST edge part's admit (under the
-- upload row's lock, when no staged part exists); a staged part on an edge
-- upload is then refused (one upload is one path). The backend upload is
-- created on edge_backend under the upload's recorded storage_key and
-- completed or aborted by the control plane; backend_upload_ended_at
-- records that the backend call was made (the reaper retries an abort
-- while it is NULL).
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS edge BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS backend_upload_id TEXT;
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS edge_backend TEXT;
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS backend_upload_ended_at TIMESTAMPTZ;

-- A part with a backend_etag is an edge part: its bytes are on the backend
-- (as the part of backend_upload_id), no staged file exists for it.
ALTER TABLE multipart_parts ADD COLUMN IF NOT EXISTS backend_etag TEXT;
