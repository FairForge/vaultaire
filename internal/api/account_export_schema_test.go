package api

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExportSectionsCoverSchema (the TestErasedTablesCoverSchema /
// TestSweepBucketSourcesCoverSchema idea, WP-R10-3b): every table keyed by a
// tenant or a user is either a section of the export or named in
// exportExcludedTables with the reason — a migration that adds one cannot
// leave a customer's data out of the export unnoticed. And R10 invariant 12:
// every column whose name says secret / hash / token / password / code is
// named in exportNeverColumns, and no section's query mentions one of its
// table's never-columns — a migration that adds `users.recovery_secret` is
// red here before any export could carry it.
func TestExportSectionsCoverSchema(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}

	// Every section's query prepares against the schema (the columns exist).
	covered := map[string]bool{}
	for _, s := range exportSections {
		for _, q := range s.SQL {
			stmt, err := db.PrepareContext(ctx, q)
			require.NoError(t, err, "section %s: query does not prepare: %s", s.Name, q)
			_ = stmt.Close()
		}
		for _, tbl := range s.Tables {
			assert.False(t, covered[tbl], "table %s is in two sections", tbl)
			covered[tbl] = true
		}
	}

	// Direction 1: every tenant/user table is a section or excluded.
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT table_name FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND column_name IN ('tenant_id', 'user_id', 'admin_user_id', 'granted_by')
		 ORDER BY table_name`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	seen := 0
	for rows.Next() {
		var table string
		require.NoError(t, rows.Scan(&table))
		seen++
		_, excluded := exportExcludedTables[table]
		assert.True(t, covered[table] || excluded, "table %s is keyed by tenant/user: give it an export section or list it in exportExcludedTables with the reason", table)
		assert.False(t, covered[table] && excluded, "table %s is both a section and excluded", table)
	}
	require.NoError(t, rows.Err())
	assert.Greater(t, seen, 50, "the walk saw the schema")
	for tbl := range exportExcludedTables {
		assert.NotEmpty(t, exportExcludedTables[tbl], "excluded table %s has no reason", tbl)
	}

	// Direction 2: every secret-looking column is on the never list, and no
	// section query of that table mentions it.
	cols, err := db.QueryContext(ctx, `
		SELECT table_name, column_name FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND (column_name ILIKE '%secret%' OR column_name ILIKE '%hash%' OR column_name ILIKE '%token%'
		        OR column_name ILIKE '%password%' OR column_name ILIKE '%code%')
		 ORDER BY 1, 2`)
	require.NoError(t, err)
	defer func() { _ = cols.Close() }()
	sectionOf := map[string]exportSection{}
	for _, s := range exportSections {
		for _, tbl := range s.Tables {
			sectionOf[tbl] = s
		}
	}
	found := 0
	for cols.Next() {
		var table, column string
		require.NoError(t, cols.Scan(&table, &column))
		found++
		key := table + "." + column
		reason, listed := exportNeverColumns[key]
		assert.True(t, listed, "column %s looks like a secret: add it to exportNeverColumns (with why it is never exported, or why its name is a false positive)", key)
		assert.NotEmpty(t, reason, "never-column %s has no note", key)
		if s, ok := sectionOf[table]; ok {
			word := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(column) + `\b`)
			for _, q := range s.SQL {
				assert.False(t, word.MatchString(q), "section %s selects %s", s.Name, key)
			}
		}
	}
	require.NoError(t, cols.Err())
	assert.GreaterOrEqual(t, found, 15, "the secret-column walk saw the schema")
	for key := range exportNeverColumns {
		assert.Len(t, strings.SplitN(key, ".", 2), 2, "never-column %q is not table.column", key)
	}
}
