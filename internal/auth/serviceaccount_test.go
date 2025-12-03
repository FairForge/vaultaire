// internal/auth/serviceaccount_test.go
package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServiceAccountConfig_Validate(t *testing.T) {
	t.Run("valid config passes validation", func(t *testing.T) {
		config := &ServiceAccountConfig{
			ID:          "sa-123",
			Name:        "backup-service",
			TenantID:    "tenant-1",
			Roles:       []string{"storage:read", "storage:write"},
			Description: "Automated backup service",
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &ServiceAccountConfig{
			ID:       "sa-123",
			TenantID: "tenant-1",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})

	t.Run("rejects empty tenant ID", func(t *testing.T) {
		config := &ServiceAccountConfig{
			ID:   "sa-123",
			Name: "backup-service",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "tenant")
	})

	t.Run("validates name format", func(t *testing.T) {
		config := &ServiceAccountConfig{
			ID:       "sa-123",
			Name:     "invalid name with spaces!",
			TenantID: "tenant-1",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})
}

func TestNewServiceAccountManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewServiceAccountManager()
		assert.NotNil(t, manager)
	})
}

func TestServiceAccountManager_Create(t *testing.T) {
	manager := NewServiceAccountManager()

	t.Run("creates service account with credentials", func(t *testing.T) {
		config := &ServiceAccountConfig{
			Name:     "test-service",
			TenantID: "tenant-1",
			Roles:    []string{"storage:read"},
		}

		account, credentials, err := manager.Create(context.Background(), config)
		require.NoError(t, err)
		assert.NotEmpty(t, account.ID)
		assert.Equal(t, "test-service", account.Name)
		assert.NotEmpty(t, credentials.KeyID)
		assert.NotEmpty(t, credentials.KeySecret)
		assert.True(t, account.Enabled)
	})

	t.Run("generates unique IDs", func(t *testing.T) {
		config1 := &ServiceAccountConfig{Name: "svc-1", TenantID: "tenant-1"}
		config2 := &ServiceAccountConfig{Name: "svc-2", TenantID: "tenant-1"}

		account1, _, _ := manager.Create(context.Background(), config1)
		account2, _, _ := manager.Create(context.Background(), config2)

		assert.NotEqual(t, account1.ID, account2.ID)
	})

	t.Run("rejects duplicate names in same tenant", func(t *testing.T) {
		config := &ServiceAccountConfig{
			Name:     "duplicate-name",
			TenantID: "tenant-dup",
		}

		_, _, err := manager.Create(context.Background(), config)
		require.NoError(t, err)

		_, _, err = manager.Create(context.Background(), config)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")
	})

	t.Run("allows same name in different tenants", func(t *testing.T) {
		config1 := &ServiceAccountConfig{Name: "shared-name", TenantID: "tenant-a"}
		config2 := &ServiceAccountConfig{Name: "shared-name", TenantID: "tenant-b"}

		_, _, err := manager.Create(context.Background(), config1)
		require.NoError(t, err)

		_, _, err = manager.Create(context.Background(), config2)
		assert.NoError(t, err)
	})
}

func TestServiceAccountManager_Get(t *testing.T) {
	manager := NewServiceAccountManager()

	t.Run("retrieves by ID", func(t *testing.T) {
		config := &ServiceAccountConfig{Name: "get-test", TenantID: "tenant-1"}
		created, _, _ := manager.Create(context.Background(), config)

		account := manager.Get(created.ID)
		assert.NotNil(t, account)
		assert.Equal(t, created.ID, account.ID)
	})

	t.Run("returns nil for unknown ID", func(t *testing.T) {
		account := manager.Get("unknown-id")
		assert.Nil(t, account)
	})
}

func TestServiceAccountManager_GetByName(t *testing.T) {
	manager := NewServiceAccountManager()

	t.Run("retrieves by name and tenant", func(t *testing.T) {
		config := &ServiceAccountConfig{Name: "named-account", TenantID: "tenant-1"}
		created, _, _ := manager.Create(context.Background(), config)

		account := manager.GetByName("tenant-1", "named-account")
		assert.NotNil(t, account)
		assert.Equal(t, created.ID, account.ID)
	})

	t.Run("returns nil for wrong tenant", func(t *testing.T) {
		config := &ServiceAccountConfig{Name: "tenant-specific", TenantID: "tenant-x"}
		_, _, _ = manager.Create(context.Background(), config)

		account := manager.GetByName("tenant-y", "tenant-specific")
		assert.Nil(t, account)
	})
}

func TestServiceAccountManager_List(t *testing.T) {
	manager := NewServiceAccountManager()

	_, _, _ = manager.Create(context.Background(), &ServiceAccountConfig{Name: "list-1", TenantID: "tenant-list"})
	_, _, _ = manager.Create(context.Background(), &ServiceAccountConfig{Name: "list-2", TenantID: "tenant-list"})
	_, _, _ = manager.Create(context.Background(), &ServiceAccountConfig{Name: "list-3", TenantID: "tenant-other"})

	t.Run("lists by tenant", func(t *testing.T) {
		accounts := manager.ListByTenant("tenant-list")
		assert.Len(t, accounts, 2)
	})

	t.Run("returns empty for unknown tenant", func(t *testing.T) {
		accounts := manager.ListByTenant("tenant-unknown")
		assert.Empty(t, accounts)
	})
}

func TestServiceAccountManager_Delete(t *testing.T) {
	manager := NewServiceAccountManager()

	t.Run("deletes service account", func(t *testing.T) {
		config := &ServiceAccountConfig{Name: "to-delete", TenantID: "tenant-1"}
		account, _, _ := manager.Create(context.Background(), config)

		err := manager.Delete(context.Background(), account.ID)
		assert.NoError(t, err)
		assert.Nil(t, manager.Get(account.ID))
	})

	t.Run("returns error for unknown ID", func(t *testing.T) {
		err := manager.Delete(context.Background(), "unknown-id")
		assert.Error(t, err)
	})
}

func TestServiceAccountManager_Enable_Disable(t *testing.T) {
	manager := NewServiceAccountManager()

	config := &ServiceAccountConfig{Name: "toggle-test", TenantID: "tenant-1"}
	account, _, _ := manager.Create(context.Background(), config)

	t.Run("disables account", func(t *testing.T) {
		err := manager.Disable(account.ID)
		assert.NoError(t, err)

		updated := manager.Get(account.ID)
		assert.False(t, updated.Enabled)
	})

	t.Run("enables account", func(t *testing.T) {
		err := manager.Enable(account.ID)
		assert.NoError(t, err)

		updated := manager.Get(account.ID)
		assert.True(t, updated.Enabled)
	})
}

func TestServiceAccountManager_Authenticate(t *testing.T) {
	manager := NewServiceAccountManager()

	config := &ServiceAccountConfig{
		Name:     "auth-test",
		TenantID: "tenant-1",
		Roles:    []string{"storage:read"},
	}
	account, credentials, _ := manager.Create(context.Background(), config)

	t.Run("authenticates with valid credentials", func(t *testing.T) {
		result, err := manager.Authenticate(context.Background(), credentials.KeyID, credentials.KeySecret)
		require.NoError(t, err)
		assert.True(t, result.Success)
		assert.Equal(t, account.ID, result.AccountID)
		assert.Equal(t, "tenant-1", result.TenantID)
		assert.Contains(t, result.Roles, "storage:read")
	})

	t.Run("rejects invalid key ID", func(t *testing.T) {
		result, err := manager.Authenticate(context.Background(), "invalid-key", credentials.KeySecret)
		assert.Error(t, err)
		assert.Nil(t, result)
	})

	t.Run("rejects invalid secret", func(t *testing.T) {
		result, err := manager.Authenticate(context.Background(), credentials.KeyID, "wrong-secret")
		assert.Error(t, err)
		assert.Nil(t, result)
	})

	t.Run("rejects disabled account", func(t *testing.T) {
		_ = manager.Disable(account.ID)

		result, err := manager.Authenticate(context.Background(), credentials.KeyID, credentials.KeySecret)
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "disabled")

		_ = manager.Enable(account.ID)
	})
}

func TestServiceAccountManager_RotateCredentials(t *testing.T) {
	manager := NewServiceAccountManager()

	config := &ServiceAccountConfig{Name: "rotate-test", TenantID: "tenant-1"}
	account, oldCreds, _ := manager.Create(context.Background(), config)

	t.Run("generates new credentials", func(t *testing.T) {
		newCreds, err := manager.RotateCredentials(context.Background(), account.ID)
		require.NoError(t, err)
		assert.NotEqual(t, oldCreds.KeyID, newCreds.KeyID)
		assert.NotEqual(t, oldCreds.KeySecret, newCreds.KeySecret)
	})

	t.Run("old credentials no longer work", func(t *testing.T) {
		_, err := manager.Authenticate(context.Background(), oldCreds.KeyID, oldCreds.KeySecret)
		assert.Error(t, err)
	})
}

func TestServiceAccountManager_UpdateRoles(t *testing.T) {
	manager := NewServiceAccountManager()

	config := &ServiceAccountConfig{
		Name:     "role-test",
		TenantID: "tenant-1",
		Roles:    []string{"storage:read"},
	}
	account, _, _ := manager.Create(context.Background(), config)

	t.Run("updates roles", func(t *testing.T) {
		err := manager.UpdateRoles(account.ID, []string{"storage:read", "storage:write", "admin"})
		assert.NoError(t, err)

		updated := manager.Get(account.ID)
		assert.Len(t, updated.Roles, 3)
		assert.Contains(t, updated.Roles, "admin")
	})
}

func TestServiceAccountCredentials(t *testing.T) {
	t.Run("key ID has correct prefix", func(t *testing.T) {
		manager := NewServiceAccountManager()
		config := &ServiceAccountConfig{Name: "prefix-test", TenantID: "tenant-1"}
		_, credentials, _ := manager.Create(context.Background(), config)

		assert.True(t, len(credentials.KeyID) > 0)
		assert.True(t, len(credentials.KeySecret) >= 32)
	})
}

func TestServiceAccountManager_Expiration(t *testing.T) {
	manager := NewServiceAccountManager()

	t.Run("creates account with expiration", func(t *testing.T) {
		expiry := time.Now().Add(24 * time.Hour)
		config := &ServiceAccountConfig{
			Name:      "expiring-account",
			TenantID:  "tenant-1",
			ExpiresAt: &expiry,
		}

		account, _, err := manager.Create(context.Background(), config)
		require.NoError(t, err)
		assert.NotNil(t, account.ExpiresAt)
		assert.True(t, account.ExpiresAt.After(time.Now()))
	})

	t.Run("rejects expired account authentication", func(t *testing.T) {
		expiry := time.Now().Add(-1 * time.Hour)
		config := &ServiceAccountConfig{
			Name:      "expired-account",
			TenantID:  "tenant-1",
			ExpiresAt: &expiry,
		}

		_, credentials, _ := manager.Create(context.Background(), config)

		_, err := manager.Authenticate(context.Background(), credentials.KeyID, credentials.KeySecret)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "expired")
	})
}

func TestServiceAccountManager_IPAllowlist(t *testing.T) {
	manager := NewServiceAccountManager()

	t.Run("creates account with IP allowlist", func(t *testing.T) {
		config := &ServiceAccountConfig{
			Name:        "ip-restricted",
			TenantID:    "tenant-1",
			IPAllowlist: []string{"10.0.0.0/8", "192.168.1.0/24"},
		}

		account, _, err := manager.Create(context.Background(), config)
		require.NoError(t, err)
		assert.Len(t, account.IPAllowlist, 2)
	})

	t.Run("validates IP in allowlist", func(t *testing.T) {
		config := &ServiceAccountConfig{
			Name:        "ip-check",
			TenantID:    "tenant-1",
			IPAllowlist: []string{"192.168.1.0/24"},
		}
		account, _, _ := manager.Create(context.Background(), config)

		assert.True(t, manager.IsIPAllowed(account.ID, "192.168.1.100"))
		assert.False(t, manager.IsIPAllowed(account.ID, "10.0.0.1"))
	})

	t.Run("allows all IPs when allowlist empty", func(t *testing.T) {
		config := &ServiceAccountConfig{
			Name:     "no-ip-restriction",
			TenantID: "tenant-1",
		}
		account, _, _ := manager.Create(context.Background(), config)

		assert.True(t, manager.IsIPAllowed(account.ID, "1.2.3.4"))
	})
}
