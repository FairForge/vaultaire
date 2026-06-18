-- Phase 10: Convergent chunk encryption
-- Track encryption state per chunk in the global content index.

ALTER TABLE global_content_index ADD COLUMN IF NOT EXISTS encrypted BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE global_content_index ADD COLUMN IF NOT EXISTS encryption_algo VARCHAR(32);
