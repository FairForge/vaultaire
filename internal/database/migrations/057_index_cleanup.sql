-- 057_index_cleanup.sql (WP-9, H-11)
-- Idempotent — safe to re-run on every deploy.
--
-- Drops the 9 redundant indexes identified in the launch readiness review —
-- each is a duplicate of (or a strict prefix of) a PK/UNIQUE constraint the
-- table already carries, so they cost write amplification and buy nothing:
--   idx_ohc_lookup                     = object_head_cache PK
--   idx_tcr_object / idx_tcr_tenant    = old 016-era tenant_chunk_refs indexes
--   idx_tenant_chunk_refs_object       = prefix of UNIQUE(tenant_id, bucket_name, object_key, chunk_index)
--   idx_tenant_chunk_refs_hash         = duplicate of 016's idx_tcr_hash on the
--                                        same column (an FK indexes only the
--                                        referenced side, never the referencing
--                                        side — idx_tcr_hash is what keeps hash
--                                        lookups on tenant_chunk_refs indexed)
--   idx_om_listing / idx_object_metadata_tenant_bucket
--                                      = prefix of UNIQUE(tenant_id, bucket_name, object_key)
--   idx_bandwidth_tenant_date          = duplicate of UNIQUE(tenant_id, date)
--   idx_gci_marked_for_deletion        = superseded GC partial index
--
-- Adds the missing events(created_at) index: the event-log list endpoint and
-- retention jobs scan by time but only (tenant_id, created_at) was indexed.

DROP INDEX IF EXISTS idx_ohc_lookup;
DROP INDEX IF EXISTS idx_tcr_object;
DROP INDEX IF EXISTS idx_tcr_tenant;
DROP INDEX IF EXISTS idx_tenant_chunk_refs_object;
DROP INDEX IF EXISTS idx_tenant_chunk_refs_hash;
DROP INDEX IF EXISTS idx_om_listing;
DROP INDEX IF EXISTS idx_object_metadata_tenant_bucket;
DROP INDEX IF EXISTS idx_bandwidth_tenant_date;
DROP INDEX IF EXISTS idx_gci_marked_for_deletion;

CREATE INDEX IF NOT EXISTS idx_events_created ON events(created_at DESC);
