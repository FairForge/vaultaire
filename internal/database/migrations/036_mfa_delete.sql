-- 036: MFA Delete support for Object Lock buckets
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS mfa_delete_enabled BOOLEAN NOT NULL DEFAULT FALSE;
