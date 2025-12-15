// internal/perf/caching_test.go
package perf

import (
	"sync"
	"testing"
	"time"
)

func TestNewLRUCache(t *testing.T) {
	cache := NewLRUCache(10)

	if cache == nil {
		t.Fatal("expected non-nil cache")
	}
	if cache.Len() != 0 {
		t.Error("expected empty cache")
	}
}

func TestLRUCacheSetGet(t *testing.T) {
	cache := NewLRUCache(10)

	cache.Set("key1", "value1", 0)

	val, ok := cache.Get("key1")
	if !ok {
		t.Fatal("expected value")
	}
	if val != "value1" {
		t.Errorf("expected value1, got %v", val)
	}
}

func TestLRUCacheGetMiss(t *testing.T) {
	cache := NewLRUCache(10)

	_, ok := cache.Get("nonexistent")
	if ok {
		t.Error("expected miss")
	}

	stats := cache.Stats()
	if stats.Misses != 1 {
		t.Errorf("expected 1 miss, got %d", stats.Misses)
	}
}

func TestLRUCacheEviction(t *testing.T) {
	cache := NewLRUCache(3)

	cache.Set("key1", "value1", 0)
	cache.Set("key2", "value2", 0)
	cache.Set("key3", "value3", 0)
	cache.Set("key4", "value4", 0) // Should evict key1

	_, ok := cache.Get("key1")
	if ok {
		t.Error("key1 should have been evicted")
	}

	_, ok = cache.Get("key4")
	if !ok {
		t.Error("key4 should exist")
	}
}

func TestLRUCacheLRUOrder(t *testing.T) {
	cache := NewLRUCache(3)

	cache.Set("key1", "value1", 0)
	cache.Set("key2", "value2", 0)
	cache.Set("key3", "value3", 0)

	// Access key1 to make it recently used
	cache.Get("key1")

	// Add key4, should evict key2 (least recently used)
	cache.Set("key4", "value4", 0)

	_, ok := cache.Get("key2")
	if ok {
		t.Error("key2 should have been evicted")
	}

	_, ok = cache.Get("key1")
	if !ok {
		t.Error("key1 should still exist")
	}
}

func TestLRUCacheTTL(t *testing.T) {
	cache := NewLRUCache(10)

	cache.Set("key1", "value1", 50*time.Millisecond)

	val, ok := cache.Get("key1")
	if !ok || val != "value1" {
		t.Error("expected value before expiry")
	}

	time.Sleep(100 * time.Millisecond)

	_, ok = cache.Get("key1")
	if ok {
		t.Error("expected miss after expiry")
	}
}

func TestLRUCacheDelete(t *testing.T) {
	cache := NewLRUCache(10)

	cache.Set("key1", "value1", 0)
	cache.Delete("key1")

	_, ok := cache.Get("key1")
	if ok {
		t.Error("expected key to be deleted")
	}
}

func TestLRUCacheClear(t *testing.T) {
	cache := NewLRUCache(10)

	cache.Set("key1", "value1", 0)
	cache.Set("key2", "value2", 0)
	cache.Clear()

	if cache.Len() != 0 {
		t.Error("expected empty cache after clear")
	}
}

func TestLRUCacheStats(t *testing.T) {
	cache := NewLRUCache(10)

	cache.Set("key1", "value1", 0)
	cache.Get("key1") // Hit
	cache.Get("key2") // Miss

	stats := cache.Stats()

	if stats.Hits != 1 {
		t.Errorf("expected 1 hit, got %d", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Errorf("expected 1 miss, got %d", stats.Misses)
	}
	if stats.HitRate != 50.0 {
		t.Errorf("expected 50%% hit rate, got %.1f%%", stats.HitRate)
	}
}

func TestLRUCacheUpdate(t *testing.T) {
	cache := NewLRUCache(10)

	cache.Set("key1", "value1", 0)
	cache.Set("key1", "value2", 0)

	val, _ := cache.Get("key1")
	if val != "value2" {
		t.Errorf("expected updated value, got %v", val)
	}

	if cache.Len() != 1 {
		t.Error("update should not increase count")
	}
}

func TestTTLCache(t *testing.T) {
	cache := NewTTLCache(time.Minute, 100)

	cache.Set("key1", "value1", 0)

	val, ok := cache.Get("key1")
	if !ok || val != "value1" {
		t.Error("expected value")
	}
}

func TestTTLCacheExpiry(t *testing.T) {
	cache := NewTTLCache(50*time.Millisecond, 100)

	cache.Set("key1", "value1", 0)

	time.Sleep(100 * time.Millisecond)

	_, ok := cache.Get("key1")
	if ok {
		t.Error("expected miss after expiry")
	}
}

func TestTTLCacheDelete(t *testing.T) {
	cache := NewTTLCache(time.Minute, 100)

	cache.Set("key1", "value1", 0)
	cache.Delete("key1")

	_, ok := cache.Get("key1")
	if ok {
		t.Error("expected key to be deleted")
	}
}

func TestTTLCacheClear(t *testing.T) {
	cache := NewTTLCache(time.Minute, 100)

	cache.Set("key1", "value1", 0)
	cache.Set("key2", "value2", 0)
	cache.Clear()

	if cache.Len() != 0 {
		t.Error("expected empty cache")
	}
}

func TestShardedCache(t *testing.T) {
	cache := NewShardedCache(4, 100)

	cache.Set("key1", "value1", 0)

	val, ok := cache.Get("key1")
	if !ok || val != "value1" {
		t.Error("expected value")
	}
}

func TestShardedCacheDistribution(t *testing.T) {
	cache := NewShardedCache(4, 100)

	// Add many keys
	for i := 0; i < 100; i++ {
		cache.Set(string(rune('a'+i)), i, 0)
	}

	stats := cache.Stats()
	if stats.Count < 50 {
		t.Error("expected keys distributed across shards")
	}
}

func TestShardedCacheDelete(t *testing.T) {
	cache := NewShardedCache(4, 100)

	cache.Set("key1", "value1", 0)
	cache.Delete("key1")

	_, ok := cache.Get("key1")
	if ok {
		t.Error("expected key deleted")
	}
}

func TestShardedCacheClear(t *testing.T) {
	cache := NewShardedCache(4, 100)

	for i := 0; i < 20; i++ {
		cache.Set(string(rune('a'+i)), i, 0)
	}

	cache.Clear()

	stats := cache.Stats()
	if stats.Count != 0 {
		t.Error("expected empty after clear")
	}
}

func TestShardedCacheStats(t *testing.T) {
	cache := NewShardedCache(4, 100)

	cache.Set("key1", "value1", 0)
	cache.Get("key1")
	cache.Get("nonexistent")

	stats := cache.Stats()

	if stats.Hits != 1 {
		t.Errorf("expected 1 hit, got %d", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Errorf("expected 1 miss, got %d", stats.Misses)
	}
}

func TestShardedCacheConcurrent(t *testing.T) {
	cache := NewShardedCache(8, 1000)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := string(rune('a' + n%26))
			cache.Set(key, n, 0)
			cache.Get(key)
		}(i)
	}
	wg.Wait()

	stats := cache.Stats()
	if stats.Hits == 0 {
		t.Error("expected some hits")
	}
}

func TestCacheWarmer(t *testing.T) {
	cache := NewLRUCache(100)
	loader := func(key string) (interface{}, error) {
		return "loaded_" + key, nil
	}

	warmer := NewCacheWarmer(cache, loader)
	err := warmer.Warm([]string{"a", "b", "c"}, time.Minute)
	if err != nil {
		t.Fatalf("warm failed: %v", err)
	}

	val, ok := cache.Get("a")
	if !ok || val != "loaded_a" {
		t.Error("expected warmed value")
	}
}

func TestCacheWarmerAsync(t *testing.T) {
	cache := NewLRUCache(100)
	loader := func(key string) (interface{}, error) {
		time.Sleep(10 * time.Millisecond)
		return "loaded_" + key, nil
	}

	warmer := NewCacheWarmer(cache, loader)
	errCh := warmer.WarmAsync([]string{"a", "b", "c", "d"}, time.Minute, 2)

	for err := range errCh {
		if err != nil {
			t.Fatalf("async warm failed: %v", err)
		}
	}

	val, ok := cache.Get("d")
	if !ok || val != "loaded_d" {
		t.Error("expected warmed value")
	}
}

func TestNextPowerOf2(t *testing.T) {
	tests := []struct {
		input    int
		expected int
	}{
		{1, 1},
		{2, 2},
		{3, 4},
		{5, 8},
		{7, 8},
		{9, 16},
	}

	for _, tc := range tests {
		result := nextPowerOf2(tc.input)
		if result != tc.expected {
			t.Errorf("nextPowerOf2(%d) = %d, want %d", tc.input, result, tc.expected)
		}
	}
}

func TestCacheStrategyConstants(t *testing.T) {
	strategies := []CacheStrategy{
		StrategyLRU,
		StrategyLFU,
		StrategyTTL,
		StrategyARC,
		StrategyFIFO,
	}

	for _, s := range strategies {
		if s == "" {
			t.Error("strategy should not be empty")
		}
	}
}

func TestCacheEntryFields(t *testing.T) {
	entry := CacheEntry{
		Key:         "test",
		Value:       "value",
		Size:        100,
		CreatedAt:   time.Now(),
		AccessedAt:  time.Now(),
		ExpiresAt:   time.Now().Add(time.Hour),
		AccessCount: 5,
	}

	if entry.Key == "" {
		t.Error("key should not be empty")
	}
	if entry.AccessCount != 5 {
		t.Error("unexpected access count")
	}
}
