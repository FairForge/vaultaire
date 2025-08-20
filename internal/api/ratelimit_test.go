package api

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRateLimiter_Construction(t *testing.T) {
	t.Run("creates with defaults", func(t *testing.T) {
		rl := NewRateLimiter()

		assert.NotNil(t, rl, "should not be nil")
		assert.Equal(t, 100, rl.requestsPerSecond)
		assert.Equal(t, 200, rl.burstSize)
	})
}

func TestRateLimiter_Allow(t *testing.T) {
	t.Run("allows within limit", func(t *testing.T) {
		rl := NewRateLimiter()
		rl.requestsPerSecond = 10
		rl.burstSize = 10

		for i := 0; i < 10; i++ {
			assert.True(t, rl.Allow("test"))
		}
	})

	t.Run("blocks over limit", func(t *testing.T) {
		rl := NewRateLimiter()
		rl.requestsPerSecond = 1
		rl.burstSize = 2

		assert.True(t, rl.Allow("test"))
		assert.True(t, rl.Allow("test"))
		assert.False(t, rl.Allow("test"))
	})
}

func TestRateLimiter_MemoryBounds(t *testing.T) {
	t.Run("prevents unlimited growth", func(t *testing.T) {
		rl := NewRateLimiter()

		for i := 0; i < 10001; i++ {
			rl.Allow(fmt.Sprintf("tenant-%d", i))
		}

		rl.mu.RLock()
		count := len(rl.limiters)
		rl.mu.RUnlock()

		assert.LessOrEqual(t, count, 10000)
	})
}
