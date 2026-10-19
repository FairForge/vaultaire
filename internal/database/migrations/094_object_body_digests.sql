-- 094: what the row knows about the bytes it describes (WP-R8-3 A.4 and
-- RP14-11, Stage 1a Prompt 14 Part A).
--
-- body_md5: the MD5 of a WHOLE object's bytes when its ETag is not that MD5 —
-- a multipart upload completed whole (its ETag is the `-N` MD5 of the parts'
-- MD5s). Written by CompleteMultipartUpload's whole branch from a tee on the
-- staged-parts stream (the plaintext's, for a sealed object), copied onto the
-- version row with the attributes (092), kept by the whole re-encrypt (the
-- plaintext does not change). Read by every whole 200 (GET, /cdn) as
-- COALESCE(body_md5, single-part ETag): a `-N` object is now verified as it
-- streams and cut on a mismatch, like a single-part one (object_integrity.go).
-- NULL = not known: a row written before this migration, a single-part object
-- (its ETag is the digest), a chunked object (its chunks verify themselves),
-- an R2 direct multipart upload (R2 gives no whole MD5) — served exactly as
-- before (counted unverified{multipart} when the ETag is `-N`).
--
-- ssec_key_md5: the base64 MD5 of the customer's SSE-C key, as AWS keeps it
-- with the object. Written by every SSE-C write; read by HEAD and GET: a key
-- whose MD5 differs is 403 from the row alone, before any backend call and
-- with no object header (AWS's answer; HEAD used to say 200 because the key
-- could only be checked against the blob — the docs/API.md deviation). NULL =
-- written before this migration, or not SSE-C: HEAD 200, GET 403 after the
-- blob fails to open, as before.
--
-- Additive, no backfill: the previous build keeps serving this schema until
-- the switch and never names these columns.

ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS body_md5 TEXT;
ALTER TABLE object_versions ADD COLUMN IF NOT EXISTS body_md5 TEXT;
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS ssec_key_md5 TEXT;
ALTER TABLE object_versions ADD COLUMN IF NOT EXISTS ssec_key_md5 TEXT;
