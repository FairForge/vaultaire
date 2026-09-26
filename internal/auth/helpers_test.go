package auth

import (
	"database/sql"
	"testing"

	_ "github.com/lib/pq"
)

// setupTestDB opens the local dev PostgreSQL and clears the @stored.ge test
// fixtures. Tests that call it skip when the server is unreachable.
func setupTestDB(t *testing.T) *sql.DB {
	connStr := "user=viera dbname=vaultaire sslmode=disable"
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatal(err)
	}

	// Clean up test data in dependency order:
	// api_keys → tenants → users (foreign keys cascade but be explicit)
	_, _ = db.Exec("DELETE FROM api_keys WHERE user_id IN (SELECT id FROM users WHERE email LIKE '%@stored.ge')")
	_, _ = db.Exec("DELETE FROM tenants WHERE email LIKE '%@stored.ge'")
	_, _ = db.Exec("DELETE FROM users WHERE email LIKE '%@stored.ge'")

	return db
}
