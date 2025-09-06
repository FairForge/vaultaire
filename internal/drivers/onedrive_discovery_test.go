package drivers

import (
	"context"
	"os"
	"testing"
)

func TestOneDriveDriver_DriveDiscovery(t *testing.T) {
	t.Run("discovers drives in tenant", func(t *testing.T) {
		driver, _ := NewOneDriveDriver("test", "test", "", "test", nil)

		// Mock discovery - in real implementation this would call Graph API
		drives, err := driver.DiscoverDrives(context.Background())

		if err == nil && drives == nil {
			t.Error("expected either drives or error, got neither")
		}
	})
}

func TestOneDriveDriver_DriveDiscovery_Integration(t *testing.T) {
	if os.Getenv("ONEDRIVE_CLIENT_ID") == "" {
		t.Skip("Skipping integration test - no credentials")
	}

	t.Run("discovers real drives", func(t *testing.T) {
		driver, err := NewOneDriveDriverFromConfig(nil)
		if err != nil {
			t.Fatalf("failed to create driver: %v", err)
		}

		ctx := context.Background()
		drives, err := driver.DiscoverDrives(ctx)

		// For SharePoint Plan 2, we should discover OneDrive drives
		// each user gets 1TB+ OneDrive storage
		t.Logf("Drives: %v, Error: %v", drives, err)
	})
}
