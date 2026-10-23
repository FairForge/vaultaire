package account

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/lib/pq"
)

// TestErasedTablesCoverSchema (the R9 TestSQLLiteralsMatchSchema idea): every
// table in the migrated schema with a tenant_id / user_id / admin_user_id /
// granted_by column must be named in Deleted or Kept — a migration that adds
// a tenant-scoped table without a row here fails CI instead of leaking PII
// past an erasure. Kept tables must also say what happens to them (a scrub,
// or an empty SQL with the comment in erase.go).
func TestErasedTablesCoverSchema(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT table_name FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND column_name IN ('tenant_id', 'user_id', 'admin_user_id', 'granted_by')
		 ORDER BY table_name`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	covered := map[string]string{}
	for _, r := range Deleted {
		covered[r.Table] = "deleted"
	}
	for _, r := range Kept {
		covered[r.Table] = "kept"
	}
	var missing []string
	var seen []string
	for rows.Next() {
		var table string
		require.NoError(t, rows.Scan(&table))
		seen = append(seen, table)
		if _, ok := covered[table]; !ok {
			missing = append(missing, table)
		}
	}
	require.NoError(t, rows.Err())
	assert.Empty(t, missing, "tables keyed by tenant/user that no erase rule names — add them to Deleted or Kept in erase.go")
	assert.Greater(t, len(seen), 50, "the walk saw the schema")

	// Tables with no tenant/user column are not in the walk and need no
	// rule: payment_provider_readiness (098) is infrastructure — when a
	// payment provider was last up or down — not tenant data; backend_plans
	// (099, WP-CAP-1) the same — what WE bought from a storage vendor and
	// when it renews, keyed by backend, no customer in it. The transparency
	// log's tables (103) are the same and are pinned by
	// TestTranslogTablesAreInfrastructure below.

	// Every rule names a table that exists (a typo would silently skip).
	exists := map[string]bool{}
	for _, s := range seen {
		exists[s] = true
	}
	for _, r := range append(append([]Rule{}, Deleted...), Kept...) {
		switch r.Table {
		case "global_content_index", "waitlist_signups", "webhook_deliveries", "users", "tenants":
			continue // keyed by dedup_scope / e-mail / webhook_id / id — not in the walk
		}
		assert.True(t, exists[r.Table], fmt.Sprintf("rule names unknown table %q", r.Table))
	}

	// Every statement prepares against the schema (columns exist).
	for _, r := range append(append([]Rule{}, Deleted...), Kept...) {
		if r.SQL == "" {
			continue
		}
		stmt, err := db.PrepareContext(ctx, r.SQL)
		require.NoError(t, err, "rule for %s does not prepare: %s", r.Table, r.SQL)
		_ = stmt.Close()
	}
}

// translogTables are the transparency log's tables (migration 103, K4; its
// collected cosignatures, 105, K4c-2): public infrastructure. They carry no tenant or user column, so the
// erasure and the export never walk them, and no erase rule may name them —
// an entry is append-only and public, and its kinds name no tenant
// (internal/translog TestTranslog_EntriesNeverNameATenant).
var translogTables = []string{"translog_entries", "translog_hashes", "translog_checkpoints", "translog_cosignatures", "translog_anchors"}

func TestTranslogTablesAreInfrastructure(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	for _, table := range translogTables {
		var exists bool
		require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists))
		require.True(t, exists, "%s is missing — migrations 103, 105, 106", table)
		var personal int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.columns
			 WHERE table_schema = 'public' AND table_name = $1
			   AND (column_name IN ('tenant_id', 'user_id', 'admin_user_id', 'granted_by', 'performed_by')
			        OR column_name ILIKE '%email%' OR column_name ILIKE '%ip%addr%' OR column_name = 'ip'
			        OR column_name ILIKE '%tenant%' OR column_name ILIKE '%user%')`, table).Scan(&personal))
		assert.Zero(t, personal, "%s grew a tenant/user/e-mail/IP column: the log is public infrastructure", table)
		for _, r := range append(append([]Rule{}, Deleted...), Kept...) {
			assert.NotEqual(t, table, r.Table, "an erase rule names %s: the log is append-only and holds no tenant data", table)
		}
	}
}
