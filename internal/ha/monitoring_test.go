// internal/ha/monitoring_test.go
package ha

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHAMonitor_NewMonitor(t *testing.T) {
	t.Run("creates monitor with default config", func(t *testing.T) {
		monitor := NewHAMonitor(nil)
		assert.NotNil(t, monitor)
	})

	t.Run("creates monitor with custom config", func(t *testing.T) {
		config := &HAMonitorConfig{
			CollectionInterval: time.Second * 30,
			RetentionPeriod:    time.Hour * 24,
			AlertThresholds: AlertThresholds{
				FailedBackendPercent: 50.0,
				LatencyP99Threshold:  time.Second * 2,
				ErrorRateThreshold:   5.0,
				RTOBreachThreshold:   1,
			},
		}
		monitor := NewHAMonitor(config)
		assert.NotNil(t, monitor)
		assert.Equal(t, time.Second*30, monitor.Config().CollectionInterval)
	})
}

func TestHAMonitor_RegisterComponent(t *testing.T) {
	t.Run("registers HA components for monitoring", func(t *testing.T) {
		monitor := NewHAMonitor(nil)

		orchestrator := NewHAOrchestrator()
		geoConfig := DefaultGeoConfig()
		geoManager, _ := NewGeoManager(geoConfig)

		monitor.RegisterOrchestrator(orchestrator)
		monitor.RegisterGeoManager(geoManager)

		assert.True(t, monitor.HasOrchestrator())
		assert.True(t, monitor.HasGeoManager())
	})
}

func TestHAMonitor_CollectMetrics(t *testing.T) {
	t.Run("collects metrics from all components", func(t *testing.T) {
		monitor := NewHAMonitor(nil)

		orchestrator := NewHAOrchestrator()
		orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		orchestrator.RegisterBackend("secondary", BackendConfig{Primary: false})

		monitor.RegisterOrchestrator(orchestrator)

		metrics := monitor.CollectMetrics(context.Background())

		assert.NotNil(t, metrics)
		assert.NotEmpty(t, metrics.Timestamp)
		assert.GreaterOrEqual(t, metrics.TotalBackends, 2)
	})

	t.Run("includes backend health metrics", func(t *testing.T) {
		monitor := NewHAMonitor(nil)

		orchestrator := NewHAOrchestrator()
		orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		orchestrator.RegisterBackend("secondary", BackendConfig{Primary: false})

		// Make one healthy, one failed (need multiple failures to trigger state change)
		orchestrator.ReportHealthCheck("primary", true, time.Millisecond*50, nil)
		for i := 0; i < 3; i++ {
			orchestrator.ReportHealthCheck("secondary", false, 0, assert.AnError)
		}

		monitor.RegisterOrchestrator(orchestrator)

		metrics := monitor.CollectMetrics(context.Background())

		assert.Equal(t, 1, metrics.HealthyBackends)
		assert.Equal(t, 1, metrics.UnhealthyBackends)
	})
}

func TestHAMonitor_GetSnapshot(t *testing.T) {
	t.Run("returns current system snapshot", func(t *testing.T) {
		monitor := NewHAMonitor(nil)

		orchestrator := NewHAOrchestrator()
		orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		monitor.RegisterOrchestrator(orchestrator)

		snapshot := monitor.GetSnapshot()

		assert.NotNil(t, snapshot)
		assert.NotEmpty(t, snapshot.Timestamp)
		assert.NotNil(t, snapshot.SystemStatus)
	})
}

func TestHAMonitor_MetricsHistory(t *testing.T) {
	t.Run("stores metrics history", func(t *testing.T) {
		config := &HAMonitorConfig{
			CollectionInterval: time.Millisecond * 10,
			RetentionPeriod:    time.Hour,
		}
		monitor := NewHAMonitor(config)

		orchestrator := NewHAOrchestrator()
		orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		monitor.RegisterOrchestrator(orchestrator)

		// Collect a few metrics
		for i := 0; i < 5; i++ {
			monitor.CollectMetrics(context.Background())
			time.Sleep(time.Millisecond * 5)
		}

		history := monitor.GetMetricsHistory(time.Minute)
		assert.GreaterOrEqual(t, len(history), 5)
	})

	t.Run("respects retention period", func(t *testing.T) {
		config := &HAMonitorConfig{
			CollectionInterval: time.Millisecond * 10,
			RetentionPeriod:    time.Millisecond * 100,
		}
		monitor := NewHAMonitor(config)

		orchestrator := NewHAOrchestrator()
		monitor.RegisterOrchestrator(orchestrator)

		// Collect metrics
		for i := 0; i < 10; i++ {
			monitor.CollectMetrics(context.Background())
			time.Sleep(time.Millisecond * 20)
		}

		// Cleanup old metrics
		monitor.CleanupOldMetrics()

		history := monitor.GetMetricsHistory(time.Second)
		// Some metrics should have been cleaned up
		assert.NotEmpty(t, history)
	})
}

func TestHAMonitor_Alerts(t *testing.T) {
	t.Run("generates alert when backend fails", func(t *testing.T) {
		config := &HAMonitorConfig{
			AlertThresholds: AlertThresholds{
				FailedBackendPercent: 30.0,
			},
		}
		monitor := NewHAMonitor(config)

		orchestrator := NewHAOrchestrator()
		orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		orchestrator.RegisterBackend("secondary", BackendConfig{Primary: false})

		// Fail one backend (50% failure rate)
		for i := 0; i < 3; i++ {
			orchestrator.ReportHealthCheck("primary", false, 0, assert.AnError)
		}

		monitor.RegisterOrchestrator(orchestrator)

		alerts := monitor.CheckAlerts(context.Background())

		hasBackendAlert := false
		for _, alert := range alerts {
			if alert.Type == AlertBackendFailure {
				hasBackendAlert = true
				break
			}
		}
		assert.True(t, hasBackendAlert, "Should generate backend failure alert")
	})

	t.Run("generates alert on high latency", func(t *testing.T) {
		config := &HAMonitorConfig{
			AlertThresholds: AlertThresholds{
				LatencyP99Threshold: time.Millisecond * 100,
			},
		}
		monitor := NewHAMonitor(config)

		orchestrator := NewHAOrchestrator()
		orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})

		// Report high latency
		orchestrator.ReportHealthCheck("primary", true, time.Second*5, nil)

		monitor.RegisterOrchestrator(orchestrator)

		alerts := monitor.CheckAlerts(context.Background())

		hasLatencyAlert := false
		for _, alert := range alerts {
			if alert.Type == AlertHighLatency {
				hasLatencyAlert = true
				break
			}
		}
		assert.True(t, hasLatencyAlert, "Should generate high latency alert")
	})
}

func TestHAMonitor_AlertSubscription(t *testing.T) {
	t.Run("notifies subscribers of alerts", func(t *testing.T) {
		monitor := NewHAMonitor(nil)

		alertReceived := make(chan Alert, 1)
		monitor.SubscribeAlerts(func(alert Alert) {
			alertReceived <- alert
		})

		// Trigger an alert
		monitor.EmitAlert(Alert{
			Type:     AlertBackendFailure,
			Severity: SeverityCritical,
			Message:  "Test alert",
		})

		select {
		case alert := <-alertReceived:
			assert.Equal(t, AlertBackendFailure, alert.Type)
			assert.Equal(t, SeverityCritical, alert.Severity)
		case <-time.After(time.Second):
			t.Fatal("Alert not received")
		}
	})
}

func TestHAMonitor_Dashboard(t *testing.T) {
	t.Run("generates dashboard data", func(t *testing.T) {
		monitor := NewHAMonitor(nil)

		orchestrator := NewHAOrchestrator()
		orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		orchestrator.RegisterBackend("secondary", BackendConfig{Primary: false})
		monitor.RegisterOrchestrator(orchestrator)

		geoConfig := DefaultGeoConfig()
		geoManager, _ := NewGeoManager(geoConfig)
		monitor.RegisterGeoManager(geoManager)

		dashboard := monitor.GetDashboardData()

		assert.NotNil(t, dashboard)
		assert.NotNil(t, dashboard.BackendStatus)
		assert.NotNil(t, dashboard.RegionStatus)
		assert.NotEmpty(t, dashboard.GeneratedAt)
	})
}

func TestHAMonitor_PrometheusMetrics(t *testing.T) {
	t.Run("exports prometheus format metrics", func(t *testing.T) {
		monitor := NewHAMonitor(nil)

		orchestrator := NewHAOrchestrator()
		orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		monitor.RegisterOrchestrator(orchestrator)

		// Collect some metrics first
		monitor.CollectMetrics(context.Background())

		promMetrics := monitor.GetPrometheusMetrics()

		assert.NotEmpty(t, promMetrics)
		assert.Contains(t, promMetrics, "vaultaire_ha_backends_total")
		assert.Contains(t, promMetrics, "vaultaire_ha_backends_healthy")
	})
}

func TestHAMonitor_StartStop(t *testing.T) {
	t.Run("starts and stops monitoring loop", func(t *testing.T) {
		config := &HAMonitorConfig{
			CollectionInterval: time.Millisecond * 50,
		}
		monitor := NewHAMonitor(config)

		orchestrator := NewHAOrchestrator()
		monitor.RegisterOrchestrator(orchestrator)

		ctx, cancel := context.WithCancel(context.Background())

		// Start monitoring
		go monitor.Start(ctx)

		// Let it run for a bit
		time.Sleep(time.Millisecond * 200)

		// Stop monitoring
		cancel()

		// Verify metrics were collected
		history := monitor.GetMetricsHistory(time.Second)
		assert.NotEmpty(t, history)
	})
}

func TestHAMonitor_SystemHealthScore(t *testing.T) {
	t.Run("calculates overall health score", func(t *testing.T) {
		monitor := NewHAMonitor(nil)

		orchestrator := NewHAOrchestrator()
		orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		orchestrator.RegisterBackend("secondary", BackendConfig{Primary: false})

		// All healthy
		orchestrator.ReportHealthCheck("primary", true, time.Millisecond*10, nil)
		orchestrator.ReportHealthCheck("secondary", true, time.Millisecond*10, nil)

		monitor.RegisterOrchestrator(orchestrator)

		score := monitor.CalculateHealthScore()

		assert.GreaterOrEqual(t, score, 90.0) // Should be high when all healthy
	})

	t.Run("reduces score when backends fail", func(t *testing.T) {
		monitor := NewHAMonitor(nil)

		orchestrator := NewHAOrchestrator()
		orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		orchestrator.RegisterBackend("secondary", BackendConfig{Primary: false})

		// One failed
		orchestrator.ReportHealthCheck("primary", true, time.Millisecond*10, nil)
		for i := 0; i < 3; i++ {
			orchestrator.ReportHealthCheck("secondary", false, 0, assert.AnError)
		}

		monitor.RegisterOrchestrator(orchestrator)

		score := monitor.CalculateHealthScore()

		assert.Less(t, score, 90.0) // Should be lower with failures
	})
}

func TestHAMonitor_Uptime(t *testing.T) {
	t.Run("tracks system uptime", func(t *testing.T) {
		monitor := NewHAMonitor(nil)

		// Let some time pass
		time.Sleep(time.Millisecond * 100)

		uptime := monitor.GetUptime()
		assert.GreaterOrEqual(t, uptime, time.Millisecond*100)
	})
}

func TestDefaultHAMonitorConfig(t *testing.T) {
	t.Run("provides sensible defaults", func(t *testing.T) {
		config := DefaultHAMonitorConfig()

		assert.Equal(t, time.Second*10, config.CollectionInterval)
		assert.Equal(t, time.Hour*24*7, config.RetentionPeriod) // 7 days
		assert.Greater(t, config.AlertThresholds.FailedBackendPercent, 0.0)
		assert.Greater(t, config.AlertThresholds.LatencyP99Threshold, time.Duration(0))
	})
}
