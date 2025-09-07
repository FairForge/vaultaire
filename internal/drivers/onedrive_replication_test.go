package drivers

import (
	"context"
	"testing"
)

func TestOneDriveDriver_Replication(t *testing.T) {
	t.Run("replicates to multiple drives", func(t *testing.T) {
		driver, _ := NewOneDriveDriver("test", "test", "", "test", nil)

		// Verify replication configuration exists
		if driver.replicationEnabled == false {
			// Will fail initially since replicationEnabled doesn't exist
			t.Error("expected replication to be enabled")
		}
	})

	t.Run("handles replication failures", func(t *testing.T) {
		driver, _ := NewOneDriveDriver("test", "test", "", "test", nil)

		err := driver.Replicate(context.Background(), "source", "target", "file.txt")

		if err == nil {
			t.Fatal("expected error for unimplemented method")
		}
	})
}
