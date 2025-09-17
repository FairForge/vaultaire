// internal/cache/tiered_cache.go
package cache

import (
	"fmt"
	"sync"

	"go.uber.org/zap"
)

type Config struct {
	MemorySize int64
	SSDSize    int64
	SSDPath    string
}

type TieredCache struct {
	config *Config
	logger *zap.Logger
	memory map[string][]byte
	mu     sync.RWMutex
}

func NewTieredCache(config *Config, logger *zap.Logger) *TieredCache {
	return &TieredCache{
		config: config,
		logger: logger,
		memory: make(map[string][]byte),
	}
}

func (tc *TieredCache) Get(key string) ([]byte, error) {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	data, ok := tc.memory[key]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return data, nil
}

func (tc *TieredCache) Set(key string, data []byte) error {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	tc.memory[key] = data
	return nil
}

func (tc *TieredCache) Delete(key string) error {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	delete(tc.memory, key)
	return nil
}

func (tc *TieredCache) HealthCheck() error {
	return nil
}

func (tc *TieredCache) GetMetrics() map[string]interface{} {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	return map[string]interface{}{
		"items": len(tc.memory),
	}
}

func (tc *TieredCache) Flush() {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.memory = make(map[string][]byte)
}
