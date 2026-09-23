-- 062: Smart-tier demotion ledger (Phase 5.15.8).
--
-- One row per object the Smart demotion job moved from the hot backend to the
-- cold backend. The hot copy is NOT deleted at demotion time: it is reclaimed
-- on a later run, after a grace period, under a row lock and only if the
-- object_head_cache row still routes to the cold backend with the same ETag
-- (an overwrite or delete in between means the hot copy is no longer ours to
-- remove). Rows also let the read path promote cheaply: within the grace
-- window promotion is a routing flip, no data movement.
CREATE TABLE IF NOT EXISTS smart_demotions (
    tenant_id      TEXT        NOT NULL,
    bucket         TEXT        NOT NULL,
    object_key     TEXT        NOT NULL,
    etag           TEXT        NOT NULL,
    size_bytes     BIGINT      NOT NULL DEFAULT 0,
    hot_backend    TEXT        NOT NULL,
    cold_backend   TEXT        NOT NULL,
    reason         TEXT        NOT NULL DEFAULT '',   -- 'idle' | 'over_budget'
    demoted_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    hot_deleted_at TIMESTAMPTZ,                        -- NULL until the hot copy is reclaimed
    hot_outcome    TEXT        NOT NULL DEFAULT '',   -- 'deleted' | 'kept_changed' | 'object_gone' | 'delete_failed'
    PRIMARY KEY (tenant_id, bucket, object_key)
);

CREATE INDEX IF NOT EXISTS idx_smart_demotions_pending
    ON smart_demotions (demoted_at)
    WHERE hot_deleted_at IS NULL;
