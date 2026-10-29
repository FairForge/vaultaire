// migrations_fresh_db_test.go — WP-8 (CR-8): the migration set must rebuild a
// working schema from an EMPTY database. This is the DR guarantee: if the prod
// box is lost, `createdb && apply all migrations` must yield a schema where
// registration (the 4-table persist) and metered billing work with no manual
// SQL and no runtime InitializeSchema calls.
//
// External test package so it can drive the real registration path in
// internal/auth against the freshly migrated database.
package database_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/lib/pq"
)

// freshDBName is fixed (not random) so a crashed run is cleaned up by the
// DROP DATABASE IF EXISTS on the next run instead of leaking databases.
const freshDBName = "vaultaire_wp8_fresh_migrations"

// runtimeTables are the tables that until WP-8 existed only as
// InitializeSchema calls in Go (never invoked by the production binary) —
// the migration set must now own all of them.
var runtimeTables = []string{
	"tenant_quotas",
	"quota_usage_events",
	"user_activities",
	"upgrade_triggers",
	"upgrade_suggestions",
	"grace_periods",
	"usage_reports",
	"usage_daily_snapshots",
	"report_schedules",
	"billing_policies",
	"billing_credits",
	"invoices",
	"artifacts",
	"audit_logs_archive",
}

func TestMigrations_FreshDatabaseBootstrap(t *testing.T) {
	cfg := testutil.DBConfig() // R9: vaultaire_test by default; DATABASE_URL still wins
	ctx := context.Background()

	admin, err := sql.Open("postgres", testutil.DSNFor(cfg))
	require.NoError(t, err)
	defer func() { _ = admin.Close() }()
	require.NoError(t, admin.PingContext(ctx))

	_, err = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+freshDBName)
	require.NoError(t, err, "drop stale fresh database")
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+freshDBName)
	require.NoError(t, err, "create fresh database")

	freshCfg := cfg
	freshCfg.Database = freshDBName
	fdb, err := sql.Open("postgres", testutil.DSNFor(freshCfg))
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = fdb.Close()
		// Best-effort drop; WITH (FORCE) kicks any lingering connections.
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + freshDBName + " WITH (FORCE)")
	})
	require.NoError(t, fdb.PingContext(ctx))

	// Apply every migration IN ORDER against the empty database.
	applyAllMigrations(ctx, t, fdb, "to an empty database")

	// Every runtime table must now exist from migrations alone.
	for _, table := range runtimeTables {
		var reg sql.NullString
		err := fdb.QueryRowContext(ctx, "SELECT to_regclass($1)::text", table).Scan(&reg)
		require.NoError(t, err)
		assert.True(t, reg.Valid, "table %s must be created by the migration set", table)
	}

	// 1.13: feature_flags (migration 059) must exist so the live-iteration
	// kit (kill-switches + per-tenant enablement) works on a fresh DB.
	{
		var reg sql.NullString
		err := fdb.QueryRowContext(ctx, "SELECT to_regclass('feature_flags')::text").Scan(&reg)
		require.NoError(t, err)
		assert.True(t, reg.Valid, "feature_flags must be created by migration 059")
	}

	// WP-C guards: the chunk tables' tenant_id must be TEXT (registration
	// mints string IDs). These tables are created by migration 016 — which
	// sorts before 051, so 051's IF NOT EXISTS never fires — and a UUID here
	// silently disables chunking for every real tenant (the pre-#339 bug).
	for _, table := range []string{"tenant_chunk_refs", "object_metadata"} {
		var dataType string
		err := fdb.QueryRowContext(ctx, `
			SELECT data_type FROM information_schema.columns
			WHERE table_name = $1 AND column_name = 'tenant_id'`, table).Scan(&dataType)
		require.NoError(t, err)
		assert.Equal(t, "text", dataType,
			"%s.tenant_id must be TEXT — UUID silently disables chunking for real tenants", table)
	}

	// Exactly one get_tenant_dedup_ratio overload, taking TEXT. The UUID
	// overload being resurrected by an old migration broke the dedup admin
	// panel on every deploy.
	var overloads int
	require.NoError(t, fdb.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pg_proc WHERE proname = 'get_tenant_dedup_ratio'`).Scan(&overloads))
	assert.Equal(t, 1, overloads, "get_tenant_dedup_ratio must have exactly one overload")
	var args string
	require.NoError(t, fdb.QueryRowContext(ctx, `
		SELECT pg_get_function_identity_arguments(oid) FROM pg_proc
		WHERE proname = 'get_tenant_dedup_ratio' LIMIT 1`).Scan(&args))
	assert.Contains(t, args, "text", "get_tenant_dedup_ratio must take TEXT")

	// Registration — the 4-table persist (users → tenants → api_keys →
	// tenant_quotas) — must succeed against the migrated schema.
	svc := auth.NewAuthService(nil, fdb)
	email := fmt.Sprintf("wp8-fresh-%d@example.com", time.Now().UnixNano())
	user, tenant, _, err := svc.CreateUserWithTenant(ctx, email, "test-password-123", "WP8 Fresh Co")
	require.NoError(t, err, "registration must succeed on a fresh migrations-only database")

	for table, where := range map[string]string{
		"users":         "email = '" + email + "'",
		"tenants":       "id = '" + tenant.ID + "'",
		"api_keys":      "user_id = '" + user.ID + "'",
		"tenant_quotas": "tenant_id = '" + tenant.ID + "'",
	} {
		var n int
		err := fdb.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+where).Scan(&n)
		require.NoError(t, err, "count %s", table)
		assert.GreaterOrEqual(t, n, 1, "registration must persist a row in %s", table)
	}

	// Free-tier defaults (migration 034 semantics) must hold on a fresh DB
	// even though 034's ALTER is skipped when tenant_quotas doesn't exist yet.
	var tier string
	var limitBytes int64
	err = fdb.QueryRowContext(ctx,
		"SELECT tier, storage_limit_bytes FROM tenant_quotas WHERE tenant_id = $1",
		tenant.ID).Scan(&tier, &limitBytes)
	require.NoError(t, err)
	assert.Equal(t, "free", tier, "new tenants must default to the free tier")
	assert.Equal(t, int64(5368709120), limitBytes, "free tier must default to 5 GB")

	// WP-PAY-7 (migration 100): a fresh database never has what the contract
	// dropped — the files that created it no longer do.
	assertContractDropped(ctx, t, fdb)
}

// assertContractDropped: the objects migration 100 drops are absent.
func assertContractDropped(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	var cols int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		 WHERE (table_name = 'tenants' AND column_name IN
		        ('stripe_subscription_id', 'subscription_status', 'house_period', 'deletion_stripe_cancelled_at'))
		    OR (table_name = 'tenant_quotas' AND column_name = 'spending_cap_cents')`).Scan(&cols))
	assert.Zero(t, cols, "a dropped column exists")
	for _, table := range []string{"subscriptions", "billing_charges", "metered_usage_reports"} {
		var reg sql.NullString
		require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, table).Scan(&reg))
		assert.False(t, reg.Valid, "table %s exists", table)
	}
}

// droppedAttnums counts the dropped-column slots of a table (pg_attribute
// keeps one per DROP COLUMN; 1600 columns, dropped included, is the cap).
func droppedAttnums(ctx context.Context, t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pg_attribute WHERE attrelid = $1::regclass AND attisdropped`, table).Scan(&n))
	return n
}

// TestMigration100_Idempotent — the contract drop (WP-PAY-7) on a database
// migrated as prod is, then re-applied the way every deploy re-applies the
// whole set: the objects stay gone, and no pass re-adds a column for 100 to
// drop again (the files that created them were emptied in the same change —
// otherwise tenants would gain four dropped-column slots per deploy).
func TestMigration100_Idempotent(t *testing.T) {
	cfg := testutil.DBConfig()
	ctx := context.Background()
	admin, err := sql.Open("postgres", testutil.DSNFor(cfg))
	require.NoError(t, err)
	defer func() { _ = admin.Close() }()
	require.NoError(t, admin.PingContext(ctx))

	const dbName = freshDBName + "_m100"
	_, err = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbName)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+dbName)
	require.NoError(t, err)
	c := cfg
	c.Database = dbName
	db, err := sql.Open("postgres", testutil.DSNFor(c))
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
	})

	// Arrange: the schema as prod holds it before 100 — every file up to 099,
	// then the objects 100 drops, as the old files created them.
	files := migrationFiles(t)
	for _, f := range files {
		if filepath.Base(f) >= "100_" {
			continue
		}
		applyFile(ctx, t, db, f)
	}
	_, err = db.ExecContext(ctx, `
		ALTER TABLE tenants ADD COLUMN IF NOT EXISTS stripe_subscription_id VARCHAR(255);
		ALTER TABLE tenants ADD COLUMN IF NOT EXISTS subscription_status VARCHAR(50) NOT NULL DEFAULT 'none';
		ALTER TABLE tenants ADD COLUMN IF NOT EXISTS house_period TEXT NOT NULL DEFAULT '';
		ALTER TABLE tenants ADD COLUMN IF NOT EXISTS deletion_stripe_cancelled_at TIMESTAMPTZ;
		ALTER TABLE tenant_quotas ADD COLUMN IF NOT EXISTS spending_cap_cents BIGINT NOT NULL DEFAULT 0;
		CREATE TABLE IF NOT EXISTS subscriptions (id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		    tenant_id VARCHAR(255) NOT NULL REFERENCES tenants(id) ON DELETE CASCADE);
		CREATE TABLE IF NOT EXISTS billing_charges (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), tenant_id VARCHAR(255) NOT NULL);
		CREATE TABLE IF NOT EXISTS metered_usage_reports (id BIGSERIAL PRIMARY KEY, tenant_id TEXT NOT NULL);`)
	require.NoError(t, err)

	// Act: the deploy that carries 100, then two more deploys.
	applyAllMigrations(ctx, t, db, "on the deploy that carries 100")
	assertContractDropped(ctx, t, db)
	slots := droppedAttnums(ctx, t, db, "tenants")
	quotaSlots := droppedAttnums(ctx, t, db, "tenant_quotas")
	applyAllMigrations(ctx, t, db, "on the next deploy")
	applyAllMigrations(ctx, t, db, "on the one after")

	// Assert
	assertContractDropped(ctx, t, db)
	assert.Equal(t, slots, droppedAttnums(ctx, t, db, "tenants"), "a re-run re-added a column for 100 to drop again")
	assert.Equal(t, quotaSlots, droppedAttnums(ctx, t, db, "tenant_quotas"))
}

// applyAllMigrations runs every migration file in sorted order. Each file is
// executed as a single batch, so any failing statement fails the file — the
// same strictness as `psql -v ON_ERROR_STOP=1` (Runbook 0.6).
func applyAllMigrations(ctx context.Context, t *testing.T, db *sql.DB, when string) {
	t.Helper()
	for _, f := range migrationFiles(t) {
		content, err := os.ReadFile(f)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, string(content))
		require.NoError(t, err, "migration %s must apply cleanly %s", filepath.Base(f), when)
	}
}

// migrationFiles is the set in apply order.
func migrationFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no migration files found")
	sort.Strings(files)
	return files
}

// applyFile runs one migration file as one batch.
func applyFile(ctx context.Context, t *testing.T, db *sql.DB, f string) {
	t.Helper()
	content, err := os.ReadFile(f)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(content))
	require.NoError(t, err, "migration %s must apply cleanly", filepath.Base(f))
}

// TestMigrations_Reapply — WP-9 (CR-10): the deploy pipeline re-applies the
// ENTIRE migration set on every deploy. Under an honest runner
// (ON_ERROR_STOP=1, no `|| true`) every statement must therefore be
// idempotent — a second full pass against an already-migrated database has to
// come back clean, or every deploy after WP-9's runner change would fail.
func TestMigrations_Reapply(t *testing.T) {
	cfg := testutil.DBConfig() // R9: vaultaire_test by default; DATABASE_URL still wins
	ctx := context.Background()

	admin, err := sql.Open("postgres", testutil.DSNFor(cfg))
	require.NoError(t, err)
	defer func() { _ = admin.Close() }()
	require.NoError(t, admin.PingContext(ctx))

	const dbName = freshDBName + "_reapply"
	_, err = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbName)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+dbName)
	require.NoError(t, err)

	reapplyCfg := cfg
	reapplyCfg.Database = dbName
	fdb, err := sql.Open("postgres", testutil.DSNFor(reapplyCfg))
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = fdb.Close()
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
	})
	require.NoError(t, fdb.PingContext(ctx))

	applyAllMigrations(ctx, t, fdb, "on the first pass")
	applyAllMigrations(ctx, t, fdb, "on the second pass (deploys re-run every file — all statements must be idempotent)")
	assertIndexHygiene(ctx, t, fdb)
}

// assertIndexHygiene — migration 119 (WP-R9-3, R9-17): the four plain
// indexes that duplicated a UNIQUE index on the same column are gone after
// any number of passes (their CREATE is emptied in 005/006, as 100's drops
// were), the UNIQUE indexes stay, idempotency_cache carries the claim
// columns of WP-R11-2 and multipart_uploads the tagging of WP-R3-6.
func assertIndexHygiene(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	exists := func(name string) bool {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, name).Scan(&n))
		return n == 1
	}
	for _, dup := range []string{"idx_api_keys_key_id", "idx_tenants_access_key", "idx_tenants_email", "idx_users_email"} {
		assert.False(t, exists(dup), "%s duplicates a UNIQUE index and is dropped", dup)
	}
	for _, kept := range []string{"api_keys_key_id_key", "tenants_access_key_key", "tenants_email_key", "users_email_key", "idx_api_keys_active"} {
		assert.True(t, exists(kept), "%s stays", kept)
	}
	for _, col := range [][2]string{{"idempotency_cache", "request_digest"}, {"idempotency_cache", "claimed"}, {"multipart_uploads", "tagging"}} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`, col[0], col[1]).Scan(&n))
		assert.Equal(t, 1, n, "%s.%s", col[0], col[1])
	}
}
