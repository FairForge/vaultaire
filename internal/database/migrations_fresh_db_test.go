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

	// Apply every migration IN ORDER against the empty database. Each file is
	// executed as a single batch, so any failing statement fails the file —
	// the same strictness as `psql -v ON_ERROR_STOP=1` (Runbook 0.6).
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no migration files found")
	sort.Strings(files)

	for _, f := range files {
		content, err := os.ReadFile(f)
		require.NoError(t, err)
		_, err = fdb.ExecContext(ctx, string(content))
		require.NoError(t, err, "migration %s must apply cleanly to an empty database", filepath.Base(f))
	}

	// Every runtime table must now exist from migrations alone.
	for _, table := range runtimeTables {
		var reg sql.NullString
		err := fdb.QueryRowContext(ctx, "SELECT to_regclass($1)::text", table).Scan(&reg)
		require.NoError(t, err)
		assert.True(t, reg.Valid, "table %s must be created by the migration set", table)
	}

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

func replaceDBName(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}
