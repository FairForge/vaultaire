package auth

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestUserPreferences(t *testing.T) {
	auth := NewAuthService(nil)
	ctx := context.Background()

	user, _, _, err := auth.CreateUserWithTenant(ctx, "prefs@test.com", "password", "TestCo")
	require.NoError(t, err)

	t.Run("get default preferences", func(t *testing.T) {
		prefs, err := auth.GetUserPreferences(ctx, user.ID)
		require.NoError(t, err)

		assert.Equal(t, "dark", prefs.Theme)
		assert.True(t, prefs.EmailNotifications)
		assert.Equal(t, "list", prefs.FileView)
	})

	t.Run("update preferences", func(t *testing.T) {
		newPrefs := UserPreferences{
			Theme:              "light",
			EmailNotifications: false,
			FileView:           "grid",
			ShowHiddenFiles:    true,
			AutoBackup:         true,
		}

		err := auth.SetUserPreferences(ctx, user.ID, newPrefs)
		require.NoError(t, err)

		saved, _ := auth.GetUserPreferences(ctx, user.ID)
		assert.Equal(t, "light", saved.Theme)
		assert.False(t, saved.EmailNotifications)
		assert.Equal(t, "grid", saved.FileView)
	})
}
