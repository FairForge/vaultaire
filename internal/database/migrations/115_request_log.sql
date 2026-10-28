-- 115_request_log.sql
--
-- WP-LOG-1 (Stage 1a Prompt 49, plan § WP-STRIPE — the developers' request
-- log). Additive only: the previous build reads none of it.
--
-- request_log: one row per S3 request of the last 7 days, for every tenant
-- (s3_access_log is per bucket and opt-in; this is not), written by the ONE
-- access-log path (S3AccessLogTracker's second sink) behind the request_log
-- flag. What a developer needs to find a request a support ticket quotes —
-- and nothing more: never a body, never a header, never an SSE-C key, a
-- signature or any secret (TestRequestLog_NeverHoldsAnSSECKeyOrASecret):
--   tenant_id   the owner
--   key_id      the access key id that signed it (public: the AK, never the secret)
--   logged_at   when it was answered
--   op          the S3 operation (PutObject, GetObject, …)
--   bucket, object_key   what it named ('' when nothing)
--   status      the HTTP status answered
--   error_code  the S3 error code answered ('' on success)
--   request_id  the request id the client was answered (the error's RequestId)
--   bytes_in, bytes_out  body bytes received / sent
--   host        what served it (origin, or the edge host's name)
-- Tenant data: erased with the account (account.EraseRows), excluded from
-- the export (the same requests as the S3 access log, kept 7 days for the
-- developer's own debugging). The retention job prunes it at 7 days.
CREATE TABLE IF NOT EXISTS request_log (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    key_id      TEXT NOT NULL DEFAULT '',
    logged_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    op          TEXT NOT NULL DEFAULT '',
    bucket      TEXT NOT NULL DEFAULT '',
    object_key  TEXT NOT NULL DEFAULT '',
    status      SMALLINT NOT NULL DEFAULT 0,
    error_code  TEXT NOT NULL DEFAULT '',
    request_id  TEXT NOT NULL DEFAULT '',
    bytes_in    BIGINT NOT NULL DEFAULT 0,
    bytes_out   BIGINT NOT NULL DEFAULT 0,
    host        TEXT NOT NULL DEFAULT ''
);
-- The page and the API read a tenant's newest first, keyset paged.
CREATE INDEX IF NOT EXISTS idx_request_log_tenant_time ON request_log (tenant_id, logged_at DESC, id DESC);
-- The support search: one request id.
CREATE INDEX IF NOT EXISTS idx_request_log_request_id ON request_log (tenant_id, request_id);
-- The retention job's batched delete.
CREATE INDEX IF NOT EXISTS idx_request_log_logged_at ON request_log (logged_at);

-- request_suggestions: the suggestion shown beside an error code, stored
-- ONCE per code and joined on read. The text is the code's generic advice
-- (internal/api requestSuggestions), never a response's own suggestion: a
-- "Did you mean <bucket>?" names one tenant's data and this table is every
-- tenant's. Infrastructure: no tenant column; never erased, never exported.
CREATE TABLE IF NOT EXISTS request_suggestions (
    error_code  TEXT PRIMARY KEY,
    suggestion  TEXT NOT NULL
);
