// internal/cache/config_api.go
package cache

import (
	"encoding/json"
	"sync"
	"time"
)

// CacheConfig represents runtime cache configuration
type CacheConfig struct {

	// Size limits
	MemorySize int64 `json:"memory_size"`
	SSDSize    int64 `json:"ssd_size"`

	// Performance tuning
	CompressionEnabled bool   `json:"compression_enabled"`
	CompressionAlgo    string `json:"compression_algo"`
	EncryptionEnabled  bool   `json:"encryption_enabled"`
	DeduplicationOn    bool   `json:"deduplication_on"`

	// Eviction policy
	EvictionPolicy string        `json:"eviction_policy"`
	TTL            time.Duration `json:"ttl"`

	// Advanced features
	PrefetchEnabled  bool   `json:"prefetch_enabled"`
	ConsistencyLevel string `json:"consistency_level"`
	DebugMode        bool   `json:"debug_mode"`
}

// ConfigAPI provides runtime configuration management
type ConfigAPI struct {
	mu       sync.RWMutex
	config   *CacheConfig
	cache    *SSDCache
	onChange []func(*CacheConfig)
}

// NewConfigAPI creates a configuration API
func NewConfigAPI(cache *SSDCache) *ConfigAPI {
	return &ConfigAPI{
		cache: cache,
		config: &CacheConfig{
			MemorySize:         100 * 1024 * 1024,
			SSDSize:            1024 * 1024 * 1024,
			CompressionEnabled: true,
			CompressionAlgo:    "snappy",
			EvictionPolicy:     "lru",
			TTL:                1 * time.Hour,
		},
		onChange: make([]func(*CacheConfig), 0),
	}
}

// GetConfig returns current configuration
func (api *ConfigAPI) GetConfig() *CacheConfig {
	api.mu.RLock()
	defer api.mu.RUnlock()

	// Deep copy to avoid lock issues
	return &CacheConfig{
		MemorySize:         api.config.MemorySize,
		SSDSize:            api.config.SSDSize,
		CompressionEnabled: api.config.CompressionEnabled,
		CompressionAlgo:    api.config.CompressionAlgo,
		EncryptionEnabled:  api.config.EncryptionEnabled,
		DeduplicationOn:    api.config.DeduplicationOn,
		EvictionPolicy:     api.config.EvictionPolicy,
		TTL:                api.config.TTL,
		PrefetchEnabled:    api.config.PrefetchEnabled,
		ConsistencyLevel:   api.config.ConsistencyLevel,
		DebugMode:          api.config.DebugMode,
	}
}

// UpdateConfig applies new configuration
func (api *ConfigAPI) UpdateConfig(updates map[string]interface{}) error {
	api.mu.Lock()
	defer api.mu.Unlock()

	// Apply updates
	data, _ := json.Marshal(updates)
	if err := json.Unmarshal(data, api.config); err != nil {
		return err
	}

	// Apply to cache
	api.applyConfig()

	// Notify listeners
	for _, callback := range api.onChange {
		go callback(api.config)
	}

	return nil
}

// RegisterChangeListener adds a config change callback
func (api *ConfigAPI) RegisterChangeListener(callback func(*CacheConfig)) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.onChange = append(api.onChange, callback)
}

// applyConfig applies configuration to cache
func (api *ConfigAPI) applyConfig() {
	if api.cache == nil {
		return
	}

	// Apply compression
	if api.config.CompressionEnabled {
		api.cache.EnableCompression(api.config.CompressionAlgo)
	}

	// Apply deduplication
	if api.config.DeduplicationOn {
		api.cache.EnableDeduplication()
	}

	// More configuration applications would go here
}

// ValidateConfig checks if configuration is valid
func (api *ConfigAPI) ValidateConfig(config *CacheConfig) []string {
	var errors []string

	if config.MemorySize < 1024*1024 {
		errors = append(errors, "memory_size must be at least 1MB")
	}

	if config.SSDSize < config.MemorySize {
		errors = append(errors, "ssd_size must be larger than memory_size")
	}

	validAlgos := map[string]bool{"snappy": true, "gzip": true, "lz4": true}
	if !validAlgos[config.CompressionAlgo] {
		errors = append(errors, "invalid compression algorithm")
	}

	return errors
}
