-- 123: Stage 1a Prompt 56 — one file for the prompt (D-STRIPE-43's rule).
-- WP-FAMILY-1 itself needs no column (WP-ACCT-1's tree columns and WP-TEAM-1's
-- tenant_invitations.kind = 'family' are on main); Part 0's two findings do.
--
-- RP90-5: an agent caps only what its grant made — the S3 keys it minted
-- (api_keys.agent_grant_id, 122) and the buckets it created. A bucket records
-- the grant that created it here (NULL = a person's, or created before this
-- file). A plain uuid, no foreign key (RP84-1's lesson, 122's agent_grants
-- rule): a grant's row outlives its revoke, and the erasure takes the
-- tenant's buckets and grants together. Never customer-writable (buckets.metadata
-- is: the management PATCH replaces it whole).
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS created_by_agent_grant UUID;

-- RP90-6: an approval is bound to the resource's IDENTITY, not its name: the
-- bucket's created_at (as RFC 3339 nanoseconds) or the key's id, read when the
-- agent asks and checked when it spends, so an approval for a name never
-- reaches a bucket created (or re-created) after the ask. NULL = an approval
-- asked before this file: spent by name, as before (they live 24 h).
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS resource_ref TEXT;

-- Additive: the previous build reads neither column, writes no bucket row
-- with the first and no approval with the second; it keeps serving the new
-- schema until the switch.
