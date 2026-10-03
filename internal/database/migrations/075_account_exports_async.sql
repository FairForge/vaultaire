-- 075: the asynchronous GDPR export (WP-R10-3b / WP-R12-1).
--
-- account_exports (038) recorded a synchronous, inline export: file_path and
-- expires_at were never set. The export is now rendered by a background job
-- into the tenant's system bucket `_exports`, is re-downloadable for 7 days
-- through a presigned URL, and the row is the audit trail of it.
--
--   pending   → requested; the job claims it (claimed_at, attempts) and renders
--   completed → the object is written; file_path = "<bucket>/<key>", etag, size,
--               expires_at = completed_at + 7 days
--   expired   → the retention job deleted the object; the row stays
--   failed    → three attempts failed; error_message says why
--
-- One export in flight per user: a partial unique index, so a second request
-- fails on insert (no read-then-insert race).

ALTER TABLE account_exports ADD COLUMN IF NOT EXISTS attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE account_exports ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ;
ALTER TABLE account_exports ADD COLUMN IF NOT EXISTS etag TEXT;

ALTER TABLE account_exports DROP CONSTRAINT IF EXISTS valid_export_status;
ALTER TABLE account_exports ADD CONSTRAINT valid_export_status
    CHECK (status IN ('pending', 'processing', 'completed', 'failed', 'expired'));

-- A 'processing' row is a render the pre-075 synchronous code never finished
-- (the process died between the INSERT and the UPDATE): nothing to retry.
UPDATE account_exports SET status = 'failed', error_message = 'left processing by the synchronous exporter (pre-075)'
 WHERE status = 'processing';

CREATE UNIQUE INDEX IF NOT EXISTS idx_account_exports_one_pending
    ON account_exports (user_id) WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_account_exports_pending
    ON account_exports (created_at) WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_account_exports_expiry
    ON account_exports (expires_at) WHERE status = 'completed';
