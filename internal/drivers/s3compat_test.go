package drivers

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestS3CompatDriver_Operations(t *testing.T) {
	// Skip if no credentials
	accessKey := os.Getenv("S3_ACCESS_KEY")
	secretKey := os.Getenv("S3_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		t.Skip("S3_ACCESS_KEY and S3_SECRET_KEY required")
	}

	logger := zap.NewNop()
	driver, err := NewS3CompatDriver(accessKey, secretKey, logger)
	require.NoError(t, err)

	ctx := context.Background()

	// Test HealthCheck
	err = driver.HealthCheck(ctx)
	assert.NoError(t, err)

	// Test Put
	testData := []byte("test content for S3-compatible")
	err = driver.Put(ctx, "test-container", "test-artifact", bytes.NewReader(testData))
	assert.NoError(t, err)

	// Test List
	artifacts, err := driver.List(ctx, "test-container", "")
	assert.NoError(t, err)
	assert.Contains(t, artifacts, "test-artifact")

	// Test Get
	reader, err := driver.Get(ctx, "test-container", "test-artifact")
	require.NoError(t, err)

	defer func() {
		if err := reader.Close(); err != nil {
			t.Errorf("failed to close reader: %v", err)
		}
	}()

	// Fix: Declare buf BEFORE using it
	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(reader) // Don't need n since we're not using it
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	assert.Equal(t, testData, buf.Bytes())

	// Test Delete
	err = driver.Delete(ctx, "test-container", "test-artifact")
	assert.NoError(t, err)
}
