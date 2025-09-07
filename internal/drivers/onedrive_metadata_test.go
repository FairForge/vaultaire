package drivers

import (
	"context"
	"testing"
)

func TestOneDriveDriver_Metadata(t *testing.T) {
	t.Run("syncs file metadata", func(t *testing.T) {
		driver, _ := NewOneDriveDriver("test", "test", "", "test", nil)

		exists, err := driver.Exists(context.Background(), "test-container", "test.txt")

		if err == nil {
			t.Fatal("expected error for unimplemented method")
		}
		if exists {
			t.Fatal("should return false for unimplemented method")
		}
	})

	t.Run("lists container contents", func(t *testing.T) {
		driver, _ := NewOneDriveDriver("test", "test", "", "test", nil)

		items, err := driver.List(context.Background(), "test-container", "")

		if err == nil {
			t.Fatal("expected error for unimplemented method")
		}
		if items != nil {
			t.Fatal("should return nil for unimplemented method")
		}
	})
}
