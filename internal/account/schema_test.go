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
