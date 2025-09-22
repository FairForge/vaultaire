package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserProfile_Get(t *testing.T) {
	auth := NewAuthService(nil)
	ctx := context.Background()

	// Create test user
	user, _, _, err := auth.CreateUserWithTenant(ctx, "profile@test.com", "password", "TestCo")
	require.NoError(t, err)

	t.Run("get user profile", func(t *testing.T) {
		profile, err := auth.GetUserProfile(ctx, user.ID)
		require.NoError(t, err)

		assert.Equal(t, "profile@test.com", profile.Email)
		assert.Equal(t, "TestCo", profile.Company)
		assert.NotZero(t, profile.StorageUsed)
		assert.NotZero(t, profile.StorageLimit)
	})
}

func TestUserProfile_Update(t *testing.T) {
	auth := NewAuthService(nil)
	ctx := context.Background()

	user, _, _, err := auth.CreateUserWithTenant(ctx, "update@test.com", "password", "OldCo")
	require.NoError(t, err)

	t.Run("update profile fields", func(t *testing.T) {
		updates := ProfileUpdate{
			Company:     "NewCo",
			DisplayName: "John Doe",
			Timezone:    "America/New_York",
		}

		err := auth.UpdateUserProfile(ctx, user.ID, updates)
		require.NoError(t, err)

		profile, _ := auth.GetUserProfile(ctx, user.ID)
		assert.Equal(t, "NewCo", profile.Company)
		assert.Equal(t, "John Doe", profile.DisplayName)
	})
}
