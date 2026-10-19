-- 095: the webhook outbox (WP-R11-3, Stage 1a Prompt 16 Part B; R11-11).
--
-- A webhook delivery was ONE attempt: webhook_deliveries.retry_count was
-- always 0 and next_retry_at never written, so a receiver that was down for
-- a deploy lost the event for good. One row here per (event, endpoint) is the
-- durable intent, written where the delivery pool job is enqueued (the job is
-- attempt 1). An attempt that fails leaves the row `pending` with
-- next_attempt_at on the backoff (1/5/25/125 s, jittered) until its fifth
-- attempt, then `failed`; a 2xx makes it `delivered`. The `webhook_retry`
-- job (every 30 s, FOR UPDATE SKIP LOCKED) hands due rows back to the same
-- pool. An endpoint deleted or disabled meanwhile makes its rows `skipped`.
--
-- state:            pending | delivered | failed (attempts exhausted) | skipped
-- attempts:         POSTs that finished (the row's next attempt is attempts+1)
-- next_attempt_at:  when the row is due; while an attempt is queued or in
--                   flight it is a lease (now + 15 min) so a crashed process's
--                   attempt is retried and a running one is not doubled
-- endpoint_id:      SET NULL when the endpoint is deleted (the row is made
--                   `skipped` first by the delete; a NULL is skipped by the job)
--
-- Terminal rows are kept 30 days (the retention job, like webhook_deliveries)
-- and erased with the account (account.EraseRows). Additive: the previous
-- build never names this table.

CREATE TABLE IF NOT EXISTS webhook_outbox (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    endpoint_id      TEXT REFERENCES webhook_endpoints(id) ON DELETE SET NULL,
    event_id         TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    state            TEXT NOT NULL DEFAULT 'pending',
    attempts         INT NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ,
    last_status      INT,
    last_error       TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at     TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One intent per (event, endpoint): a re-enqueue is ON CONFLICT DO NOTHING.
CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_outbox_event_endpoint
    ON webhook_outbox (event_id, endpoint_id);
-- The retry job's scan: due pending rows, oldest first.
CREATE INDEX IF NOT EXISTS idx_webhook_outbox_due
    ON webhook_outbox (next_attempt_at) WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS idx_webhook_outbox_tenant ON webhook_outbox (tenant_id);
CREATE INDEX IF NOT EXISTS idx_webhook_outbox_endpoint ON webhook_outbox (endpoint_id);
CREATE INDEX IF NOT EXISTS idx_webhook_outbox_created_at ON webhook_outbox (created_at);
