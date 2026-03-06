package auth

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/lib/pq" // PostgreSQL driver
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDBAuthService_CreateUser(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database test")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	service := NewDBAuthService(db)

	user, err := service.CreateUser(context.Background(), "test@stored.ge", "password123")
	require.NoError(t, err)
	assert.NotEmpty(t, user.ID)
	assert.Equal(t, "test@stored.ge", user.Email)

	// Should fail on duplicate
	_, err = service.CreateUser(context.Background(), "test@stored.ge", "password456")
	assert.Error(t, err)
}

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
