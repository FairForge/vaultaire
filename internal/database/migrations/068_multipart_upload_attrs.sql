-- 068_multipart_upload_attrs.sql (Review R3, R3-03/R3-04/R10-24)
--
-- CreateMultipartUpload is where an S3 client sends the object's attributes
-- (Content-Type, x-amz-meta-*, Cache-Control, Content-Disposition, ...,
-- x-amz-storage-class); CompleteMultipartUpload carries only the part list.
-- The upload row never kept them, so every multipart object landed as
-- application/octet-stream with no metadata, and the storage-class header
-- was dropped (a GLACIER multipart went downstairs). The columns mirror the
-- object_head_cache row the complete step writes.

ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS content_type TEXT NOT NULL DEFAULT '';
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS storage_class TEXT NOT NULL DEFAULT '';
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS content_disposition TEXT NOT NULL DEFAULT '';
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS content_encoding TEXT NOT NULL DEFAULT '';
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS content_language TEXT NOT NULL DEFAULT '';
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS cache_control TEXT NOT NULL DEFAULT '';
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS http_expires TEXT NOT NULL DEFAULT '';
ALTER TABLE multipart_uploads ADD COLUMN IF NOT EXISTS website_redirect_location TEXT NOT NULL DEFAULT '';
