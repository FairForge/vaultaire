package cache

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestConfigAPI_GetConfig(t *testing.T) {
	cache, _ := NewSSDCache(1024*1024, 10*1024*1024, t.TempDir())
	api := NewConfigAPI(cache)

	config := api.GetConfig()
	assert.NotNil(t, config)
	assert.Equal(t, int64(100*1024*1024), config.MemorySize)
	assert.Equal(t, "snappy", config.CompressionAlgo)
}

func TestConfigAPI_UpdateConfig(t *testing.T) {
	cache, _ := NewSSDCache(1024*1024, 10*1024*1024, t.TempDir())
	api := NewConfigAPI(cache)

	updates := map[string]interface{}{
		"memory_size":      int64(200 * 1024 * 1024),
		"debug_mode":       true,
		"compression_algo": "gzip",
	}

	err := api.UpdateConfig(updates)
	assert.NoError(t, err)

	config := api.GetConfig()
	assert.Equal(t, int64(200*1024*1024), config.MemorySize)
	assert.True(t, config.DebugMode)
	assert.Equal(t, "gzip", config.CompressionAlgo)
}

func TestConfigAPI_ValidateConfig(t *testing.T) {
	cache, _ := NewSSDCache(1024*1024, 10*1024*1024, t.TempDir())
	api := NewConfigAPI(cache)

	// Valid config
	valid := &CacheConfig{
		MemorySize:      10 * 1024 * 1024,
		SSDSize:         100 * 1024 * 1024,
		CompressionAlgo: "snappy",
	}
	errors := api.ValidateConfig(valid)
	assert.Empty(t, errors)

	// Invalid config
	invalid := &CacheConfig{
		MemorySize:      500, // Too small
		SSDSize:         100, // Smaller than memory
		CompressionAlgo: "invalid",
	}
	errors = api.ValidateConfig(invalid)
	assert.Len(t, errors, 3)
}

func TestConfigAPI_ChangeListeners(t *testing.T) {
	cache, _ := NewSSDCache(1024*1024, 10*1024*1024, t.TempDir())
	api := NewConfigAPI(cache)

	var mu sync.Mutex
	notified := false

	api.RegisterChangeListener(func(config *CacheConfig) {
		mu.Lock()
		defer mu.Unlock()
		notified = true
	})

	err := api.UpdateConfig(map[string]interface{}{
		"debug_mode": true,
	})
	assert.NoError(t, err)

	// Give callback time to run
	time.Sleep(10 * time.Millisecond)

	mu.Lock()
	wasNotified := notified
	mu.Unlock()
	assert.True(t, wasNotified)
}
