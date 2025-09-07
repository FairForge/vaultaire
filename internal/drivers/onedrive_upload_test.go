package drivers

import (
	"bytes"
	"context"
	"testing"
)

func TestOneDriveDriver_Upload(t *testing.T) {
	t.Run("uploads small file directly", func(t *testing.T) {
		driver, _ := NewOneDriveDriver("test", "test", "", "test", nil)

		// Small file (< 4MB) should use simple upload
		data := bytes.NewReader([]byte("test content"))
		err := driver.Put(context.Background(), "test-container", "test.txt", data)

		// Expect error since not implemented yet
		if err == nil {
			t.Fatal("expected error for unimplemented method")
		}
	})

	t.Run("uses upload session for large files", func(t *testing.T) {
		driver, _ := NewOneDriveDriver("test", "test", "", "test", nil)

		// Large file (> 4MB) should use upload session
		largeData := make([]byte, 5*1024*1024) // 5MB
		data := bytes.NewReader(largeData)

		err := driver.Put(context.Background(), "test-container", "large.bin", data)

		// Expect error since not implemented yet
		if err == nil {
			t.Fatal("expected error for unimplemented method")
		}
	})
}
