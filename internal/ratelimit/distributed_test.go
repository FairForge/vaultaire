package ratelimit

import (
    "testing"
    "time"
    
    "github.com/stretchr/testify/assert"
)

func TestDistributedRateLimiter(t *testing.T) {
    t.Run("sliding window algorithm", func(t *testing.T) {
        // Arrange
        limiter := NewSlidingWindowLimiter(10, time.Second) // 10 req/s
        
        // Act - make 5 requests
        for i := 0; i < 5; i++ {
            assert.True(t, limiter.Allow("test-key"))
        }
        
        // Wait for them to expire
        time.Sleep(1100 * time.Millisecond)
        
        // Should allow 10 more (window reset)
        for i := 0; i < 10; i++ {
            assert.True(t, limiter.Allow("test-key"))
        }
        
        // 11th should fail
        assert.False(t, limiter.Allow("test-key"))
    })
    
    t.Run("fixed window counter", func(t *testing.T) {
        // Arrange
        limiter := NewFixedWindowLimiter(5, time.Second)
        
        // Exhaust window
        for i := 0; i < 5; i++ {
            assert.True(t, limiter.Allow("key1"))
        }
        assert.False(t, limiter.Allow("key1"))
        
        // Wait for new window
        time.Sleep(1100 * time.Millisecond)
        
        // New window should reset
        assert.True(t, limiter.Allow("key1"))
    })
    
    t.Run("sliding window smooth limiting", func(t *testing.T) {
        // Arrange
        limiter := NewSlidingWindowLimiter(5, time.Second)
        
        // Use half the limit
        for i := 0; i < 3; i++ {
            assert.True(t, limiter.Allow("test2"))
        }
        
        // Wait a bit
        time.Sleep(200 * time.Millisecond)
        
        // Should still allow 2 more
        assert.True(t, limiter.Allow("test2"))
        assert.True(t, limiter.Allow("test2"))
        
        // But not a 6th
        assert.False(t, limiter.Allow("test2"))
    })
}
