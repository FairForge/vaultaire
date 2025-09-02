package drivers

import (
	"context"
	"strings"
	"testing"
)

func TestLocalDriver_FilePermissions_Full(t *testing.T) {
	driver := setupTestDriver(t)
	ctx := context.Background()

	t.Run("SetPermissions", func(t *testing.T) {
		err := driver.Put(ctx, "test", "file.txt", strings.NewReader("content"))
		if err != nil {
			t.Fatal(err)
		}

		err = driver.SetPermissions(ctx, "test", "file.txt", 0644)
		if err != nil {
			t.Fatal(err)
		}

		perm, err := driver.GetPermissions(ctx, "test", "file.txt")
		if err != nil {
			t.Fatal(err)
		}

		if perm != 0644 {
			t.Errorf("Expected 0644, got %o", perm)
		}
	})
}
