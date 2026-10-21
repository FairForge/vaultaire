-- PostgreSQL 18 on slc-vaultaire-01 (121 GiB RAM, NVMe) — docs/DEPLOY.md
-- "Host tuning" (2026-10-21). Was near-default: shared_buffers 160 MB,
-- random_page_cost 4, jit on, max_wal_size 1 GB, effective_io_concurrency 16.
-- Run as postgres: psql -f postgresql-tuning.sql, then
-- `systemctl restart postgresql@18-main` (shared_buffers needs a restart;
-- the app's DB calls fail for the few seconds it takes).
ALTER SYSTEM SET shared_buffers = '2GB';
ALTER SYSTEM SET effective_cache_size = '64GB';
ALTER SYSTEM SET random_page_cost = 1.1;
ALTER SYSTEM SET effective_io_concurrency = 200;
ALTER SYSTEM SET max_wal_size = '8GB';
ALTER SYSTEM SET wal_compression = 'zstd';
ALTER SYSTEM SET jit = off;
