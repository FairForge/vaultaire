-- 108: S3 secrets wrapped under the tenant's key (Stage 1a Prompt 42, plan
-- 43.7; docs/surfaces/TRUST.md item 12; docs/ARCHITECTURE_CONTRACT.md rule 4,
-- keys only through the one hierarchy, and rule 5, no global key).
--
-- api_keys.secret_key and sts_tokens.secret_key are plaintext: SigV4 needs the
-- secret, so a dump of one table signs as every tenant through the front door.
-- secret_wrapped is the same secret sealed under a key derived from the
-- tenant's key version secret_key_version (crypto.TenantKeys.SealCredential:
-- HKDF of the version's chunk key, AES-256-GCM, the AAD binding tenant, key id
-- and version — a wrapped secret moved to another row or tenant never opens).
-- The version lives in tenant_key_versions until the account is erased, so the
-- erasure's crypto-shred kills these too.
--
-- Expand, then contract. This migration only adds: the writers fill both
-- columns where a master key exists (prod has none until the F-14 ceremony, so
-- nothing is wrapped there until then), every reader opens secret_wrapped when
-- it is present and never falls back to the plaintext, and the key_reencrypt
-- job backfills the rows written before. The plaintext column is nulled only
-- behind the credential_plaintext_drop flag (PR C) — which is why
-- sts_tokens.secret_key loses its NOT NULL here: the previous build always
-- writes it, so dropping the constraint changes nothing for it.
--
-- tenants.secret_key (the mirror of the live primary pair, read by no lookup)
-- gets no wrapped column: it is nulled under the same flag and dropped with
-- the mirror columns (D-12).

SET lock_timeout = '5s';

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS secret_wrapped BYTEA;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS secret_key_version INT;
ALTER TABLE sts_tokens ADD COLUMN IF NOT EXISTS secret_wrapped BYTEA;
ALTER TABLE sts_tokens ADD COLUMN IF NOT EXISTS secret_key_version INT;

ALTER TABLE sts_tokens ALTER COLUMN secret_key DROP NOT NULL;

-- The backfill's scan: live rows still holding only the plaintext.
CREATE INDEX IF NOT EXISTS idx_api_keys_secret_unwrapped
    ON api_keys (tenant_id)
    WHERE secret_wrapped IS NULL AND secret_key IS NOT NULL AND revoked_at IS NULL;
