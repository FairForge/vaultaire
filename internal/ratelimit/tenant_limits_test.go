// internal/ratelimit/tenant_limits_test.go
package ratelimit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTenantLimits(t *testing.T) {
	t.Run("applies tenant-specific overrides", func(t *testing.T) {
		// Arrange
		limiter := NewTenantLimiter()

		// Set default limits
		limiter.SetDefaultLimit("GET", 10, 10)

		// Override for premium tenant
		limiter.SetTenantLimit("premium-tenant", "GET", 100, 100)

		// Act & Assert
		// Regular tenant gets default
		for i := 0; i < 10; i++ {
			assert.True(t, limiter.Allow("regular-tenant", "GET"))
		}
		assert.False(t, limiter.Allow("regular-tenant", "GET"), "Regular tenant should be limited")

		// Premium tenant gets higher limit
		for i := 0; i < 50; i++ {
			assert.True(t, limiter.Allow("premium-tenant", "GET"))
		}
	})

	t.Run("supports tier-based limits", func(t *testing.T) {
		// Arrange
		limiter := NewTenantLimiter()

		// Define tiers
		limiter.SetTierLimit("free", "PUT", 1, 2)
		limiter.SetTierLimit("pro", "PUT", 10, 20)
		limiter.SetTierLimit("enterprise", "PUT", 100, 200)

		// Assign tenants to tiers
		limiter.SetTenantTier("tenant1", "free")
		limiter.SetTenantTier("tenant2", "pro")
		limiter.SetTenantTier("tenant3", "enterprise")

		// Act & Assert
		assert.True(t, limiter.Allow("tenant1", "PUT"))
		assert.True(t, limiter.Allow("tenant1", "PUT")) // burst of 2
		assert.False(t, limiter.Allow("tenant1", "PUT"), "Free tier exhausted")

		// Pro tier has higher limits
		for i := 0; i < 10; i++ {
			assert.True(t, limiter.Allow("tenant2", "PUT"))
		}
	})
}
