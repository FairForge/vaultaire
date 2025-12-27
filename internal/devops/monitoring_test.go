// internal/devops/monitoring_test.go
package devops

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewMonitoringManager(t *testing.T) {
	t.Run("creates with defaults", func(t *testing.T) {
		manager := NewMonitoringManager(nil)
		assert.NotNil(t, manager)
		assert.True(t, manager.config.Enabled)
		assert.Equal(t, 9090, manager.config.MetricsPort)
	})

	t.Run("creates with custom config", func(t *testing.T) {
		config := &MonitoringConfig{MetricsPort: 8080}
		manager := NewMonitoringManager(config)
		assert.Equal(t, 8080, manager.config.MetricsPort)
	})
}

func TestMonitoringManager_Metrics(t *testing.T) {
	manager := NewMonitoringManager(nil)

	t.Run("registers metric", func(t *testing.T) {
		err := manager.RegisterMetric("test_counter", MetricTypeCounter, "Test counter")
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		err := manager.RegisterMetric("", MetricTypeCounter, "")
		assert.Error(t, err)
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		_ = manager.RegisterMetric("dup_metric", MetricTypeCounter, "")
		err := manager.RegisterMetric("dup_metric", MetricTypeCounter, "")
		assert.Error(t, err)
	})

	t.Run("records metric value", func(t *testing.T) {
		_ = manager.RegisterMetric("value_metric", MetricTypeGauge, "")
		err := manager.RecordMetric("value_metric", 42.5, nil)
		assert.NoError(t, err)

		metric := manager.GetMetric("value_metric")
		assert.Equal(t, 42.5, metric.Value)
	})

	t.Run("records metric with labels", func(t *testing.T) {
		_ = manager.RegisterMetric("labeled_metric", MetricTypeGauge, "")
		labels := map[string]string{"env": "test"}
		err := manager.RecordMetric("labeled_metric", 10, labels)
		assert.NoError(t, err)

		metric := manager.GetMetric("labeled_metric")
		assert.Equal(t, "test", metric.Labels["env"])
	})

	t.Run("errors for unknown metric", func(t *testing.T) {
		err := manager.RecordMetric("unknown", 1, nil)
		assert.Error(t, err)
	})
}

func TestMonitoringManager_Counter(t *testing.T) {
	manager := NewMonitoringManager(nil)
	_ = manager.RegisterMetric("requests", MetricTypeCounter, "")

	t.Run("increments counter", func(t *testing.T) {
		_ = manager.IncrementCounter("requests", 1)
		_ = manager.IncrementCounter("requests", 1)
		_ = manager.IncrementCounter("requests", 1)

		metric := manager.GetMetric("requests")
		assert.Equal(t, float64(3), metric.Value)
	})

	t.Run("errors for non-counter", func(t *testing.T) {
		_ = manager.RegisterMetric("gauge", MetricTypeGauge, "")
		err := manager.IncrementCounter("gauge", 1)
		assert.Error(t, err)
	})
}

func TestMonitoringManager_ListMetrics(t *testing.T) {
	manager := NewMonitoringManager(nil)
	_ = manager.RegisterMetric("metric1", MetricTypeCounter, "")
	_ = manager.RegisterMetric("metric2", MetricTypeGauge, "")

	metrics := manager.ListMetrics()
	assert.Len(t, metrics, 2)
}

func TestMonitoringManager_HealthChecks(t *testing.T) {
	manager := NewMonitoringManager(nil)

	t.Run("registers health check", func(t *testing.T) {
		err := manager.RegisterHealthCheck(&HealthCheck{
			Name:     "database",
			Endpoint: "/health/db",
		})
		assert.NoError(t, err)
	})

	t.Run("sets defaults", func(t *testing.T) {
		check := manager.GetHealthCheck("database")
		require.NotNil(t, check)
		assert.Equal(t, 30*time.Second, check.Interval)
		assert.Equal(t, 10*time.Second, check.Timeout)
		assert.Equal(t, 3, check.Threshold)
		assert.True(t, check.Enabled)
	})

	t.Run("rejects nil check", func(t *testing.T) {
		err := manager.RegisterHealthCheck(nil)
		assert.Error(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		err := manager.RegisterHealthCheck(&HealthCheck{})
		assert.Error(t, err)
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		_ = manager.RegisterHealthCheck(&HealthCheck{Name: "dup"})
		err := manager.RegisterHealthCheck(&HealthCheck{Name: "dup"})
		assert.Error(t, err)
	})
}

func TestMonitoringManager_UpdateHealthCheckStatus(t *testing.T) {
	manager := NewMonitoringManager(nil)
	_ = manager.RegisterHealthCheck(&HealthCheck{Name: "test", Threshold: 3})

	t.Run("updates to healthy", func(t *testing.T) {
		err := manager.UpdateHealthCheckStatus("test", HealthStatusHealthy, 10*time.Millisecond)
		assert.NoError(t, err)

		check := manager.GetHealthCheck("test")
		assert.Equal(t, HealthStatusHealthy, check.LastStatus)
		assert.Equal(t, 0, check.FailCount)
	})

	t.Run("tracks failures", func(t *testing.T) {
		_ = manager.UpdateHealthCheckStatus("test", HealthStatusUnhealthy, 100*time.Millisecond)
		check := manager.GetHealthCheck("test")
		assert.Equal(t, 1, check.FailCount)
		assert.Equal(t, HealthStatusDegraded, check.LastStatus)
	})

	t.Run("becomes unhealthy after threshold", func(t *testing.T) {
		_ = manager.UpdateHealthCheckStatus("test", HealthStatusUnhealthy, 100*time.Millisecond)
		_ = manager.UpdateHealthCheckStatus("test", HealthStatusUnhealthy, 100*time.Millisecond)

		check := manager.GetHealthCheck("test")
		assert.Equal(t, HealthStatusUnhealthy, check.LastStatus)
		assert.Equal(t, 3, check.FailCount)
	})

	t.Run("resets on success", func(t *testing.T) {
		_ = manager.UpdateHealthCheckStatus("test", HealthStatusHealthy, 10*time.Millisecond)

		check := manager.GetHealthCheck("test")
		assert.Equal(t, HealthStatusHealthy, check.LastStatus)
		assert.Equal(t, 0, check.FailCount)
	})

	t.Run("errors for unknown check", func(t *testing.T) {
		err := manager.UpdateHealthCheckStatus("unknown", HealthStatusHealthy, 0)
		assert.Error(t, err)
	})
}

func TestMonitoringManager_Services(t *testing.T) {
	manager := NewMonitoringManager(nil)

	t.Run("registers service", func(t *testing.T) {
		err := manager.RegisterService("api")
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		err := manager.RegisterService("")
		assert.Error(t, err)
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		_ = manager.RegisterService("dup")
		err := manager.RegisterService("dup")
		assert.Error(t, err)
	})

	t.Run("updates service health", func(t *testing.T) {
		_ = manager.RegisterService("database")
		err := manager.UpdateServiceHealth("database", HealthStatusHealthy, "connected", 5*time.Millisecond)
		assert.NoError(t, err)

		health := manager.GetServiceHealth("database")
		assert.Equal(t, HealthStatusHealthy, health.Status)
		assert.Equal(t, "connected", health.Message)
	})

	t.Run("errors for unknown service", func(t *testing.T) {
		err := manager.UpdateServiceHealth("unknown", HealthStatusHealthy, "", 0)
		assert.Error(t, err)
	})
}

func TestMonitoringManager_GetOverallHealth(t *testing.T) {
	t.Run("returns unknown when empty", func(t *testing.T) {
		manager := NewMonitoringManager(nil)
		assert.Equal(t, HealthStatusUnknown, manager.GetOverallHealth())
	})

	t.Run("returns healthy when all healthy", func(t *testing.T) {
		manager := NewMonitoringManager(nil)
		_ = manager.RegisterService("api")
		_ = manager.UpdateServiceHealth("api", HealthStatusHealthy, "", 0)

		assert.Equal(t, HealthStatusHealthy, manager.GetOverallHealth())
	})

	t.Run("returns degraded when one degraded", func(t *testing.T) {
		manager := NewMonitoringManager(nil)
		_ = manager.RegisterService("api")
		_ = manager.RegisterService("db")
		_ = manager.UpdateServiceHealth("api", HealthStatusHealthy, "", 0)
		_ = manager.UpdateServiceHealth("db", HealthStatusDegraded, "", 0)

		assert.Equal(t, HealthStatusDegraded, manager.GetOverallHealth())
	})

	t.Run("returns unhealthy when one unhealthy", func(t *testing.T) {
		manager := NewMonitoringManager(nil)
		_ = manager.RegisterService("api")
		_ = manager.RegisterService("db")
		_ = manager.UpdateServiceHealth("api", HealthStatusHealthy, "", 0)
		_ = manager.UpdateServiceHealth("db", HealthStatusUnhealthy, "", 0)

		assert.Equal(t, HealthStatusUnhealthy, manager.GetOverallHealth())
	})
}

func TestMonitoringManager_GetHealthReport(t *testing.T) {
	manager := NewMonitoringManager(nil)
	_ = manager.RegisterService("api")
	_ = manager.UpdateServiceHealth("api", HealthStatusHealthy, "ok", 5*time.Millisecond)
	_ = manager.RegisterHealthCheck(&HealthCheck{Name: "db"})
	_ = manager.UpdateHealthCheckStatus("db", HealthStatusHealthy, 10*time.Millisecond)

	report := manager.GetHealthReport()

	assert.Equal(t, HealthStatusHealthy, report["status"])
	assert.NotNil(t, report["services"])
	assert.NotNil(t, report["health_checks"])
}

func TestMonitoringManager_SetupProductionMonitoring(t *testing.T) {
	manager := NewMonitoringManager(nil)

	err := manager.SetupProductionMonitoring()
	require.NoError(t, err)

	t.Run("registers standard metrics", func(t *testing.T) {
		metrics := manager.ListMetrics()
		assert.GreaterOrEqual(t, len(metrics), 10)

		// Check specific metrics
		assert.NotNil(t, manager.GetMetric("http_requests_total"))
		assert.NotNil(t, manager.GetMetric("storage_bytes_total"))
	})

	t.Run("registers services", func(t *testing.T) {
		assert.NotNil(t, manager.GetServiceHealth("api"))
		assert.NotNil(t, manager.GetServiceHealth("database"))
	})

	t.Run("registers health checks", func(t *testing.T) {
		checks := manager.ListHealthChecks()
		assert.GreaterOrEqual(t, len(checks), 3)
	})
}

func TestMonitoringManager_GeneratePrometheusConfig(t *testing.T) {
	manager := NewMonitoringManager(&MonitoringConfig{
		ScrapeInterval: 15 * time.Second,
		MetricsPath:    "/metrics",
	})

	config := manager.GeneratePrometheusConfig([]string{"localhost:9090", "localhost:9091"})

	assert.Contains(t, config, "scrape_interval: 15s")
	assert.Contains(t, config, "'localhost:9090'")
	assert.Contains(t, config, "'localhost:9091'")
	assert.Contains(t, config, "metrics_path: '/metrics'")
}

func TestMetricTypeConstants(t *testing.T) {
	assert.Equal(t, MetricType("counter"), MetricTypeCounter)
	assert.Equal(t, MetricType("gauge"), MetricTypeGauge)
	assert.Equal(t, MetricType("histogram"), MetricTypeHistogram)
	assert.Equal(t, MetricType("summary"), MetricTypeSummary)
}

func TestHealthStatusConstants(t *testing.T) {
	assert.Equal(t, HealthStatus("healthy"), HealthStatusHealthy)
	assert.Equal(t, HealthStatus("degraded"), HealthStatusDegraded)
	assert.Equal(t, HealthStatus("unhealthy"), HealthStatusUnhealthy)
	assert.Equal(t, HealthStatus("unknown"), HealthStatusUnknown)
}

func TestDefaultMonitoringConfigs(t *testing.T) {
	t.Run("production disables pprof", func(t *testing.T) {
		config := DefaultMonitoringConfigs[EnvTypeProduction]
		assert.False(t, config.EnablePprof)
		assert.True(t, config.EnableTracing)
		assert.Equal(t, 0.1, config.TracingSampleRate)
	})

	t.Run("development enables pprof", func(t *testing.T) {
		config := DefaultMonitoringConfigs[EnvTypeDevelopment]
		assert.True(t, config.EnablePprof)
		assert.False(t, config.EnableTracing)
	})
}

func TestGetMonitoringConfigForEnvironment(t *testing.T) {
	t.Run("returns production config", func(t *testing.T) {
		config := GetMonitoringConfigForEnvironment(EnvTypeProduction)
		assert.False(t, config.EnablePprof)
	})

	t.Run("returns development for unknown", func(t *testing.T) {
		config := GetMonitoringConfigForEnvironment("unknown")
		assert.True(t, config.EnablePprof)
	})
}
