package drivers

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestOneDriveDriver_TokenManagement(t *testing.T) {
	t.Run("validates token on initialization", func(t *testing.T) {
		driver, err := NewOneDriveDriver(
			"invalid-client",
			"invalid-secret",
			"",
			"invalid-tenant",
			nil,
		)

		// Should succeed in creating driver (token validation happens on first API call)
		if err != nil {
			t.Fatalf("failed to create driver: %v", err)
		}

		// Try to use the driver - should fail with auth error
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err = driver.ValidateAuth(ctx)
		if err == nil {
			t.Fatal("expected auth validation to fail with invalid credentials")
		}
	})
}

func TestOneDriveDriver_TokenManagement_Integration(t *testing.T) {
	// Skip if no real credentials
	if os.Getenv("ONEDRIVE_CLIENT_ID") == "" {
		t.Skip("Skipping integration test - no credentials")
	}

	t.Run("validates token with real credentials", func(t *testing.T) {
		driver, err := NewOneDriveDriverFromConfig(nil)
		if err != nil {
			t.Fatalf("failed to create driver: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err = driver.ValidateAuth(ctx)
		if err != nil {
			t.Fatalf("auth validation failed: %v", err)
		}
	})
}
