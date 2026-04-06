package auth

import (
	"context"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadFromDB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database test")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Arrange: create a user+tenant via an AuthService that has the DB handle
	writer := NewAuthService(nil, db)
	user, tenant, _, err := writer.CreateUserWithTenant(ctx, "loadtest@stored.ge", "secret123", "LoadTest Inc")
	require.NoError(t, err)

	// Act: create a fresh AuthService (empty maps) and load from DB
	reader := NewAuthService(nil, db)

	// Verify maps are empty before load
	_, err = reader.GetUserByEmail(ctx, "loadtest@stored.ge")
	require.Error(t, err, "user should not be in memory before LoadFromDB")

	err = reader.LoadFromDB(ctx)
	require.NoError(t, err)

	// Assert: user is now in memory
	loaded, err := reader.GetUserByEmail(ctx, "loadtest@stored.ge")
	require.NoError(t, err)
	assert.Equal(t, user.ID, loaded.ID)
	assert.Equal(t, "loadtest@stored.ge", loaded.Email)
	assert.Equal(t, tenant.ID, loaded.TenantID, "user.TenantID should be linked")

	// Assert: tenant is in keyIndex (ValidateS3Request path)
	loadedTenant, err := reader.ValidateS3Request(ctx, tenant.AccessKey)
	require.NoError(t, err)
	assert.Equal(t, tenant.ID, loadedTenant.ID)
	assert.Equal(t, user.ID, loadedTenant.UserID)

	// Assert: ValidatePassword works with loaded hash
	valid, err := reader.ValidatePassword(ctx, "loadtest@stored.ge", "secret123")
	require.NoError(t, err)
	assert.True(t, valid, "password should validate after LoadFromDB")
}

func TestLoadFromDB_NilDB(t *testing.T) {
	// LoadFromDB should be a no-op when sqlDB is nil (test mode)
	svc := NewAuthService(nil, nil)
	err := svc.LoadFromDB(context.Background())
	require.NoError(t, err)
}
