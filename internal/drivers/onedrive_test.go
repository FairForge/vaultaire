package drivers

import (
	"testing"
)

func TestOneDriveDriver_GraphSDKSetup(t *testing.T) {
	t.Run("creates driver with Graph SDK client", func(t *testing.T) {
		driver, err := NewOneDriveDriver(
			"test-client-id",
			"test-client-secret",
			"test-refresh-token",
			"test-tenant",
			nil,
		)

		if err != nil {
			t.Fatalf("failed to create driver: %v", err)
		}

		if driver == nil {
			t.Fatal("driver is nil")
		}

		if driver.graphClient == nil {
			t.Fatal("Graph client not initialized")
		}
	})
}
