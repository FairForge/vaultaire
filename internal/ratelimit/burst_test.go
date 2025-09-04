package ratelimit

import (
    "testing"
    "time"
    
    "github.com/stretchr/testify/assert"
)

func TestBurstHandling(t *testing.T) {
    t.Run("allows burst then enforces rate", func(t *testing.T) {
        // Arrange
        limiter := NewBurstLimiter(5, 10) // 5 req/s, burst of 10
        
        // Act & Assert
        // Should allow burst of 10 immediately
        for i := 0; i < 10; i++ {
            assert.True(t, limiter.Allow(), "Should allow burst of %d", i+1)
        }
        
        // 11th request should fail
        assert.False(t, limiter.Allow(), "Should block after burst")
        
        // Wait for token refill
        time.Sleep(250 * time.Millisecond) // Should refill ~1 token (5/sec = 1/200ms)
        assert.True(t, limiter.Allow(), "Should allow after refill")
    })
    
    t.Run("refills tokens over time", func(t *testing.T) {
        // Arrange
        limiter := NewBurstLimiter(10, 5) // 10 req/s, burst of 5
        
        // Exhaust burst
        for i := 0; i < 5; i++ {
            limiter.Allow()
        }
        
        // Wait for refill
        time.Sleep(500 * time.Millisecond) // Should refill 5 tokens (10/sec * 0.5s)
        
        // Should allow 5 more requests
        allowed := 0
        for i := 0; i < 10; i++ {
            if limiter.Allow() {
                allowed++
            }
        }
        
        assert.GreaterOrEqual(t, allowed, 4, "Should have refilled tokens")
        assert.LessOrEqual(t, allowed, 6, "Should not exceed burst")
    })
    
    t.Run("adaptive burst increases for good tenants", func(t *testing.T) {
        // Arrange
        limiter := NewAdaptiveBurstLimiter(100, 10, 50) // 100 req/s (high rate), burst 10->50
        tenant := "good-tenant"
        
        // Make 21 good requests to trigger increase (need >=20)
        for i := 0; i < 21; i++ {
            allowed := limiter.Allow(tenant)
            assert.True(t, allowed, "Request %d should be allowed", i+1)
            time.Sleep(5 * time.Millisecond) // Stay well under rate limit
        }
        
        // Check burst increased
        currentBurst := limiter.GetBurst(tenant)
        assert.Greater(t, currentBurst, 10, "Burst should increase for good behavior")
        assert.LessOrEqual(t, currentBurst, 50, "Burst should not exceed max")
    })
}
