-- 030_metadata.sql
-- Phase 5.11.3: User-defined metadata on buckets and objects
-- Idempotent — safe to re-run on every deploy

ALTER TABLE buckets ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';
