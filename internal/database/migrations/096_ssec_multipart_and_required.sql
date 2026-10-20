-- 096: SSE-C complete (WP-SSEC-1, Stage 1a Prompt 17 Part B; plan 5.14.8a).
--
-- SSE-C is the developer surface's only "can't read" path, and it stopped at
-- the single PUT: CreateMultipartUpload with the customer's key answered 501,
-- CopyObject of or into an SSE-C object 501, and no bucket could require a
-- key. Three columns, all additive (the previous build never names them):
--
-- multipart_uploads.ssec_key_md5  the base64 MD5 of the customer key the
--                                 upload was created with ('' = not SSE-C).
--                                 Every UploadPart must carry a key of this
--                                 MD5 (400 InvalidArgument otherwise, nothing
--                                 staged) and seals its part under it on the
--                                 staging disk; Complete carries no key and
--                                 needs none. The KEY is never stored.
-- multipart_parts.sealed_md5      the MD5 of a sealed part's STORED bytes
--                                 (its ciphertext on the staging disk), ''
--                                 for a plaintext part. The part's etag stays
--                                 the plaintext's MD5 (the -N ETag is built
--                                 from it); Complete re-hashes the sealed
--                                 bytes against this as it reads them, so a
--                                 part that changed at rest between
--                                 UploadPart and Complete is never committed
--                                 (the staged-part corruption class, WP-R3-1).
-- buckets.ssec_required           a bucket that promises keys: an unkeyed
--                                 PUT, CreateMultipartUpload or copy
--                                 destination into it is 400 InvalidRequest
--                                 (AWS's bucket-policy wording), a generated
--                                 object (export, access log, inventory) is
--                                 refused, never plaintext. Objects written
--                                 before the flag stay as they were. A public
--                                 bucket cannot require a key (409).

SET lock_timeout = '5s';

ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS ssec_key_md5 TEXT NOT NULL DEFAULT '';
ALTER TABLE multipart_parts   ADD COLUMN IF NOT EXISTS sealed_md5   TEXT NOT NULL DEFAULT '';
ALTER TABLE buckets           ADD COLUMN IF NOT EXISTS ssec_required BOOLEAN NOT NULL DEFAULT FALSE;

RESET lock_timeout;
