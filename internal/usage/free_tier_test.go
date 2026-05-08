package usage

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsFreeTier(t *testing.T) {
	assert.True(t, IsFreeTier("free"))
	assert.False(t, IsFreeTier("starter"))
	assert.False(t, IsFreeTier("professional"))
	assert.False(t, IsFreeTier(""))
}

func TestFreeTierLimits(t *testing.T) {
	assert.Equal(t, int64(5368709120), FreeTierLimits.StorageBytes)
	assert.Equal(t, int64(1073741824), FreeTierLimits.BandwidthBytes)
	assert.Equal(t, 1, FreeTierLimits.MaxBuckets)
	assert.Equal(t, 1, FreeTierLimits.MaxAPIKeys)
}
