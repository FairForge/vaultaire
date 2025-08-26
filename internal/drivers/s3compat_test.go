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
	artifacts, err := driver.List(ctx, "test-container")
	assert.NoError(t, err)
	assert.Contains(t, artifacts, "test-artifact")

	// Test Get
	reader, err := driver.Get(ctx, "test-container", "test-artifact")
	require.NoError(t, err)
	defer reader.Close()

	buf := new(bytes.Buffer)
	buf.ReadFrom(reader)
	assert.Equal(t, testData, buf.Bytes())

	// Test Delete
	err = driver.Delete(ctx, "test-container", "test-artifact")
	assert.NoError(t, err)
}
