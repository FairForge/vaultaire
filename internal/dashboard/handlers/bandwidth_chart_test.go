package handlers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildChartBars_Empty(t *testing.T) {
	bars := BuildChartBars(nil)
	assert.Nil(t, bars)
}

func TestBuildChartBars_SingleDay(t *testing.T) {
	days := []BandwidthDay{
		{Date: time.Date(2026, 4, 7, 0, 0, 0, 0, time.UTC), Ingress: 1000, Egress: 500},
	}

	bars := BuildChartBars(days)
	require.Len(t, bars, 1)
	assert.Equal(t, "Apr 7", bars[0].Label)
	assert.True(t, bars[0].ShowLabel)
	assert.True(t, bars[0].InH > 0)
	assert.True(t, bars[0].EgH > 0)
}

func TestBuildChartBars_MultipleDays(t *testing.T) {
	days := make([]BandwidthDay, 10)
	base := time.Date(2026, 3, 28, 0, 0, 0, 0, time.UTC)
	for i := range days {
		days[i] = BandwidthDay{
			Date:    base.AddDate(0, 0, i),
			Ingress: int64((i + 1) * 1024 * 1024),
			Egress:  int64((i + 1) * 512 * 1024),
		}
	}

	bars := BuildChartBars(days)
	require.Len(t, bars, 10)

	// First and every 5th bar should show label.
	assert.True(t, bars[0].ShowLabel)
	assert.False(t, bars[1].ShowLabel)
	assert.True(t, bars[5].ShowLabel)
	// Last bar always shows label.
	assert.True(t, bars[9].ShowLabel)

	// Last bar should have the tallest total (max values).
	assert.True(t, bars[9].InH >= bars[0].InH)
}

func TestBuildChartBars_MinimumHeight(t *testing.T) {
	// A day with tiny non-zero values should still get min 2px height.
	days := []BandwidthDay{
		{Date: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), Ingress: 1000000, Egress: 1000000},
		{Date: time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC), Ingress: 1, Egress: 1},
	}

	bars := BuildChartBars(days)
	require.Len(t, bars, 2)
	// The second bar should have minimum 2px for each non-zero dimension.
	assert.True(t, bars[1].InH >= 2)
	assert.True(t, bars[1].EgH >= 2)
}

func TestBuildChartBars_ZeroMax(t *testing.T) {
	// All zeros should still produce bars with zero height.
	days := []BandwidthDay{
		{Date: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), Ingress: 0, Egress: 0},
	}

	bars := BuildChartBars(days)
	require.Len(t, bars, 1)
	assert.Equal(t, 0.0, bars[0].InH)
	assert.Equal(t, 0.0, bars[0].EgH)
}
