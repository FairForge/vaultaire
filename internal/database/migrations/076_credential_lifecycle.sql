-- 076: the credential lifecycle (WP-R5-14, with WP-R5-9, WP-R5-5, WP-R5-10).
--
-- The api_keys row IS the credential. Registration used to write the primary
-- pair twice — tenants.access_key/secret_key and an api_keys row named
-- 'primary' — and only the api_keys row could be rotated or revoked while
-- every credential lookup resolved tenants FIRST with no revocation check:
-- a rotated or revoked primary pair kept answering 200 for ever (found live
-- by WP-R10-3b). From here on:
--
--   api_keys.tenant_id   the key's tenant, on the row (WP-R5-9): the lookups
--                        no longer join users.email = tenants.email, so an
--                        e-mail change can never misroute or break a key.
--                        Backfilled from that join; a row left NULL (no user,
--                        or a user with no tenant) never authenticates.
--   api_keys.is_primary  the account's primary pair — the one registration
--                        hands out, what the credentials page, the export
--                        download link and the presigned-URL route use, and
--                        the one the free-tier key cap does not count. One
--                        LIVE primary per tenant (partial unique index): a
--                        rotation revokes it and inserts its successor in
--                        one transaction; it cannot be revoked outright.
--   tenants.access_key / secret_key
--                        a mirror of the live primary pair, rewritten by the
--                        same transaction, read by no credential lookup.
--   users.password_changed_at
--                        stamped by a password change or reset; a JWT whose
--                        iat is before it is refused (WP-R5-10).

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS tenant_id VARCHAR(255);
UPDATE api_keys ak
   SET tenant_id = t.id
  FROM users u
  JOIN tenants t ON t.email = u.email
 WHERE ak.user_id = u.id AND ak.tenant_id IS NULL;
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant ON api_keys(tenant_id);

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS is_primary BOOLEAN NOT NULL DEFAULT FALSE;
UPDATE api_keys ak
   SET is_primary = TRUE
  FROM tenants t
 WHERE t.access_key = ak.key_id AND NOT ak.is_primary AND ak.revoked_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_one_live_primary
    ON api_keys(tenant_id) WHERE is_primary AND revoked_at IS NULL;

ALTER TABLE users ADD COLUMN IF NOT EXISTS password_changed_at TIMESTAMPTZ;
