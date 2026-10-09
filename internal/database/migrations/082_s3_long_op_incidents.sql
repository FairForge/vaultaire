-- 082: s3_long_op_incidents — what a stopping slot could not finish (Prompt
-- 2a.2 G1, 2026-10-08). Prometheus scrapes only the ACTIVE slot, so the
-- counters of the slot a deploy is stopping (an operation it cancelled at
-- its bound, an <Error> document it sent inside a committed 200 while
-- draining) were never seen. The stopping slot writes one row per such
-- operation before the engine closes the database; the active slot exports
-- vaultaire_s3_long_ops_abandoned_total{op} and
-- vaultaire_s3_long_ops_drain_errors_total{op} from this table (the R13
-- lesson: never an in-process value). Erased with the account; not part of
-- the GDPR export (operational bookkeeping about a request, not the user's
-- data).
CREATE TABLE IF NOT EXISTS s3_long_op_incidents (
    id           BIGSERIAL   PRIMARY KEY,
    outcome      TEXT        NOT NULL,   -- 'abandoned' | 'error_after_commit'
    op           TEXT        NOT NULL,   -- CompleteMultipartUpload | CopyObject | DeleteObjects
    tenant_id    TEXT        NOT NULL DEFAULT '',
    bucket       TEXT        NOT NULL DEFAULT '',
    object_key   TEXT        NOT NULL DEFAULT '',
    age_seconds  DOUBLE PRECISION NOT NULL DEFAULT 0,
    slot         TEXT        NOT NULL DEFAULT '',   -- the stopping process's port
    version      TEXT        NOT NULL DEFAULT '',   -- its BuildSHA
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_s3_long_op_incidents_tenant ON s3_long_op_incidents (tenant_id);
