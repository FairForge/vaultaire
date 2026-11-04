-- Migration 127: transfer in (WP-XFER-1, Stage 1a Prompt 59 PR A;
-- docs/reviews/WP-STRIPE-11.md). 126 is Part 0's (scrub_findings once).
--
-- A tenant gives a source — an S3-compatible endpoint, region, bucket and
-- prefix, with a key it made (read-only, by its own vendor) — and a
-- destination bucket of its own; the `transfer` job lists the source from
-- its cursor into transfer_items and copies each item through the PUT's
-- own write path (placement, the quota reservation, the version rows),
-- idempotent on (source ETag, size).
--
-- The source key (D-XFER-1, migration 108's rule): secret_wrapped +
-- secret_key_version (crypto.TenantKeys.SealCredential, AAD tenant ‖ job id
-- ‖ version) when the server has a master key, else the plaintext
-- secret_key; NULLED — the access key id too — the moment the job is
-- completed, failed or cancelled. Never exported (the export omits every
-- key column), never logged. Both tables are tenant data: erased with the
-- account (account.EraseRows).
--
-- Additive and idempotent: the deploy re-applies every file.

SET lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS transfer_jobs (
    id                  UUID PRIMARY KEY,
    tenant_id           TEXT NOT NULL,
    bucket              TEXT NOT NULL,                 -- the destination, the tenant's own
    source_endpoint     TEXT NOT NULL,                 -- https://host[:port], no path/query/userinfo
    source_region       TEXT NOT NULL DEFAULT '',
    source_bucket       TEXT NOT NULL,
    source_prefix       TEXT NOT NULL DEFAULT '',
    access_key_id       TEXT,                          -- NULL once the job ended
    secret_key          TEXT,                          -- plaintext only without a master key
    secret_wrapped      BYTEA,
    secret_key_version  INT,
    state               TEXT NOT NULL DEFAULT 'pending'
                        CHECK (state IN ('pending', 'running', 'paused', 'completed', 'failed', 'cancelled')),
    paused_reason       TEXT NOT NULL DEFAULT '',      -- quota | source_denied
    paused_error         TEXT NOT NULL DEFAULT '',      -- the refusal's S3 code (the quota's 403)
    list_cursor         TEXT NOT NULL DEFAULT '',      -- the last source key listed
    listed_all          BOOLEAN NOT NULL DEFAULT FALSE,
    listed              BIGINT NOT NULL DEFAULT 0,
    copied              BIGINT NOT NULL DEFAULT 0,
    skipped             BIGINT NOT NULL DEFAULT 0,
    failed              BIGINT NOT NULL DEFAULT 0,
    bytes_copied        BIGINT NOT NULL DEFAULT 0,
    rate_bytes_per_sec  BIGINT NOT NULL,
    last_error          TEXT NOT NULL DEFAULT '',
    created_by          TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at          TIMESTAMPTZ,
    finished_at         TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_transfer_jobs_tenant ON transfer_jobs (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_transfer_jobs_due ON transfer_jobs (created_at) WHERE state IN ('pending', 'running');

CREATE TABLE IF NOT EXISTS transfer_items (
    job_id       UUID NOT NULL REFERENCES transfer_jobs (id) ON DELETE CASCADE,
    tenant_id    TEXT NOT NULL,
    source_key   TEXT NOT NULL,
    source_etag  TEXT NOT NULL DEFAULT '',
    size         BIGINT NOT NULL DEFAULT 0,
    state        TEXT NOT NULL DEFAULT 'pending'
                 CHECK (state IN ('pending', 'copied', 'skipped', 'failed')),
    dest_etag    TEXT,                                  -- the ETag the copy gave the destination
    attempts     INT NOT NULL DEFAULT 0,
    last_error   TEXT NOT NULL DEFAULT '',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (job_id, source_key)
);

CREATE INDEX IF NOT EXISTS idx_transfer_items_pending ON transfer_items (job_id, source_key) WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS idx_transfer_items_tenant ON transfer_items (tenant_id);
