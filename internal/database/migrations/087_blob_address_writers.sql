-- 087: the writers of WP-R2-1 (Stage 1a Prompt 2C, PR C1, 2026-10-12).
--
-- From this build on, every whole-object write puts its bytes at a NEW
-- address <key>@<26 base32 chars> (blob_address.go newBlobAddress) and one
-- transaction flips the head row — and the version row — to it; the blob it
-- displaced is retired (retired_blobs, 086), never overwritten. Two facts the
-- writers need a column for:
--
-- multipart_uploads.storage_key: the address a CompleteMultipartUpload chose,
-- written BEFORE its backend write. A complete whose head-row transaction
-- failed after the bytes landed is retried by the client; the retry (and a
-- concurrent complete of the same upload) writes the SAME address instead of
-- a second one, so a retried complete never leaves two blobs and the
-- collector never deletes a blob an active upload is about to commit
-- (blobStillNamed treats an active upload's address as named). An abort, or
-- the reaper's, retires it as 'orphan_write' — no row ever committed it.
-- NULL = an upload created before this build, or one no complete has reached.
--
-- object_versions.encryption_key_version: the tenant key version a retained
-- version's SSE-S3 blob is sealed under (WP-R8-1), written by the version
-- write from the head row's value (Decision 2 of docs/reviews/WP-R2-1.md).
-- Reads do not need it (the blob's 0x02 header records it); the rotation
-- backlog gauge does: once versions keep bytes, their blobs are SSE-S3
-- objects the gauge must count (PR C2, keyRotationPendingSQL).
--
-- Additive and idempotent. The previous build (Part B, 086) ignores both
-- columns: it never names a column it does not know, and its own in-place
-- writes neither read nor need them (the two-slot deploy runs it on this
-- schema for up to 15 min).

ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS storage_key TEXT;
ALTER TABLE object_versions ADD COLUMN IF NOT EXISTS encryption_key_version INT;

-- The collector's re-check (blobStillNamed) asks whether an ACTIVE upload
-- names an address; only uploads a complete has reached carry one.
CREATE INDEX IF NOT EXISTS idx_multipart_uploads_storage_key
    ON multipart_uploads (tenant_id, bucket, object_key)
    WHERE storage_key IS NOT NULL AND status = 'active';

-- The re-check also asks whether a version row names an address; the
-- primary key starts with (tenant_id, bucket, object_key), which is all the
-- lookup needs.
