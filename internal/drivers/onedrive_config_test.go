package drivers

import (
	"os"
	"testing"
)

func TestOneDriveDriver_LoadConfig(t *testing.T) {
	t.Run("loads credentials from environment", func(t *testing.T) {
		// Set test environment
		_ = os.Setenv("ONEDRIVE_CLIENT_ID", "test-client")
		_ = os.Setenv("ONEDRIVE_CLIENT_SECRET", "test-secret")
		_ = os.Setenv("ONEDRIVE_TENANT_ID", "test-tenant")
		defer func() {
			_ = os.Unsetenv("ONEDRIVE_CLIENT_ID")
			_ = os.Unsetenv("ONEDRIVE_CLIENT_SECRET")
			_ = os.Unsetenv("ONEDRIVE_TENANT_ID")
		}()

		config := LoadOneDriveConfig()

		if config.ClientID != "test-client" {
			t.Errorf("expected ClientID test-client, got %s", config.ClientID)
		}
		if config.ClientSecret != "test-secret" {
			t.Errorf("expected ClientSecret test-secret, got %s", config.ClientSecret)
		}
		if config.TenantID != "test-tenant" {
			t.Errorf("expected TenantID test-tenant, got %s", config.TenantID)
		}
	})
}
