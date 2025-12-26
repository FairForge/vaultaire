// internal/devops/monitoring.go
package devops

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// MetricType represents the type of metric
type MetricType string

const (
	MetricTypeCounter   MetricType = "counter"
	MetricTypeGauge     MetricType = "gauge"
	MetricTypeHistogram MetricType = "histogram"
	MetricTypeSummary   MetricType = "summary"
)

// HealthStatus represents service health
type HealthStatus string

const (
	HealthStatusHealthy   HealthStatus = "healthy"
	HealthStatusDegraded  HealthStatus = "degraded"
	HealthStatusUnhealthy HealthStatus = "unhealthy"
	HealthStatusUnknown   HealthStatus = "unknown"
)

// Metric represents a monitoring metric
type Metric struct {
	Name        string            `json:"name"`
	Type        MetricType        `json:"type"`
	Description string            `json:"description"`
	Value       float64           `json:"value"`
	Labels      map[string]string `json:"labels,omitempty"`
	Timestamp   time.Time         `json:"timestamp"`
}

// HealthCheck represents a health check configuration
type HealthCheck struct {
	Name        string        `json:"name"`
	Endpoint    string        `json:"endpoint"`
	Interval    time.Duration `json:"interval"`
	Timeout     time.Duration `json:"timeout"`
	Enabled     bool          `json:"enabled"`
	LastCheck   *time.Time    `json:"last_check,omitempty"`
	LastStatus  HealthStatus  `json:"last_status"`
	LastLatency time.Duration `json:"last_latency"`
	FailCount   int           `json:"fail_count"`
	Threshold   int           `json:"threshold"` // Failures before unhealthy
}

// ServiceHealth represents a service's health status
type ServiceHealth struct {
	Name      string            `json:"name"`
	Status    HealthStatus      `json:"status"`
	Message   string            `json:"message,omitempty"`
	CheckedAt time.Time         `json:"checked_at"`
	Latency   time.Duration     `json:"latency"`
	Details   map[string]string `json:"details,omitempty"`
}

// MonitoringConfig configures monitoring
type MonitoringConfig struct {
	Enabled           bool          `json:"enabled"`
	MetricsPort       int           `json:"metrics_port"`
	MetricsPath       string        `json:"metrics_path"`
	HealthPath        string        `json:"health_path"`
	ReadinessPath     string        `json:"readiness_path"`
	LivenessPath      string        `json:"liveness_path"`
	ScrapeInterval    time.Duration `json:"scrape_interval"`
	RetentionPeriod   time.Duration `json:"retention_period"`
	EnablePprof       bool          `json:"enable_pprof"`
	EnableTracing     bool          `json:"enable_tracing"`
	TracingSampleRate float64       `json:"tracing_sample_rate"`
}

// DefaultMonitoringConfigs returns environment-specific configurations
var DefaultMonitoringConfigs = map[string]*MonitoringConfig{
	EnvTypeDevelopment: {
		Enabled:           true,
		MetricsPort:       9090,
		MetricsPath:       "/metrics",
		HealthPath:        "/health",
		ReadinessPath:     "/ready",
		LivenessPath:      "/live",
		ScrapeInterval:    15 * time.Second,
		RetentionPeriod:   1 * time.Hour,
		EnablePprof:       true,
		EnableTracing:     false,
		TracingSampleRate: 1.0,
	},
	EnvTypeStaging: {
		Enabled:           true,
		MetricsPort:       9090,
		MetricsPath:       "/metrics",
		HealthPath:        "/health",
		ReadinessPath:     "/ready",
		LivenessPath:      "/live",
		ScrapeInterval:    15 * time.Second,
		RetentionPeriod:   24 * time.Hour,
		EnablePprof:       true,
		EnableTracing:     true,
		TracingSampleRate: 1.0,
	},
	EnvTypeProduction: {
		Enabled:           true,
		MetricsPort:       9090,
		MetricsPath:       "/metrics",
		HealthPath:        "/health",
		ReadinessPath:     "/ready",
		LivenessPath:      "/live",
		ScrapeInterval:    15 * time.Second,
		RetentionPeriod:   7 * 24 * time.Hour,
		EnablePprof:       false,
		EnableTracing:     true,
		TracingSampleRate: 0.1,
	},
}

// MonitoringManager manages monitoring and health checks
type MonitoringManager struct {
	config       *MonitoringConfig
	metrics      map[string]*Metric
	healthChecks map[string]*HealthCheck
	services     map[string]*ServiceHealth
	mu           sync.RWMutex
}

// NewMonitoringManager creates a monitoring manager
func NewMonitoringManager(config *MonitoringConfig) *MonitoringManager {
	if config == nil {
		config = DefaultMonitoringConfigs[EnvTypeDevelopment]
	}
	return &MonitoringManager{
		config:       config,
		metrics:      make(map[string]*Metric),
		healthChecks: make(map[string]*HealthCheck),
		services:     make(map[string]*ServiceHealth),
	}
}

// GetConfig returns the configuration
func (m *MonitoringManager) GetConfig() *MonitoringConfig {
	return m.config
}

// IsEnabled returns whether monitoring is enabled
func (m *MonitoringManager) IsEnabled() bool {
	return m.config.Enabled
}

// RegisterMetric registers a new metric
func (m *MonitoringManager) RegisterMetric(name string, metricType MetricType, description string) error {
	if name == "" {
		return errors.New("monitoring: metric name is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.metrics[name]; exists {
		return fmt.Errorf("monitoring: metric %s already registered", name)
	}

	m.metrics[name] = &Metric{
		Name:        name,
		Type:        metricType,
		Description: description,
		Labels:      make(map[string]string),
		Timestamp:   time.Now(),
	}

	return nil
}

// RecordMetric records a metric value
func (m *MonitoringManager) RecordMetric(name string, value float64, labels map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	metric, exists := m.metrics[name]
	if !exists {
		return fmt.Errorf("monitoring: metric %s not registered", name)
	}

	metric.Value = value
	metric.Timestamp = time.Now()
	if labels != nil {
		metric.Labels = labels
	}

	return nil
}

// IncrementCounter increments a counter metric
func (m *MonitoringManager) IncrementCounter(name string, delta float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	metric, exists := m.metrics[name]
	if !exists {
		return fmt.Errorf("monitoring: metric %s not registered", name)
	}

	if metric.Type != MetricTypeCounter {
		return fmt.Errorf("monitoring: %s is not a counter", name)
	}

	metric.Value += delta
	metric.Timestamp = time.Now()

	return nil
}

// GetMetric returns a metric by name
func (m *MonitoringManager) GetMetric(name string) *Metric {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if metric, exists := m.metrics[name]; exists {
		copy := *metric
		return &copy
	}
	return nil
}

// ListMetrics returns all metrics
func (m *MonitoringManager) ListMetrics() []*Metric {
	m.mu.RLock()
	defer m.mu.RUnlock()

	metrics := make([]*Metric, 0, len(m.metrics))
	for _, metric := range m.metrics {
		copy := *metric
		metrics = append(metrics, &copy)
	}
	return metrics
}

// RegisterHealthCheck registers a health check
func (m *MonitoringManager) RegisterHealthCheck(check *HealthCheck) error {
	if check == nil {
		return errors.New("monitoring: health check is required")
	}
	if check.Name == "" {
		return errors.New("monitoring: health check name is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.healthChecks[check.Name]; exists {
		return fmt.Errorf("monitoring: health check %s already registered", check.Name)
	}

	if check.Interval == 0 {
		check.Interval = 30 * time.Second
	}
	if check.Timeout == 0 {
		check.Timeout = 10 * time.Second
	}
	if check.Threshold == 0 {
		check.Threshold = 3
	}
	check.Enabled = true
	check.LastStatus = HealthStatusUnknown

	m.healthChecks[check.Name] = check
	return nil
}

// GetHealthCheck returns a health check by name
func (m *MonitoringManager) GetHealthCheck(name string) *HealthCheck {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.healthChecks[name]
}

// ListHealthChecks returns all health checks
func (m *MonitoringManager) ListHealthChecks() []*HealthCheck {
	m.mu.RLock()
	defer m.mu.RUnlock()

	checks := make([]*HealthCheck, 0, len(m.healthChecks))
	for _, check := range m.healthChecks {
		checks = append(checks, check)
	}
	return checks
}

// UpdateHealthCheckStatus updates a health check's status
func (m *MonitoringManager) UpdateHealthCheckStatus(name string, status HealthStatus, latency time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	check, exists := m.healthChecks[name]
	if !exists {
		return fmt.Errorf("monitoring: health check %s not found", name)
	}

	now := time.Now()
	check.LastCheck = &now
	check.LastLatency = latency

	if status == HealthStatusHealthy {
		check.FailCount = 0
	} else {
		check.FailCount++
	}

	if check.FailCount >= check.Threshold {
		check.LastStatus = HealthStatusUnhealthy
	} else if check.FailCount > 0 {
		check.LastStatus = HealthStatusDegraded
	} else {
		check.LastStatus = status
	}

	return nil
}

// RegisterService registers a service for health monitoring
func (m *MonitoringManager) RegisterService(name string) error {
	if name == "" {
		return errors.New("monitoring: service name is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.services[name]; exists {
		return fmt.Errorf("monitoring: service %s already registered", name)
	}

	m.services[name] = &ServiceHealth{
		Name:      name,
		Status:    HealthStatusUnknown,
		CheckedAt: time.Now(),
		Details:   make(map[string]string),
	}

	return nil
}

// UpdateServiceHealth updates a service's health status
func (m *MonitoringManager) UpdateServiceHealth(name string, status HealthStatus, message string, latency time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	service, exists := m.services[name]
	if !exists {
		return fmt.Errorf("monitoring: service %s not registered", name)
	}

	service.Status = status
	service.Message = message
	service.CheckedAt = time.Now()
	service.Latency = latency

	return nil
}

// GetServiceHealth returns a service's health
func (m *MonitoringManager) GetServiceHealth(name string) *ServiceHealth {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if service, exists := m.services[name]; exists {
		copy := *service
		return &copy
	}
	return nil
}

// GetOverallHealth returns the overall system health
func (m *MonitoringManager) GetOverallHealth() HealthStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.services) == 0 && len(m.healthChecks) == 0 {
		return HealthStatusUnknown
	}

	hasUnhealthy := false
	hasDegraded := false

	for _, service := range m.services {
		switch service.Status {
		case HealthStatusUnhealthy:
			hasUnhealthy = true
		case HealthStatusDegraded:
			hasDegraded = true
		}
	}

	for _, check := range m.healthChecks {
		switch check.LastStatus {
		case HealthStatusUnhealthy:
			hasUnhealthy = true
		case HealthStatusDegraded:
			hasDegraded = true
		}
	}

	if hasUnhealthy {
		return HealthStatusUnhealthy
	}
	if hasDegraded {
		return HealthStatusDegraded
	}

	return HealthStatusHealthy
}

// GetHealthReport returns a detailed health report
func (m *MonitoringManager) GetHealthReport() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	services := make(map[string]interface{})
	for name, service := range m.services {
		services[name] = map[string]interface{}{
			"status":     service.Status,
			"message":    service.Message,
			"checked_at": service.CheckedAt,
			"latency_ms": service.Latency.Milliseconds(),
		}
	}

	checks := make(map[string]interface{})
	for name, check := range m.healthChecks {
		checks[name] = map[string]interface{}{
			"status":     check.LastStatus,
			"fail_count": check.FailCount,
			"last_check": check.LastCheck,
			"latency_ms": check.LastLatency.Milliseconds(),
		}
	}

	return map[string]interface{}{
		"status":        m.GetOverallHealth(),
		"timestamp":     time.Now(),
		"services":      services,
		"health_checks": checks,
	}
}

// SetupProductionMonitoring sets up standard production monitoring
func (m *MonitoringManager) SetupProductionMonitoring() error {
	// Register standard metrics
	metrics := []struct {
		name        string
		metricType  MetricType
		description string
	}{
		{"http_requests_total", MetricTypeCounter, "Total HTTP requests"},
		{"http_request_duration_seconds", MetricTypeHistogram, "HTTP request duration"},
		{"http_requests_in_flight", MetricTypeGauge, "Current in-flight requests"},
		{"storage_operations_total", MetricTypeCounter, "Total storage operations"},
		{"storage_bytes_transferred", MetricTypeCounter, "Bytes transferred"},
		{"storage_objects_total", MetricTypeGauge, "Total objects stored"},
		{"storage_bytes_total", MetricTypeGauge, "Total bytes stored"},
		{"backend_health", MetricTypeGauge, "Backend health status"},
		{"database_connections", MetricTypeGauge, "Database connection pool size"},
		{"cache_hits_total", MetricTypeCounter, "Cache hits"},
		{"cache_misses_total", MetricTypeCounter, "Cache misses"},
		{"errors_total", MetricTypeCounter, "Total errors"},
	}

	for _, metric := range metrics {
		if err := m.RegisterMetric(metric.name, metric.metricType, metric.description); err != nil {
			return fmt.Errorf("monitoring: failed to register %s: %w", metric.name, err)
		}
	}

	// Register services
	services := []string{
		"api",
		"database",
		"cache",
		"storage_primary",
		"storage_secondary",
	}

	for _, service := range services {
		if err := m.RegisterService(service); err != nil {
			return fmt.Errorf("monitoring: failed to register service %s: %w", service, err)
		}
	}

	// Register health checks
	checks := []*HealthCheck{
		{Name: "database", Endpoint: "/health/db", Interval: 30 * time.Second, Timeout: 5 * time.Second},
		{Name: "cache", Endpoint: "/health/cache", Interval: 30 * time.Second, Timeout: 5 * time.Second},
		{Name: "storage", Endpoint: "/health/storage", Interval: 60 * time.Second, Timeout: 10 * time.Second},
	}

	for _, check := range checks {
		if err := m.RegisterHealthCheck(check); err != nil {
			return fmt.Errorf("monitoring: failed to register check %s: %w", check.Name, err)
		}
	}

	return nil
}

// GeneratePrometheusConfig generates Prometheus scrape config
func (m *MonitoringManager) GeneratePrometheusConfig(targets []string) string {
	config := `global:
  scrape_interval: %s
  evaluation_interval: %s

scrape_configs:
  - job_name: 'vaultaire'
    static_configs:
      - targets: [%s]
    metrics_path: '%s'
`

	targetStr := ""
	for i, t := range targets {
		if i > 0 {
			targetStr += ", "
		}
		targetStr += fmt.Sprintf("'%s'", t)
	}

	return fmt.Sprintf(config,
		m.config.ScrapeInterval,
		m.config.ScrapeInterval,
		targetStr,
		m.config.MetricsPath,
	)
}

// GetMonitoringConfigForEnvironment returns config for an environment
func GetMonitoringConfigForEnvironment(envType string) *MonitoringConfig {
	if config, ok := DefaultMonitoringConfigs[envType]; ok {
		return config
	}
	return DefaultMonitoringConfigs[EnvTypeDevelopment]
}
