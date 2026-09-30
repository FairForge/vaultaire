-- 069_head_cache_byte_order_index.sql (Review R4-05 / WP-R9-9)
--
-- S3 listings are in UTF-8 byte order; the database's default collation
-- (en_US.UTF-8 in prod) is not, and a LIKE prefix scan cannot use the
-- primary key under it. The listing queries now order and range on
-- object_key COLLATE "C"; this index carries the same collation so both the
-- prefix range and the order use it. CONCURRENTLY: the migration set runs
-- statement-by-statement outside a transaction (see internal/database/CLAUDE.md).
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_object_head_cache_key_c
    ON object_head_cache (tenant_id, bucket, object_key COLLATE "C");
