package drivers

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func TestLocalDriver_CreateMultipartUpload(t *testing.T) {
	// Arrange
	driver := NewLocalDriver(t.TempDir(), zap.NewNop())
	ctx := context.Background()
	
	// Act
	upload, err := driver.CreateMultipartUpload(ctx, "test-container", "large-file.bin")
	
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if upload == nil {
		t.Fatal("upload should not be nil")
	}
	if upload.ID == "" {
		t.Error("upload ID should not be empty")
	}
}
