// internal/slo/slo_test.go
package slo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSLOConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &SLOConfig{
			Name:      "api-availability",
			Target:    99.9,
			Window:    30 * 24 * time.Hour,
			Indicator: IndicatorAvailability,
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &SLOConfig{Target: 99.9}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})

	t.Run("rejects invalid target", func(t *testing.T) {
		config := &SLOConfig{Name: "test", Target: 101}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "target")
	})
}

func TestNewSLOManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewSLOManager(nil)
		assert.NotNil(t, manager)
	})
}

func TestSLOManager_Register(t *testing.T) {
	manager := NewSLOManager(nil)

	t.Run("registers SLO", func(t *testing.T) {
		config := &SLOConfig{
			Name:      "api-latency",
			Target:    99.0,
			Window:    7 * 24 * time.Hour,
			Indicator: IndicatorLatency,
			Threshold: 200, // 200ms
		}

		slo, err := manager.Register(config)
		require.NoError(t, err)
		assert.Equal(t, "api-latency", slo.Name())
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		config := &SLOConfig{Name: "duplicate", Target: 99.0, Indicator: IndicatorAvailability}
		_, _ = manager.Register(config)
		_, err := manager.Register(config)
		assert.Error(t, err)
	})
}

func TestSLO_RecordSuccess(t *testing.T) {
	manager := NewSLOManager(nil)
	slo, _ := manager.Register(&SLOConfig{
		Name:      "availability-slo",
		Target:    99.0,
		Window:    time.Hour,
		Indicator: IndicatorAvailability,
	})

	t.Run("records successful events", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			slo.RecordSuccess()
		}

		status := slo.Status()
		assert.Equal(t, int64(100), status.TotalEvents)
		assert.Equal(t, int64(100), status.GoodEvents)
	})
}

func TestSLO_RecordFailure(t *testing.T) {
	manager := NewSLOManager(nil)
	slo, _ := manager.Register(&SLOConfig{
		Name:      "availability-slo-2",
		Target:    99.0,
		Window:    time.Hour,
		Indicator: IndicatorAvailability,
	})

	t.Run("records failed events", func(t *testing.T) {
		for i := 0; i < 99; i++ {
			slo.RecordSuccess()
		}
		slo.RecordFailure()

		status := slo.Status()
		assert.Equal(t, int64(100), status.TotalEvents)
		assert.Equal(t, int64(99), status.GoodEvents)
		assert.Equal(t, int64(1), status.BadEvents)
	})
}

func TestSLO_CurrentValue(t *testing.T) {
	manager := NewSLOManager(nil)
	slo, _ := manager.Register(&SLOConfig{
		Name:      "current-value-slo",
		Target:    99.0,
		Window:    time.Hour,
		Indicator: IndicatorAvailability,
	})

	t.Run("calculates current SLI", func(t *testing.T) {
		for i := 0; i < 995; i++ {
			slo.RecordSuccess()
		}
		for i := 0; i < 5; i++ {
			slo.RecordFailure()
		}

		value := slo.CurrentValue()
		assert.InDelta(t, 99.5, value, 0.1)
	})
}

func TestSLO_IsMet(t *testing.T) {
	manager := NewSLOManager(nil)
	slo, _ := manager.Register(&SLOConfig{
		Name:      "is-met-slo",
		Target:    99.0,
		Window:    time.Hour,
		Indicator: IndicatorAvailability,
	})

	t.Run("returns true when met", func(t *testing.T) {
		for i := 0; i < 995; i++ {
			slo.RecordSuccess()
		}
		for i := 0; i < 5; i++ {
			slo.RecordFailure()
		}

		assert.True(t, slo.IsMet())
	})

	t.Run("returns false when not met", func(t *testing.T) {
		slo2, _ := manager.Register(&SLOConfig{
			Name:      "not-met-slo",
			Target:    99.0,
			Window:    time.Hour,
			Indicator: IndicatorAvailability,
		})

		for i := 0; i < 90; i++ {
			slo2.RecordSuccess()
		}
		for i := 0; i < 10; i++ {
			slo2.RecordFailure()
		}

		assert.False(t, slo2.IsMet())
	})
}

func TestSLO_ErrorBudget(t *testing.T) {
	manager := NewSLOManager(nil)
	slo, _ := manager.Register(&SLOConfig{
		Name:      "error-budget-slo",
		Target:    99.0,
		Window:    time.Hour,
		Indicator: IndicatorAvailability,
	})

	t.Run("calculates error budget", func(t *testing.T) {
		// 99% target = 1% error budget
		budget := slo.ErrorBudget()
		assert.Equal(t, 1.0, budget.Total)
	})

	t.Run("tracks remaining budget", func(t *testing.T) {
		for i := 0; i < 995; i++ {
			slo.RecordSuccess()
		}
		for i := 0; i < 5; i++ {
			slo.RecordFailure()
		}

		budget := slo.ErrorBudget()
		assert.InDelta(t, 0.5, budget.Remaining, 0.1)
		assert.InDelta(t, 0.5, budget.Consumed, 0.1)
	})
}

func TestSLO_BurnRate(t *testing.T) {
	manager := NewSLOManager(nil)
	slo, _ := manager.Register(&SLOConfig{
		Name:      "burn-rate-slo",
		Target:    99.0,
		Window:    30 * 24 * time.Hour, // 30 days
		Indicator: IndicatorAvailability,
	})

	t.Run("calculates burn rate", func(t *testing.T) {
		// Simulate high error rate
		for i := 0; i < 90; i++ {
			slo.RecordSuccess()
		}
		for i := 0; i < 10; i++ {
			slo.RecordFailure()
		}

		burnRate := slo.BurnRate(time.Hour)
		// 10% errors vs 1% budget = 10x burn rate
		assert.Greater(t, burnRate, 1.0)
	})
}

func TestSLO_LatencyIndicator(t *testing.T) {
	manager := NewSLOManager(nil)
	slo, _ := manager.Register(&SLOConfig{
		Name:      "latency-slo",
		Target:    95.0,
		Window:    time.Hour,
		Indicator: IndicatorLatency,
		Threshold: 200, // 200ms threshold
	})

	t.Run("records latency as good when under threshold", func(t *testing.T) {
		slo.RecordLatency(150 * time.Millisecond)
		slo.RecordLatency(100 * time.Millisecond)
		slo.RecordLatency(50 * time.Millisecond)

		status := slo.Status()
		assert.Equal(t, int64(3), status.GoodEvents)
	})

	t.Run("records latency as bad when over threshold", func(t *testing.T) {
		slo.RecordLatency(250 * time.Millisecond)
		slo.RecordLatency(300 * time.Millisecond)

		status := slo.Status()
		assert.Equal(t, int64(2), status.BadEvents)
	})
}

func TestSLO_ErrorRateIndicator(t *testing.T) {
	manager := NewSLOManager(nil)
	slo, _ := manager.Register(&SLOConfig{
		Name:      "error-rate-slo",
		Target:    99.5,
		Window:    time.Hour,
		Indicator: IndicatorErrorRate,
	})

	t.Run("tracks error rate", func(t *testing.T) {
		for i := 0; i < 995; i++ {
			slo.RecordSuccess()
		}
		for i := 0; i < 5; i++ {
			slo.RecordFailure()
		}

		errorRate := slo.ErrorRate()
		assert.InDelta(t, 0.5, errorRate, 0.1)
	})
}

func TestSLO_ThroughputIndicator(t *testing.T) {
	manager := NewSLOManager(nil)
	slo, _ := manager.Register(&SLOConfig{
		Name:      "throughput-slo",
		Target:    99.0,
		Window:    time.Hour,
		Indicator: IndicatorThroughput,
		Threshold: 1000, // 1000 req/s minimum
	})

	t.Run("records throughput", func(t *testing.T) {
		slo.RecordThroughput(1500) // Above threshold
		slo.RecordThroughput(500)  // Below threshold

		status := slo.Status()
		assert.Equal(t, int64(1), status.GoodEvents)
		assert.Equal(t, int64(1), status.BadEvents)
	})
}

func TestSLOManager_List(t *testing.T) {
	manager := NewSLOManager(nil)
	_, _ = manager.Register(&SLOConfig{Name: "slo-1", Target: 99.0, Indicator: IndicatorAvailability})
	_, _ = manager.Register(&SLOConfig{Name: "slo-2", Target: 99.5, Indicator: IndicatorLatency})

	t.Run("lists all SLOs", func(t *testing.T) {
		slos := manager.List()
		assert.Len(t, slos, 2)
	})
}

func TestSLOManager_Get(t *testing.T) {
	manager := NewSLOManager(nil)
	_, _ = manager.Register(&SLOConfig{Name: "get-test", Target: 99.0, Indicator: IndicatorAvailability})

	t.Run("gets SLO by name", func(t *testing.T) {
		slo := manager.Get("get-test")
		assert.NotNil(t, slo)
		assert.Equal(t, "get-test", slo.Name())
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		slo := manager.Get("unknown")
		assert.Nil(t, slo)
	})
}

func TestSLOManager_Summary(t *testing.T) {
	manager := NewSLOManager(nil)
	slo1, _ := manager.Register(&SLOConfig{Name: "summary-1", Target: 99.0, Window: time.Hour, Indicator: IndicatorAvailability})
	slo2, _ := manager.Register(&SLOConfig{Name: "summary-2", Target: 99.0, Window: time.Hour, Indicator: IndicatorAvailability})

	// slo1 meets target
	for i := 0; i < 100; i++ {
		slo1.RecordSuccess()
	}

	// slo2 does not meet target
	for i := 0; i < 90; i++ {
		slo2.RecordSuccess()
	}
	for i := 0; i < 10; i++ {
		slo2.RecordFailure()
	}

	t.Run("returns summary", func(t *testing.T) {
		summary := manager.Summary()
		assert.Equal(t, 2, summary.Total)
		assert.Equal(t, 1, summary.Meeting)
		assert.Equal(t, 1, summary.Breaching)
	})
}

func TestIndicators(t *testing.T) {
	t.Run("defines indicators", func(t *testing.T) {
		assert.Equal(t, "availability", IndicatorAvailability)
		assert.Equal(t, "latency", IndicatorLatency)
		assert.Equal(t, "error_rate", IndicatorErrorRate)
		assert.Equal(t, "throughput", IndicatorThroughput)
	})
}

func TestSLOStatus(t *testing.T) {
	manager := NewSLOManager(nil)
	slo, _ := manager.Register(&SLOConfig{
		Name:      "status-test",
		Target:    99.0,
		Window:    time.Hour,
		Indicator: IndicatorAvailability,
	})

	for i := 0; i < 100; i++ {
		slo.RecordSuccess()
	}

	t.Run("returns complete status", func(t *testing.T) {
		status := slo.Status()
		assert.Equal(t, "status-test", status.Name)
		assert.Equal(t, 99.0, status.Target)
		assert.Equal(t, int64(100), status.TotalEvents)
		assert.True(t, status.IsMet)
	})
}

func TestSLO_TimeWindow(t *testing.T) {
	manager := NewSLOManager(nil)
	slo, _ := manager.Register(&SLOConfig{
		Name:      "window-test",
		Target:    99.0,
		Window:    time.Hour,
		Indicator: IndicatorAvailability,
	})

	t.Run("respects time window", func(t *testing.T) {
		// Events within window should be counted
		slo.RecordSuccess()

		status := slo.Status()
		assert.Equal(t, int64(1), status.TotalEvents)
	})
}
