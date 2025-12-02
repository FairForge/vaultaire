// internal/ha/monitoring.go
package ha

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// AlertType represents the type of alert
type AlertType string

const (
	AlertBackendFailure  AlertType = "backend_failure"
	AlertHighLatency     AlertType = "high_latency"
	AlertHighErrorRate   AlertType = "high_error_rate"
	AlertRTOBreach       AlertType = "rto_breach"
	AlertRegionFailure   AlertType = "region_failure"
	AlertCapacityWarning AlertType = "capacity_warning"
)

// Severity represents alert severity levels
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Alert represents a monitoring alert
type Alert struct {
	Type      AlertType
	Severity  Severity
	Message   string
	Timestamp time.Time
	Backend   string
	Details   map[string]interface{}
}

// AlertThresholds defines thresholds for alerting
type AlertThresholds struct {
	FailedBackendPercent float64
	LatencyP99Threshold  time.Duration
	ErrorRateThreshold   float64
	RTOBreachThreshold   int
}

// HAMonitorConfig configures the HA monitor
type HAMonitorConfig struct {
	CollectionInterval time.Duration
	RetentionPeriod    time.Duration
	AlertThresholds    AlertThresholds
}

// DefaultHAMonitorConfig returns sensible defaults
func DefaultHAMonitorConfig() *HAMonitorConfig {
	return &HAMonitorConfig{
		CollectionInterval: time.Second * 10,
		RetentionPeriod:    time.Hour * 24 * 7, // 7 days
		AlertThresholds: AlertThresholds{
			FailedBackendPercent: 30.0,
			LatencyP99Threshold:  time.Second * 2,
			ErrorRateThreshold:   5.0,
			RTOBreachThreshold:   1,
		},
	}
}

// HAMetrics represents collected metrics at a point in time
type HAMetrics struct {
	Timestamp         time.Time
	TotalBackends     int
	HealthyBackends   int
	UnhealthyBackends int
	DegradedBackends  int
	AverageLatency    time.Duration
	MaxLatency        time.Duration
	ErrorRate         float64
	SystemStatus      string
	RegionMetrics     map[string]RegionMetrics
}

// RegionMetrics represents metrics for a geographic region
type RegionMetrics struct {
	Name      string
	Healthy   bool
	Latency   time.Duration
	Backends  int
	LoadScore float64
}

// SystemSnapshot represents current system state
type SystemSnapshot struct {
	Timestamp      time.Time
	SystemStatus   string
	HealthScore    float64
	ActiveBackends []string
	FailedBackends []string
	ActiveAlerts   []Alert
	Uptime         time.Duration
}

// DashboardData contains data for HA dashboard
type DashboardData struct {
	GeneratedAt    time.Time
	BackendStatus  map[string]BackendDashboardStatus
	RegionStatus   map[string]RegionDashboardStatus
	RecentAlerts   []Alert
	MetricsSummary MetricsSummary
	HealthScore    float64
}

// BackendDashboardStatus represents backend status for dashboard
type BackendDashboardStatus struct {
	Name      string
	State     string
	Latency   time.Duration
	IsPrimary bool
	LastCheck time.Time
}

// RegionDashboardStatus represents region status for dashboard
type RegionDashboardStatus struct {
	Name     string
	Healthy  bool
	Backends int
	Latency  time.Duration
}

// MetricsSummary contains summarized metrics
type MetricsSummary struct {
	AvgLatency24h   time.Duration
	Uptime24h       float64
	TotalFailovers  int
	AlertsTriggered int
	HealthScoreAvg  float64
}

// HAMonitor monitors the HA system
type HAMonitor struct {
	config       *HAMonitorConfig
	orchestrator *HAOrchestrator
	geoManager   *GeoManager
	rtoTracker   *RTORPOTracker

	metrics       []HAMetrics
	alerts        []Alert
	alertHandlers []func(Alert)

	startTime time.Time
	mu        sync.RWMutex
}

// NewHAMonitor creates a new HA monitor
func NewHAMonitor(config *HAMonitorConfig) *HAMonitor {
	if config == nil {
		config = DefaultHAMonitorConfig()
	}

	return &HAMonitor{
		config:        config,
		metrics:       make([]HAMetrics, 0),
		alerts:        make([]Alert, 0),
		alertHandlers: make([]func(Alert), 0),
		startTime:     time.Now(),
	}
}

// Config returns the monitor configuration
func (m *HAMonitor) Config() *HAMonitorConfig {
	return m.config
}

// RegisterOrchestrator registers the HA orchestrator for monitoring
func (m *HAMonitor) RegisterOrchestrator(orchestrator *HAOrchestrator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.orchestrator = orchestrator
}

// RegisterGeoManager registers the geo manager for monitoring
func (m *HAMonitor) RegisterGeoManager(geoManager *GeoManager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.geoManager = geoManager
}

// RegisterRTOTracker registers the RTO/RPO tracker for monitoring
func (m *HAMonitor) RegisterRTOTracker(tracker *RTORPOTracker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rtoTracker = tracker
}

// HasOrchestrator returns true if orchestrator is registered
func (m *HAMonitor) HasOrchestrator() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.orchestrator != nil
}

// HasGeoManager returns true if geo manager is registered
func (m *HAMonitor) HasGeoManager() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.geoManager != nil
}

// CollectMetrics collects metrics from all registered components
func (m *HAMonitor) CollectMetrics(ctx context.Context) HAMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()

	metrics := HAMetrics{
		Timestamp:     time.Now(),
		RegionMetrics: make(map[string]RegionMetrics),
	}

	// Collect from orchestrator
	if m.orchestrator != nil {
		healthyBackends := m.orchestrator.GetHealthyBackends()
		status := m.orchestrator.GetSystemStatus()

		metrics.HealthyBackends = len(healthyBackends)
		metrics.SystemStatus = status.String()

		// Count total and categorize backends
		m.orchestrator.mu.RLock()
		metrics.TotalBackends = len(m.orchestrator.backends)
		var totalLatency time.Duration
		latencyCount := 0

		for _, backend := range m.orchestrator.backends {
			switch backend.State {
			case StateFailed:
				metrics.UnhealthyBackends++
			case StateDegraded:
				metrics.DegradedBackends++
			}

			if backend.Latency > 0 {
				totalLatency += backend.Latency
				latencyCount++
				if backend.Latency > metrics.MaxLatency {
					metrics.MaxLatency = backend.Latency
				}
			}
		}
		m.orchestrator.mu.RUnlock()

		if latencyCount > 0 {
			metrics.AverageLatency = totalLatency / time.Duration(latencyCount)
		}
	}

	// Collect from geo manager
	if m.geoManager != nil {
		regionStatus := m.geoManager.GetStatus()
		for region, status := range regionStatus {
			metrics.RegionMetrics[string(region)] = RegionMetrics{
				Name:    string(region),
				Healthy: status.Health == StateHealthy,
				Latency: status.Latency,
			}
		}
	}

	// Store metrics
	m.metrics = append(m.metrics, metrics)

	return metrics
}

// GetSnapshot returns current system snapshot
func (m *HAMonitor) GetSnapshot() SystemSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	snapshot := SystemSnapshot{
		Timestamp:      time.Now(),
		ActiveBackends: make([]string, 0),
		FailedBackends: make([]string, 0),
		ActiveAlerts:   make([]Alert, 0),
		Uptime:         time.Since(m.startTime),
	}

	if m.orchestrator != nil {
		snapshot.ActiveBackends = m.orchestrator.GetHealthyBackends()
		snapshot.SystemStatus = m.orchestrator.GetSystemStatus().String()

		m.orchestrator.mu.RLock()
		for name, backend := range m.orchestrator.backends {
			if backend.State == StateFailed {
				snapshot.FailedBackends = append(snapshot.FailedBackends, name)
			}
		}
		m.orchestrator.mu.RUnlock()
	}

	snapshot.HealthScore = m.calculateHealthScoreLocked()

	// Get recent alerts
	if len(m.alerts) > 0 {
		start := len(m.alerts) - 10
		if start < 0 {
			start = 0
		}
		snapshot.ActiveAlerts = m.alerts[start:]
	}

	return snapshot
}

// GetMetricsHistory returns metrics history for the given duration
func (m *HAMonitor) GetMetricsHistory(duration time.Duration) []HAMetrics {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cutoff := time.Now().Add(-duration)
	result := make([]HAMetrics, 0)

	for _, metric := range m.metrics {
		if metric.Timestamp.After(cutoff) {
			result = append(result, metric)
		}
	}

	return result
}

// CleanupOldMetrics removes metrics older than retention period
func (m *HAMonitor) CleanupOldMetrics() {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-m.config.RetentionPeriod)
	newMetrics := make([]HAMetrics, 0)

	for _, metric := range m.metrics {
		if metric.Timestamp.After(cutoff) {
			newMetrics = append(newMetrics, metric)
		}
	}

	m.metrics = newMetrics
}

// CheckAlerts checks for alert conditions
func (m *HAMonitor) CheckAlerts(ctx context.Context) []Alert {
	m.mu.Lock()
	defer m.mu.Unlock()

	alerts := make([]Alert, 0)

	if m.orchestrator != nil {
		// Check backend failure rate
		m.orchestrator.mu.RLock()
		total := len(m.orchestrator.backends)
		failed := 0
		var maxLatency time.Duration

		for _, backend := range m.orchestrator.backends {
			if backend.State == StateFailed {
				failed++
			}
			if backend.Latency > maxLatency {
				maxLatency = backend.Latency
			}
		}
		m.orchestrator.mu.RUnlock()

		if total > 0 {
			failurePercent := float64(failed) / float64(total) * 100
			if failurePercent >= m.config.AlertThresholds.FailedBackendPercent {
				alert := Alert{
					Type:      AlertBackendFailure,
					Severity:  SeverityCritical,
					Message:   fmt.Sprintf("%.1f%% of backends have failed", failurePercent),
					Timestamp: time.Now(),
					Details: map[string]interface{}{
						"failed_count": failed,
						"total_count":  total,
						"percentage":   failurePercent,
					},
				}
				alerts = append(alerts, alert)
				m.alerts = append(m.alerts, alert)
			}
		}

		// Check latency
		if maxLatency > m.config.AlertThresholds.LatencyP99Threshold {
			alert := Alert{
				Type:      AlertHighLatency,
				Severity:  SeverityWarning,
				Message:   fmt.Sprintf("High latency detected: %v", maxLatency),
				Timestamp: time.Now(),
				Details: map[string]interface{}{
					"max_latency": maxLatency.String(),
					"threshold":   m.config.AlertThresholds.LatencyP99Threshold.String(),
				},
			}
			alerts = append(alerts, alert)
			m.alerts = append(m.alerts, alert)
		}
	}

	return alerts
}

// SubscribeAlerts subscribes to alert notifications
func (m *HAMonitor) SubscribeAlerts(handler func(Alert)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alertHandlers = append(m.alertHandlers, handler)
}

// EmitAlert emits an alert to all subscribers
func (m *HAMonitor) EmitAlert(alert Alert) {
	m.mu.Lock()
	handlers := make([]func(Alert), len(m.alertHandlers))
	copy(handlers, m.alertHandlers)
	m.alerts = append(m.alerts, alert)
	m.mu.Unlock()

	for _, handler := range handlers {
		go handler(alert)
	}
}

// GetDashboardData returns data for the HA dashboard
func (m *HAMonitor) GetDashboardData() DashboardData {
	m.mu.RLock()
	defer m.mu.RUnlock()

	data := DashboardData{
		GeneratedAt:   time.Now(),
		BackendStatus: make(map[string]BackendDashboardStatus),
		RegionStatus:  make(map[string]RegionDashboardStatus),
		RecentAlerts:  make([]Alert, 0),
		HealthScore:   m.calculateHealthScoreLocked(),
	}

	// Backend status
	if m.orchestrator != nil {
		m.orchestrator.mu.RLock()
		for name, backend := range m.orchestrator.backends {
			data.BackendStatus[name] = BackendDashboardStatus{
				Name:      name,
				State:     backend.State.String(),
				Latency:   backend.Latency,
				IsPrimary: backend.Config.Primary,
				LastCheck: backend.LastCheck,
			}
		}
		m.orchestrator.mu.RUnlock()
	}

	// Region status
	if m.geoManager != nil {
		regionStatus := m.geoManager.GetStatus()
		for region, status := range regionStatus {
			data.RegionStatus[string(region)] = RegionDashboardStatus{
				Name:    string(region),
				Healthy: status.Health == StateHealthy,
				Latency: status.Latency,
			}
		}
	}

	// Recent alerts (last 20)
	if len(m.alerts) > 0 {
		start := len(m.alerts) - 20
		if start < 0 {
			start = 0
		}
		data.RecentAlerts = m.alerts[start:]
	}

	return data
}

// GetPrometheusMetrics returns metrics in Prometheus format
func (m *HAMonitor) GetPrometheusMetrics() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var sb strings.Builder

	// Get latest metrics
	var latest HAMetrics
	if len(m.metrics) > 0 {
		latest = m.metrics[len(m.metrics)-1]
	}

	sb.WriteString("# HELP vaultaire_ha_backends_total Total number of backends\n")
	sb.WriteString("# TYPE vaultaire_ha_backends_total gauge\n")
	sb.WriteString(fmt.Sprintf("vaultaire_ha_backends_total %d\n", latest.TotalBackends))

	sb.WriteString("# HELP vaultaire_ha_backends_healthy Number of healthy backends\n")
	sb.WriteString("# TYPE vaultaire_ha_backends_healthy gauge\n")
	sb.WriteString(fmt.Sprintf("vaultaire_ha_backends_healthy %d\n", latest.HealthyBackends))

	sb.WriteString("# HELP vaultaire_ha_backends_unhealthy Number of unhealthy backends\n")
	sb.WriteString("# TYPE vaultaire_ha_backends_unhealthy gauge\n")
	sb.WriteString(fmt.Sprintf("vaultaire_ha_backends_unhealthy %d\n", latest.UnhealthyBackends))

	sb.WriteString("# HELP vaultaire_ha_latency_avg_ms Average latency in milliseconds\n")
	sb.WriteString("# TYPE vaultaire_ha_latency_avg_ms gauge\n")
	sb.WriteString(fmt.Sprintf("vaultaire_ha_latency_avg_ms %.2f\n", float64(latest.AverageLatency.Milliseconds())))

	sb.WriteString("# HELP vaultaire_ha_health_score System health score 0-100\n")
	sb.WriteString("# TYPE vaultaire_ha_health_score gauge\n")
	sb.WriteString(fmt.Sprintf("vaultaire_ha_health_score %.2f\n", m.calculateHealthScoreLocked()))

	sb.WriteString("# HELP vaultaire_ha_uptime_seconds System uptime in seconds\n")
	sb.WriteString("# TYPE vaultaire_ha_uptime_seconds counter\n")
	sb.WriteString(fmt.Sprintf("vaultaire_ha_uptime_seconds %.2f\n", time.Since(m.startTime).Seconds()))

	return sb.String()
}

// Start starts the monitoring loop
func (m *HAMonitor) Start(ctx context.Context) {
	ticker := time.NewTicker(m.config.CollectionInterval)
	defer ticker.Stop()

	cleanupTicker := time.NewTicker(time.Hour)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.CollectMetrics(ctx)
			alerts := m.CheckAlerts(ctx)
			for _, alert := range alerts {
				m.notifyHandlers(alert)
			}
		case <-cleanupTicker.C:
			m.CleanupOldMetrics()
		}
	}
}

// notifyHandlers notifies alert handlers
func (m *HAMonitor) notifyHandlers(alert Alert) {
	m.mu.RLock()
	handlers := make([]func(Alert), len(m.alertHandlers))
	copy(handlers, m.alertHandlers)
	m.mu.RUnlock()

	for _, handler := range handlers {
		go handler(alert)
	}
}

// CalculateHealthScore calculates overall system health score (0-100)
func (m *HAMonitor) CalculateHealthScore() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.calculateHealthScoreLocked()
}

// calculateHealthScoreLocked calculates health score (caller must hold lock)
func (m *HAMonitor) calculateHealthScoreLocked() float64 {
	score := 100.0

	if m.orchestrator != nil {
		m.orchestrator.mu.RLock()
		total := len(m.orchestrator.backends)
		healthy := 0
		degraded := 0

		for _, backend := range m.orchestrator.backends {
			switch backend.State {
			case StateHealthy:
				healthy++
			case StateDegraded:
				degraded++
			}
		}
		m.orchestrator.mu.RUnlock()

		if total > 0 {
			// Healthy backends contribute positively
			healthyPercent := float64(healthy) / float64(total) * 100

			// Failed backends reduce score significantly
			failed := total - healthy - degraded
			failedPenalty := float64(failed) / float64(total) * 50

			// Degraded backends reduce score moderately
			degradedPenalty := float64(degraded) / float64(total) * 20

			score = healthyPercent - failedPenalty - degradedPenalty

			if score < 0 {
				score = 0
			}
		}
	}

	return score
}

// GetUptime returns system uptime
func (m *HAMonitor) GetUptime() time.Duration {
	return time.Since(m.startTime)
}
