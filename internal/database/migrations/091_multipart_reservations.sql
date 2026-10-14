-- 091_multipart_reservations.sql
--
-- WP-R10-8 Part C (R10-24, R3-14): what an active multipart upload holds
-- against its tenant's quota. Every UploadPart reserves its bytes on the
-- upload's floor before the first byte is staged; the holding lives on the
-- upload row so a restart loses nothing: Complete settles it into the
-- object's bill, Abort / the reaper / the account erasure release exactly
-- it, and the reconcile counts it (an active upload's reserved_bytes are
-- billed bytes until it ends).
--
-- Additive only: the previous build keeps serving this schema until the
-- switch (it ignores both columns; an upload it creates holds 0).

ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS reserved_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS reserved_floor TEXT;

-- The reconcile and SetHouse sum the active holdings per tenant.
CREATE INDEX IF NOT EXISTS idx_multipart_uploads_active_reserved
    ON multipart_uploads (tenant_id)
    WHERE status = 'active' AND reserved_bytes > 0;
