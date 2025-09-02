package drivers

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// TestLocalDriver_Capabilities tests capability reporting
func TestLocalDriver_Capabilities(t *testing.T) {
	driver := NewLocalDriver(t.TempDir(), zap.NewNop())

	t.Run("ReportsCapabilities", func(t *testing.T) {
		caps := driver.Capabilities()
		assert.NotEmpty(t, caps, "Should report capabilities")
		assert.Contains(t, caps, CapabilityStreaming)
		assert.Contains(t, caps, CapabilityMultipart)
	})

	t.Run("HasCapability", func(t *testing.T) {
		assert.True(t, driver.HasCapability(CapabilityStreaming))
		assert.True(t, driver.HasCapability(CapabilityAtomic))
		assert.False(t, driver.HasCapability(CapabilityVersioning))
		assert.False(t, driver.HasCapability(CapabilityEncryption))
	})
}

// TestLocalDriver_Statistics tests metrics collection
func TestLocalDriver_Statistics(t *testing.T) {
	ctx := context.Background()
	driver := NewLocalDriver(t.TempDir(), zap.NewNop())

	// Perform some operations
	_ = driver.Put(ctx, "test", "file1", bytes.NewReader([]byte("test")))
	reader, _ := driver.GetPooled(ctx, "test", "file1")
	if reader != nil {
		reader.Close()
	}

	stats := driver.GetPoolStats()
	assert.NotNil(t, stats)
	assert.Contains(t, stats, "total_reads")
}
