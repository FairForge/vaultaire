package drivers

import (
	"testing"
)

func TestOneDriveDriver_BatchOperations(t *testing.T) {
	t.Run("enforces 20 operation limit", func(t *testing.T) {
		driver, _ := NewOneDriveDriver("test", "test", "", "test", nil)

		// Create 25 operations
		var operations []BatchOperation
		for i := 0; i < 25; i++ {
			operations = append(operations, BatchOperation{
				Method: "GET",
				Path:   "/me/drive/root",
			})
		}

		batches := driver.SplitIntoBatches(operations)

		if len(batches) != 2 {
			t.Errorf("expected 2 batches for 25 operations, got %d", len(batches))
		}

		if len(batches[0]) != 20 {
			t.Errorf("expected first batch to have 20 operations, got %d", len(batches[0]))
		}

		if len(batches[1]) != 5 {
			t.Errorf("expected second batch to have 5 operations, got %d", len(batches[1]))
		}
	})
}
