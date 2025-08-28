package drivers

import (
	"bytes"
	"context"
	"io"
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

func TestLocalDriver_UploadPart(t *testing.T) {
	// Arrange
	driver := NewLocalDriver(t.TempDir(), zap.NewNop())
	ctx := context.Background()

	upload, _ := driver.CreateMultipartUpload(ctx, "test", "file.bin")
	data := []byte("test part data")

	// Act
	part, err := driver.UploadPart(ctx, upload, 1, bytes.NewReader(data))

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if part.PartNumber != 1 {
		t.Errorf("expected part number 1, got %d", part.PartNumber)
	}
}

func TestLocalDriver_CompleteMultipartUpload(t *testing.T) {
	// Arrange
	driver := NewLocalDriver(t.TempDir(), zap.NewNop())
	ctx := context.Background()

	upload, _ := driver.CreateMultipartUpload(ctx, "test", "complete.bin")
	data1 := []byte("part 1 data")
	data2 := []byte("part 2 data")

	part1, _ := driver.UploadPart(ctx, upload, 1, bytes.NewReader(data1))
	part2, _ := driver.UploadPart(ctx, upload, 2, bytes.NewReader(data2))

	parts := []CompletedPart{part1, part2}

	// Act
	err := driver.CompleteMultipartUpload(ctx, upload, parts)

	// Assert
	if err != nil {
		t.Fatal(err)
	}

	// Verify file was created
	reader, err := driver.Get(ctx, "test", "complete.bin")
	if err != nil {
		t.Fatal("completed file should exist")
	}
	defer reader.Close()
}

func TestLocalDriver_MultipartUpload_VerifyContent(t *testing.T) {
	// Arrange
	driver := NewLocalDriver(t.TempDir(), zap.NewNop())
	ctx := context.Background()

	upload, _ := driver.CreateMultipartUpload(ctx, "test", "verify.bin")

	// Upload 3 parts with specific content
	part1Data := []byte("PART1-START-12345-END")
	part2Data := []byte("PART2-START-67890-END")
	part3Data := []byte("PART3-START-ABCDE-END")

	part1, _ := driver.UploadPart(ctx, upload, 1, bytes.NewReader(part1Data))
	part2, _ := driver.UploadPart(ctx, upload, 2, bytes.NewReader(part2Data))
	part3, _ := driver.UploadPart(ctx, upload, 3, bytes.NewReader(part3Data))

	// Complete upload
	parts := []CompletedPart{part1, part2, part3}
	err := driver.CompleteMultipartUpload(ctx, upload, parts)
	if err != nil {
		t.Fatal(err)
	}

	// Verify the assembled file has correct content
	reader, err := driver.Get(ctx, "test", "verify.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}

	expectedContent := append(append(part1Data, part2Data...), part3Data...)
	if !bytes.Equal(content, expectedContent) {
		t.Errorf("Content mismatch\nExpected: %s\nGot: %s", expectedContent, content)
	}
}
