-- 089: x-amz-server-side-encryption on CreateMultipartUpload is kept
-- (WP-R8-5, the WP-R3-1 residual PM-3).
--
-- CompleteMultipartUpload carries only the part list, so the header a client
-- sent on Create was lost: an upload asking AES256 into a bucket that is not
-- sse_enabled was stored plaintext where a PUT with the same header seals.
-- The upload row now keeps it and Complete's placement reads it.
--
-- Additive (the previous build serves this schema until the switch; it
-- neither reads nor writes the column, and an upload it creates gets FALSE —
-- what it did before).

ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS sse_requested BOOLEAN NOT NULL DEFAULT FALSE;
