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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

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
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	ctx := context.Background()

	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = admin.Close() }()
	require.NoError(t, admin.PingContext(ctx))

	_, err = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+freshDBName)
	require.NoError(t, err, "drop stale fresh database")
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+freshDBName)
	require.NoError(t, err, "create fresh database")

	freshDSN, err := replaceDBName(dsn, freshDBName)
	require.NoError(t, err)

	fdb, err := sql.Open("postgres", freshDSN)
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
	var limitBytes, capCents int64
	err = fdb.QueryRowContext(ctx,
		"SELECT tier, storage_limit_bytes, spending_cap_cents FROM tenant_quotas WHERE tenant_id = $1",
		tenant.ID).Scan(&tier, &limitBytes, &capCents)
	require.NoError(t, err)
	assert.Equal(t, "free", tier, "new tenants must default to the free tier")
	assert.Equal(t, int64(5368709120), limitBytes, "free tier must default to 5 GB")
	assert.Equal(t, int64(0), capCents, "spending cap must default to 0 (no cap)")

	// The metered-billing reporter's tenant scan (internal/billing/metered.go)
	// must work against the migrated schema.
	rows, err := fdb.QueryContext(ctx, `
		SELECT tenant_id, tier, spending_cap_cents
		FROM tenant_quotas
		WHERE tier IN ('standard', 'performance') AND spending_cap_cents > 0`)
	require.NoError(t, err, "metered-billing SELECT must succeed on a fresh migrations-only database")
	require.NoError(t, rows.Err())
	_ = rows.Close()
}

// applyAllMigrations runs every migration file in sorted order. Each file is
// executed as a single batch, so any failing statement fails the file — the
// same strictness as `psql -v ON_ERROR_STOP=1` (Runbook 0.6).
func applyAllMigrations(ctx context.Context, t *testing.T, db *sql.DB, when string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no migration files found")
	sort.Strings(files)

	for _, f := range files {
		content, err := os.ReadFile(f)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, string(content))
		require.NoError(t, err, "migration %s must apply cleanly %s", filepath.Base(f), when)
	}
}

// TestMigrations_Reapply — WP-9 (CR-10): the deploy pipeline re-applies the
// ENTIRE migration set on every deploy. Under an honest runner
// (ON_ERROR_STOP=1, no `|| true`) every statement must therefore be
// idempotent — a second full pass against an already-migrated database has to
// come back clean, or every deploy after WP-9's runner change would fail.
func TestMigrations_Reapply(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	ctx := context.Background()

	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = admin.Close() }()
	require.NoError(t, admin.PingContext(ctx))

	const dbName = freshDBName + "_reapply"
	_, err = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbName)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+dbName)
	require.NoError(t, err)

	reapplyDSN, err := replaceDBName(dsn, dbName)
	require.NoError(t, err)
	fdb, err := sql.Open("postgres", reapplyDSN)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = fdb.Close()
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
	})
	require.NoError(t, fdb.PingContext(ctx))

	applyAllMigrations(ctx, t, fdb, "on the first pass")
	applyAllMigrations(ctx, t, fdb, "on the second pass (deploys re-run every file — all statements must be idempotent)")
}

func replaceDBName(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}
