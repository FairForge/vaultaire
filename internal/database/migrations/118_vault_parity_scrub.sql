-- 118_vault_parity_scrub.sql
--
-- WP-VAULT-2 (docs/reviews/WP-VAULT-2.md § 1 + § 7): the parity scrub.
-- Additive only: the previous build reads none of it, and a protect it runs
-- leaves these columns as they were — which is why the digests are bound to
-- the etag they were computed for (digest_etag): a digest is trusted only
-- while digest_etag = etag.
--
-- shard_digests: the SHA-256 (hex) of each parity shard as protect wrote it
--   (index j = shard pj); NULL on a row written before, until its first
--   scrub records what it read (the baseline).
-- object_sha256: the SHA-256 (hex) of the object's STORED bytes as protect
--   read them (ciphertext for a sealed object — no key is needed to check
--   it); what a rebuild from the parity alone must reproduce.
-- digest_etag: the etag shard_digests and object_sha256 belong to.
-- scrubbed_at: the last scrub that read every shard of the row clean.
-- scrub_failures: scrubs in a row that found a shard unreadable, missing or
--   corrupt (or a rebuild that did not match); 0 after a clean one.
-- rebuilt_at: the last rebuild from the parity alone that matched
--   object_sha256.
-- restore_requested_at: when a protect first found the object on tape and
--   asked for its restore (NULL once the row is complete);
--   restore_checked_at: the last time it asked or found the restore still
--   running — a protect waits ten minutes between two.
ALTER TABLE vault_parity ADD COLUMN IF NOT EXISTS shard_digests        TEXT[];
ALTER TABLE vault_parity ADD COLUMN IF NOT EXISTS object_sha256        TEXT;
ALTER TABLE vault_parity ADD COLUMN IF NOT EXISTS digest_etag          TEXT;
ALTER TABLE vault_parity ADD COLUMN IF NOT EXISTS scrubbed_at          TIMESTAMPTZ;
ALTER TABLE vault_parity ADD COLUMN IF NOT EXISTS scrub_failures       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE vault_parity ADD COLUMN IF NOT EXISTS rebuilt_at           TIMESTAMPTZ;
ALTER TABLE vault_parity ADD COLUMN IF NOT EXISTS restore_requested_at TIMESTAMPTZ;
ALTER TABLE vault_parity ADD COLUMN IF NOT EXISTS restore_checked_at   TIMESTAMPTZ;

-- The scrub walks complete rows in key order from a cursor.
CREATE INDEX IF NOT EXISTS idx_vault_parity_complete_key
    ON vault_parity (tenant_id, bucket, object_key) WHERE state = 'complete';
