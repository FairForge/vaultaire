package drivers

import (
	"context"
	"testing"
)

func TestOneDriveDriver_ErrorHandling(t *testing.T) {
	t.Run("handles delete errors gracefully", func(t *testing.T) {
		driver, _ := NewOneDriveDriver("test", "test", "", "test", nil)

		err := driver.Delete(context.Background(), "test-container", "test.txt")

		if err == nil {
			t.Fatal("expected error for unimplemented method")
		}
	})

	t.Run("retries transient errors", func(t *testing.T) {
		driver, _ := NewOneDriveDriver("test", "test", "", "test", nil)

		// Verify retry logic exists
		if driver.maxRetries != 3 {
			// Will fail initially since maxRetries doesn't exist
			t.Errorf("expected maxRetries to be 3, got %d", driver.maxRetries)
		}
	})
}
