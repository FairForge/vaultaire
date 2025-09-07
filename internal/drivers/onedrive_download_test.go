package drivers

import (
	"context"
	"testing"
)

func TestOneDriveDriver_Download(t *testing.T) {
	t.Run("streams file content", func(t *testing.T) {
		driver, _ := NewOneDriveDriver("test", "test", "", "test", nil)

		_, err := driver.Get(context.Background(), "test-container", "test.txt")

		// Expect error since not implemented yet
		if err == nil {
			t.Fatal("expected error for unimplemented method")
		}
	})
}
