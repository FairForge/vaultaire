package drivers

import (
	"bytes"
	"context"
	"os"
	"testing"

	"go.uber.org/zap"
)

func TestS3Driver_NewS3Driver(t *testing.T) {
	// Arrange
	endpoint := os.Getenv("S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_ENDPOINT not set")
	}
	accessKey := os.Getenv("S3_ACCESS_KEY")
	secretKey := os.Getenv("S3_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		t.Skip("S3 credentials not set")
	}
	// Act
	driver, err := NewS3Driver(endpoint, accessKey, secretKey, "test-region", zap.NewNop())
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if driver == nil {
		t.Fatal("driver should not be nil")
	}
}

func TestS3Driver_Put(t *testing.T) {
	// Skip if no credentials
	endpoint := os.Getenv("S3_ENDPOINT")
	accessKey := os.Getenv("S3_ACCESS_KEY")
	secretKey := os.Getenv("S3_SECRET_KEY")
	if endpoint == "" || accessKey == "" || secretKey == "" {
		t.Skip("S3 credentials not set")
	}

	// Arrange
	driver, err := NewS3Driver(endpoint, accessKey, secretKey, "us-east-1", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	data := []byte("test data")

	// Act
	err = driver.Put(ctx, "lyve-bucket-test", "test-file.txt", bytes.NewReader(data))

	// Assert
	if err != nil {
		t.Fatal(err)
	}
}

func TestS3Driver_Delete(t *testing.T) {
	// Skip if no credentials
	endpoint := os.Getenv("S3_ENDPOINT")
	accessKey := os.Getenv("S3_ACCESS_KEY")
	secretKey := os.Getenv("S3_SECRET_KEY")
	if endpoint == "" || accessKey == "" || secretKey == "" {
		t.Skip("S3 credentials not set")
	}
	
	driver, _ := NewS3Driver(endpoint, accessKey, secretKey, "us-east-1", zap.NewNop())
	ctx := context.Background()
	
	// Put something first
	testData := []byte("delete me")
	err := driver.Put(ctx, "lyve-bucket-test", "delete-test.txt", bytes.NewReader(testData))
	if err != nil {
		t.Fatal(err)
	}
	
	// Delete it
	err = driver.Delete(ctx, "lyve-bucket-test", "delete-test.txt")
	if err != nil {
		t.Fatal(err)
	}
	
	// Verify it's gone
	_, err = driver.Get(ctx, "lyve-bucket-test", "delete-test.txt")
	if err == nil {
		t.Fatal("file should be deleted")
	}
}
