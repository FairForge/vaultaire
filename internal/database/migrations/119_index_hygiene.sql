-- 119_index_hygiene.sql — Stage 1a Prompt 52E.
--
-- 1. WP-R9-3 (R9-17): four plain indexes that duplicate a UNIQUE index on the
--    same column of the same table, with no predicate — proved against
--    pg_indexes on a migrated database and on prod (2026-10-29):
--      idx_api_keys_key_id     (api_keys.key_id)    = api_keys_key_id_key
--      idx_tenants_access_key  (tenants.access_key) = tenants_access_key_key
--      idx_tenants_email       (tenants.email)      = tenants_email_key
--      idx_users_email         (users.email)        = users_email_key
--    Every lookup they served is served by the UNIQUE index; each cost a
--    write on the two hottest auth tables. Their CREATE in 005/006 is removed
--    in the same change, so a re-run never re-adds them. The partial
--    idx_api_keys_active stays (the planner picks it for the scoped lookup).
--
-- 2. WP-R11-2: idempotency claim-first + body hash. request_hash is
--    sha256(method ‖ path ‖ body) of the request a key was first used with
--    (NULL = a row written before: method and path only, as before). A
--    `claimed` row is the claim the middleware inserts BEFORE the handler
--    runs; it carries a ready 409 idempotency_in_progress answer, so the
--    previous build — which reads every row as a cached answer — replays
--    "in progress" for it too. The handler's answer replaces the claim.
--
-- 3. WP-R3-6: multipart_uploads.tagging keeps the x-amz-tagging header of
--    CreateMultipartUpload (validated there) for CompleteMultipartUpload,
--    which carries only the part list — as 068 keeps the other attributes.
--    NULL = no tag set (and every upload created before).
--
-- Additive and idempotent: the drops are IF EXISTS, the columns IF NOT
-- EXISTS with constant defaults (no table rewrite).

SET lock_timeout = '5s';

DROP INDEX IF EXISTS idx_api_keys_key_id;
DROP INDEX IF EXISTS idx_tenants_access_key;
DROP INDEX IF EXISTS idx_tenants_email;
DROP INDEX IF EXISTS idx_users_email;

ALTER TABLE idempotency_cache ADD COLUMN IF NOT EXISTS request_hash TEXT;
ALTER TABLE idempotency_cache ADD COLUMN IF NOT EXISTS claimed BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS tagging TEXT;
