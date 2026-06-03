-- Phase 7.6: Per-bucket data residency constraint
ALTER TABLE buckets ADD COLUMN IF NOT EXISTS data_residency TEXT DEFAULT NULL;
