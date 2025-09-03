// internal/drivers/throttle_test.go
package drivers

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/time/rate"
)

func TestBandwidthThrottle(t *testing.T) {
	t.Run("throttles read operations", func(t *testing.T) {
		// Create 5KB of data
		dataSize := 5 * 1024
		data := make([]byte, dataSize)
		for i := range data {
			data[i] = byte(i % 256)
		}

		// 5KB/s rate, 1KB burst - should take ~1 second
		limiter := rate.NewLimiter(rate.Limit(5*1024), 1024)
		throttled := &throttledReader{
			reader:  bytes.NewReader(data),
			limiter: limiter,
			ctx:     context.Background(),
		}

		// Read in small chunks to avoid burst limit
		start := time.Now()
		buffer := make([]byte, 512) // Read 512 bytes at a time
		totalRead := 0

		for {
			n, err := throttled.Read(buffer)
			totalRead += n
			if err != nil {
				break
			}
		}
		duration := time.Since(start)

		// Assert
		assert.Equal(t, dataSize, totalRead)

		// Should take at least 0.6 seconds (allowing for burst and timing variations)
		assert.GreaterOrEqual(t, duration.Seconds(), 0.6,
			"Read too fast for 5KB/s limit")
	})
}
