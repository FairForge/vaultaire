# Migrations

Every deploy (and CI, and `make test-db`) applies EVERY file here, in lexical
order, with `psql -v ON_ERROR_STOP=1` and
`PGOPTIONS='-c lock_timeout=5s -c statement_timeout=600s'` (WP-R9-3). There
is no `schema_migrations` table: "already applied" is idempotency.

Rules:

- **Idempotent.** `CREATE … IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`,
  `DROP … IF EXISTS`, guarded `DO` blocks. `TestMigrations_Reapply` applies
  the whole set twice to a fresh database.
- **Additive.** The previous build keeps serving the migrated schema until
  the switch: never drop or rename something it reads.
- **A drop empties its CREATE in the same change** (100, 119): the earlier
  file would otherwise re-create on the next deploy what this one drops.
- **A new index on a big table is `CREATE INDEX CONCURRENTLY IF NOT EXISTS`
  in its own file, outside a transaction.** A plain `CREATE INDEX` holds a
  SHARE lock that blocks every write while the old build serves; with the
  runner's 5 s `lock_timeout` it fails the deploy instead, and past 600 s
  `statement_timeout` it is cut. Big = `events`, `s3_access_log`,
  `object_versions`, `object_head_cache`, `quota_usage_events`,
  `global_content_index`, `tenant_chunk_refs`, `bandwidth_usage_daily`, and
  any table past ~100k rows on prod. The file must hold nothing else: psql
  sends each statement on its own (no implicit transaction), and
  `CONCURRENTLY` refuses to run inside a `BEGIN … COMMIT` or a `DO` block.
  An interrupted concurrent build leaves an INVALID index that a re-run's
  `IF NOT EXISTS` skips: after such a deploy check
  `SELECT indexrelid::regclass FROM pg_index WHERE NOT indisvalid` and, for
  a row, `DROP INDEX CONCURRENTLY` it by hand and re-deploy.
- **Never edit a migration already on main** except to empty a CREATE that a
  later file drops (the rule above).
- The next number is in `docs/STATUS.md`.
