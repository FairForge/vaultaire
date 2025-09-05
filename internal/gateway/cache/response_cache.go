// internal/gateway/cache/response_cache.go
package cache

import (
	"context"
	"crypto/md5"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// CacheEntry represents a cached response
type CacheEntry struct {
	Data       []byte
	Headers    http.Header
	StatusCode int
	ETag       string
	CachedAt   time.Time
	ExpiresAt  time.Time
}

// Config for response cache
type Config struct {
	MaxSize         int           // Maximum number of entries
	DefaultTTL      time.Duration // Default time-to-live
	CleanupInterval time.Duration // How often to clean expired entries
}

// DefaultConfig returns sensible defaults
func DefaultConfig() Config {
	return Config{
		MaxSize:         10000,
		DefaultTTL:      5 * time.Minute,
		CleanupInterval: 1 * time.Minute,
	}
}

// ResponseCache implements HTTP response caching
type ResponseCache struct {
	mu      sync.RWMutex
	entries map[string]*CacheEntry
	config  Config
	done    chan struct{}
}

// NewResponseCache creates a new response cache
func NewResponseCache(config Config) *ResponseCache {
	cache := &ResponseCache{
		entries: make(map[string]*CacheEntry),
		config:  config,
		done:    make(chan struct{}),
	}

	// Start cleanup goroutine
	go cache.cleanupExpired()

	return cache
}

// GenerateKey creates a cache key from request
func (c *ResponseCache) GenerateKey(req *http.Request) string {
	var parts []string

	// Include tenant if present
	if tenantID := req.Header.Get("X-Tenant-ID"); tenantID != "" {
		parts = append(parts, tenantID)
	}

	// Method and path
	parts = append(parts, req.Method, req.URL.Path)

	// Include query string if present
	if req.URL.RawQuery != "" {
		parts[len(parts)-1] += "?" + req.URL.RawQuery
	}

	return strings.Join(parts, ":")
}

// GenerateKeyWithVary creates a cache key considering Vary headers
func (c *ResponseCache) GenerateKeyWithVary(req *http.Request, varyHeaders []string) string {
	baseKey := c.GenerateKey(req)

	if len(varyHeaders) == 0 {
		return baseKey
	}

	// Sort vary headers for consistent keys
	sort.Strings(varyHeaders)

	var varyParts []string
	for _, header := range varyHeaders {
		value := req.Header.Get(header)
		if value != "" {
			varyParts = append(varyParts, fmt.Sprintf("%s=%s", header, value))
		}
	}

	if len(varyParts) > 0 {
		return baseKey + ":" + strings.Join(varyParts, ",")
	}

	return baseKey
}

// Get retrieves an entry from cache
func (c *ResponseCache) Get(ctx context.Context, key string) (*CacheEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, exists := c.entries[key]
	if !exists {
		return nil, false
	}

	// Check if expired
	if time.Now().After(entry.ExpiresAt) {
		return nil, false
	}

	return entry, true
}

// Set stores an entry in cache
func (c *ResponseCache) Set(ctx context.Context, key string, entry *CacheEntry, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Enforce max size with simple eviction
	if len(c.entries) >= c.config.MaxSize {
		// Remove oldest entry (simple strategy)
		var oldestKey string
		var oldestTime time.Time
		for k, v := range c.entries {
			if oldestTime.IsZero() || v.CachedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.CachedAt
			}
		}
		if oldestKey != "" {
			delete(c.entries, oldestKey)
		}
	}

	entry.ExpiresAt = time.Now().Add(ttl)
	c.entries[key] = entry
}

// Invalidate removes a specific key
func (c *ResponseCache) Invalidate(ctx context.Context, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, key)
}

// InvalidatePattern removes all keys matching pattern
func (c *ResponseCache) InvalidatePattern(ctx context.Context, pattern string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Simple pattern matching with * wildcard
	pattern = strings.ReplaceAll(pattern, "*", "")

	for key := range c.entries {
		if strings.Contains(key, pattern) {
			delete(c.entries, key)
		}
	}
}

// GenerateETag creates an ETag from content
func (c *ResponseCache) GenerateETag(data []byte) string {
	hash := md5.Sum(data)
	return fmt.Sprintf(`"%x"`, hash)
}

// ValidateETag checks if request has matching ETag
func (c *ResponseCache) ValidateETag(req *http.Request, entry *CacheEntry) bool {
	ifNoneMatch := req.Header.Get("If-None-Match")
	if ifNoneMatch == "" {
		return false
	}

	return ifNoneMatch == entry.ETag
}

// cleanupExpired periodically removes expired entries
func (c *ResponseCache) cleanupExpired() {
	ticker := time.NewTicker(c.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.mu.Lock()
			now := time.Now()
			for key, entry := range c.entries {
				if now.After(entry.ExpiresAt) {
					delete(c.entries, key)
				}
			}
			c.mu.Unlock()

		case <-c.done:
			return
		}
	}
}

// Stop shuts down the cache
func (c *ResponseCache) Stop() {
	close(c.done)
}
