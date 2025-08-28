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
	err = driver.Put(ctx, "${S3_BUCKET}", "test-file.txt", bytes.NewReader(data))

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
	err := driver.Put(ctx, "${S3_BUCKET}", "delete-test.txt", bytes.NewReader(testData))
	if err != nil {
		t.Fatal(err)
	}

	// Delete it
	err = driver.Delete(ctx, "${S3_BUCKET}", "delete-test.txt")
	if err != nil {
		t.Fatal(err)
	}

	// Verify it's gone
	_, err = driver.Get(ctx, "${S3_BUCKET}", "delete-test.txt")
	if err == nil {
		t.Fatal("file should be deleted")
	}
}

func TestS3Driver_MultipartUpload(t *testing.T) {
	endpoint := os.Getenv("S3_ENDPOINT")
	accessKey := os.Getenv("S3_ACCESS_KEY")
	secretKey := os.Getenv("S3_SECRET_KEY")
	if endpoint == "" || accessKey == "" || secretKey == "" {
		t.Skip("S3 credentials not set")
	}

	driver, _ := NewS3Driver(endpoint, accessKey, secretKey, "us-east-1", zap.NewNop())
	ctx := context.Background()

	// Start multipart upload
	upload, err := driver.CreateMultipartUpload(ctx, "${S3_BUCKET}", "multipart-test.bin")
	if err != nil {
		t.Fatal(err)
	}

	// Upload parts
	part1Data := []byte("Part 1 content here")
	part2Data := []byte("Part 2 content here")

	part1, err := driver.UploadPart(ctx, upload, 1, bytes.NewReader(part1Data))
	if err != nil {
		t.Fatal(err)
	}

	part2, err := driver.UploadPart(ctx, upload, 2, bytes.NewReader(part2Data))
	if err != nil {
		t.Fatal(err)
	}

	// Complete upload
	err = driver.CompleteMultipartUpload(ctx, upload, []CompletedPart{part1, part2})
	if err != nil {
		t.Fatal(err)
	}
}
