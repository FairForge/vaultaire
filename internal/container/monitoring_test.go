// internal/container/monitoring_test.go
package container

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainerMetrics(t *testing.T) {
	t.Run("creates container metrics", func(t *testing.T) {
		metrics := &ContainerMetrics{
			ContainerID:   "abc123",
			Name:          "vaultaire",
			CPUPercent:    45.5,
			MemoryUsage:   512 * 1024 * 1024,
			MemoryLimit:   1024 * 1024 * 1024,
			MemoryPercent: 50.0,
			NetworkRx:     1024 * 1024,
			NetworkTx:     512 * 1024,
			BlockRead:     100 * 1024 * 1024,
			BlockWrite:    50 * 1024 * 1024,
			PIDs:          15,
			Timestamp:     time.Now(),
		}
		assert.Equal(t, "abc123", metrics.ContainerID)
		assert.Equal(t, 45.5, metrics.CPUPercent)
	})
}

func TestContainerMetrics_MemoryUsagePercent(t *testing.T) {
	t.Run("calculates memory percent", func(t *testing.T) {
		metrics := &ContainerMetrics{
			MemoryUsage: 512 * 1024 * 1024,
			MemoryLimit: 1024 * 1024 * 1024,
		}
		assert.Equal(t, 50.0, metrics.CalculateMemoryPercent())
	})

	t.Run("handles zero limit", func(t *testing.T) {
		metrics := &ContainerMetrics{
			MemoryUsage: 512,
			MemoryLimit: 0,
		}
		assert.Equal(t, 0.0, metrics.CalculateMemoryPercent())
	})
}

func TestContainerHealth(t *testing.T) {
	t.Run("creates health status", func(t *testing.T) {
		health := &ContainerHealth{
			Status:        HealthStatusHealthy,
			FailingStreak: 0,
			Log: []HealthCheckLog{
				{
					Start:    time.Now(),
					End:      time.Now(),
					ExitCode: 0,
					Output:   "OK",
				},
			},
		}
		assert.Equal(t, HealthStatusHealthy, health.Status)
	})
}

func TestHealthStatus(t *testing.T) {
	t.Run("health statuses", func(t *testing.T) {
		assert.Equal(t, HealthStatus("healthy"), HealthStatusHealthy)
		assert.Equal(t, HealthStatus("unhealthy"), HealthStatusUnhealthy)
		assert.Equal(t, HealthStatus("starting"), HealthStatusStarting)
		assert.Equal(t, HealthStatus("none"), HealthStatusNone)
	})
}

func TestContainerState(t *testing.T) {
	t.Run("creates container state", func(t *testing.T) {
		state := &ContainerState{
			Status:     StateRunning,
			Running:    true,
			Paused:     false,
			Restarting: false,
			OOMKilled:  false,
			Dead:       false,
			Pid:        12345,
			ExitCode:   0,
			StartedAt:  time.Now(),
		}
		assert.Equal(t, StateRunning, state.Status)
		assert.True(t, state.Running)
	})
}

func TestContainerStateStatus(t *testing.T) {
	t.Run("state statuses", func(t *testing.T) {
		assert.Equal(t, ContainerStateStatus("running"), StateRunning)
		assert.Equal(t, ContainerStateStatus("exited"), StateExited)
		assert.Equal(t, ContainerStateStatus("paused"), StatePaused)
		assert.Equal(t, ContainerStateStatus("restarting"), StateRestarting)
		assert.Equal(t, ContainerStateStatus("created"), StateCreated)
	})
}

func TestNewMetricsCollector(t *testing.T) {
	t.Run("creates metrics collector", func(t *testing.T) {
		collector := NewMetricsCollector(&MetricsCollectorConfig{
			Provider: "mock",
			Interval: 10 * time.Second,
		})
		assert.NotNil(t, collector)
	})
}

func TestMetricsCollector_Collect(t *testing.T) {
	collector := NewMetricsCollector(&MetricsCollectorConfig{Provider: "mock"})

	t.Run("collects container metrics", func(t *testing.T) {
		ctx := context.Background()
		metrics, err := collector.Collect(ctx, "container-123")
		require.NoError(t, err)
		assert.NotNil(t, metrics)
		assert.Equal(t, "container-123", metrics.ContainerID)
	})
}

func TestMetricsCollector_CollectAll(t *testing.T) {
	collector := NewMetricsCollector(&MetricsCollectorConfig{Provider: "mock"})

	t.Run("collects all container metrics", func(t *testing.T) {
		ctx := context.Background()
		metrics, err := collector.CollectAll(ctx)
		require.NoError(t, err)
		assert.NotEmpty(t, metrics)
	})
}

func TestMetricsCollector_Stream(t *testing.T) {
	collector := NewMetricsCollector(&MetricsCollectorConfig{
		Provider: "mock",
		Interval: 50 * time.Millisecond,
	})

	t.Run("streams metrics", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		ch, err := collector.Stream(ctx, "container-123")
		require.NoError(t, err)

		select {
		case m := <-ch:
			assert.NotNil(t, m)
		case <-ctx.Done():
		}
	})
}

func TestResourceAlert(t *testing.T) {
	t.Run("creates resource alert", func(t *testing.T) {
		alert := &ResourceAlert{
			ContainerID: "abc123",
			Resource:    "memory",
			Threshold:   80.0,
			Current:     95.0,
			Message:     "Memory usage exceeded threshold",
			Severity:    AlertSeverityWarning,
			Timestamp:   time.Now(),
		}
		assert.Equal(t, "memory", alert.Resource)
		assert.Equal(t, AlertSeverityWarning, alert.Severity)
	})
}

func TestAlertSeverity(t *testing.T) {
	t.Run("alert severities", func(t *testing.T) {
		assert.True(t, AlertSeverityCritical > AlertSeverityWarning)
		assert.True(t, AlertSeverityWarning > AlertSeverityInfo)
	})
}

func TestResourceThreshold(t *testing.T) {
	t.Run("creates threshold", func(t *testing.T) {
		threshold := &ResourceThreshold{
			Resource: "cpu",
			Warning:  70.0,
			Critical: 90.0,
		}
		assert.Equal(t, 70.0, threshold.Warning)
		assert.Equal(t, 90.0, threshold.Critical)
	})

	t.Run("checks threshold", func(t *testing.T) {
		threshold := &ResourceThreshold{
			Resource: "cpu",
			Warning:  70.0,
			Critical: 90.0,
		}
		assert.Equal(t, AlertSeverityInfo, threshold.Check(50.0))
		assert.Equal(t, AlertSeverityWarning, threshold.Check(75.0))
		assert.Equal(t, AlertSeverityCritical, threshold.Check(95.0))
	})
}

func TestMonitoringConfig(t *testing.T) {
	t.Run("creates monitoring config", func(t *testing.T) {
		config := &MonitoringConfig{
			Interval: 15 * time.Second,
			Thresholds: []ResourceThreshold{
				{Resource: "cpu", Warning: 70, Critical: 90},
				{Resource: "memory", Warning: 80, Critical: 95},
			},
			EnableAlerts: true,
		}
		assert.Equal(t, 15*time.Second, config.Interval)
		assert.Len(t, config.Thresholds, 2)
	})
}

func TestContainerStats(t *testing.T) {
	t.Run("calculates stats summary", func(t *testing.T) {
		stats := &ContainerStats{
			Samples: []ContainerMetrics{
				{CPUPercent: 40, MemoryPercent: 50},
				{CPUPercent: 60, MemoryPercent: 60},
				{CPUPercent: 50, MemoryPercent: 55},
			},
		}
		assert.Equal(t, 50.0, stats.AvgCPU())
		assert.Equal(t, 55.0, stats.AvgMemory())
		assert.Equal(t, 60.0, stats.MaxCPU())
	})
}

func TestDefaultThresholds(t *testing.T) {
	t.Run("returns default thresholds", func(t *testing.T) {
		thresholds := DefaultThresholds()
		assert.NotEmpty(t, thresholds)

		var cpuThreshold *ResourceThreshold
		for _, th := range thresholds {
			if th.Resource == "cpu" {
				cpuThreshold = &th
				break
			}
		}
		require.NotNil(t, cpuThreshold)
		assert.Equal(t, 80.0, cpuThreshold.Warning)
	})
}
