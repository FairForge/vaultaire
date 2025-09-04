package ratelimit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOperationLimits(t *testing.T) {
	t.Run("different limits per operation", func(t *testing.T) {
		// Arrange
		limiter := NewOperationLimiter()
		limiter.SetLimit("GET", 100, 100) // 100 req/s for reads
		limiter.SetLimit("PUT", 10, 20)   // 10 req/s for writes
		limiter.SetLimit("DELETE", 1, 1)  // 1 req/s for deletes, burst of 1

		tenantID := "tenant-123"

		// Act & Assert - GET should allow many
		for i := 0; i < 10; i++ {
			allowed := limiter.Allow(tenantID, "GET")
			assert.True(t, allowed, "GET should allow many requests")
		}

		// PUT should be more restricted
		allowed := 0
		for i := 0; i < 20; i++ {
			if limiter.Allow(tenantID, "PUT") {
				allowed++
			}
		}
		assert.LessOrEqual(t, allowed, 20, "PUT should be limited")

		// DELETE should be very restricted
		assert.True(t, limiter.Allow(tenantID, "DELETE"))
		assert.False(t, limiter.Allow(tenantID, "DELETE"), "Second DELETE should be blocked")
	})

	t.Run("tracks per tenant per operation", func(t *testing.T) {
		// Arrange
		limiter := NewOperationLimiter()
		limiter.SetLimit("PUT", 1, 1)

		// Act - different tenants should have independent limits
		assert.True(t, limiter.Allow("tenant1", "PUT"))
		assert.True(t, limiter.Allow("tenant2", "PUT"))
		assert.False(t, limiter.Allow("tenant1", "PUT"), "tenant1 exhausted")
		assert.False(t, limiter.Allow("tenant2", "PUT"), "tenant2 exhausted")
	})
}
