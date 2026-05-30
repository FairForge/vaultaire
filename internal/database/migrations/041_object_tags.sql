-- 041_object_tags.sql: Per-object tagging (S3 ?tagging sub-resource).

-- Object tags are stored as a flat JSONB map {"key": "value", ...} on the
-- HEAD cache row. Distinct from metadata (x-amz-meta-*) which lives in the
-- metadata column. Tags are set/replaced/deleted via the ?tagging sub-resource.
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS tags JSONB NOT NULL DEFAULT '{}';
