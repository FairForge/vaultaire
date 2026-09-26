package auth

import (
	"database/sql"
	"testing"

	"github.com/FairForge/vaultaire/internal/testutil"
	_ "github.com/lib/pq"
)

// setupTestDB opens the test database (testutil.DSN: DATABASE_URL, else
// TEST_DB_*, else local vaultaire_test — never the shared dev DB) and clears
// the @stored.ge fixtures this package's tests create.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("test database unreachable (%v) — create it with `make test-db`", err)
	}

	// Clean up test data in dependency order:
	// api_keys → tenants → users (foreign keys cascade but be explicit)
	_, _ = db.Exec("DELETE FROM api_keys WHERE user_id IN (SELECT id FROM users WHERE email LIKE '%@stored.ge')")
	_, _ = db.Exec("DELETE FROM tenants WHERE email LIKE '%@stored.ge'")
	_, _ = db.Exec("DELETE FROM users WHERE email LIKE '%@stored.ge'")

	return db
}
