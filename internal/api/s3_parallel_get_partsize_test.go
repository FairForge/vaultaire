package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sizedRangedDriver is the ranged backend with its own preferred range size
// (engine.RangePartSizer) — Blomp's shape: every range pays a slow first
// byte, so it is read in parts the size of its SLO pieces.
type sizedRangedDriver struct {
	*rangedDriver
	part int64
}

func (d *sizedRangedDriver) PreferredRangeBytes() int64 { return d.part }

func TestParallelGet_ADriverWithAPreferredRangeSizeIsReadInItsParts(t *testing.T) {
	// Arrange: the fixture's parts are 256 KiB; this driver prefers 1 MiB.
	f := setupParallelFixture(t, true)
	drv := &sizedRangedDriver{rangedDriver: newRangedDriver(), part: 1 << 20}
	f.eng.AddDriver("sized", drv)
	f.eng.SetPrimary("sized")
	f.drv = drv.rangedDriver
	content := randomBytes(3<<20 + 12345)
	f.put(t, "big.bin", content)

	// Act
	w := f.get(t, "big.bin", nil)

	// Assert
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, content, w.Body.Bytes(), "exact bytes, in order")
	assert.Equal(t, int32(0), drv.gets.Load(), "no single-stream GET")
	assert.Equal(t, int32(4), drv.ranges.Load(), "four 1 MiB parts, not thirteen 256 KiB ones")
}

func TestParallelGet_APreferredRangeSizeOfZeroKeepsTheConfiguredParts(t *testing.T) {
	// Arrange
	f := setupParallelFixture(t, true)
	drv := &sizedRangedDriver{rangedDriver: newRangedDriver()}
	f.eng.AddDriver("sized", drv)
	f.eng.SetPrimary("sized")
	content := randomBytes(3<<20 + 12345)
	f.put(t, "big.bin", content)

	// Act
	w := f.get(t, "big.bin", nil)

	// Assert
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, content, w.Body.Bytes())
	assert.Equal(t, int32(13), drv.ranges.Load(), "the configured 256 KiB parts")
}
