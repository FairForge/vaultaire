// internal/global/monitoring.go
package global

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MetricType represents types of metrics
type MetricType string

const (
	MetricTypeCounter   MetricType = "counter"
	MetricTypeGauge     MetricType = "gauge"
	MetricTypeHistogram MetricType = "histogram"
	MetricTypeSummary   MetricType = "summary"
)

// Metric represents a single metric
type Metric struct {
	Name        string
	Type        MetricType
	Value       float64
	Labels      map[string]string
	Timestamp   time.Time
	Description string
}

// MetricSeries represents a time series of metrics
type MetricSeries struct {
	Name       string
	Type       MetricType
	Labels     map[string]string
	DataPoints []DataPoint
}

// DataPoint represents a single data point
type DataPoint struct {
	Value     float64
	Timestamp time.Time
}

// Alert represents a monitoring alert
type Alert struct {
	ID          string
	Name        string
	Severity    AlertSeverity
	Status      AlertStatus
	Message     string
	Labels      map[string]string
	FiredAt     time.Time
	ResolvedAt  time.Time
	AckedAt     time.Time
	AckedBy     string
	Annotations map[string]string
}

// AlertSeverity represents alert severity levels
type AlertSeverity string

const (
	AlertSeverityCritical AlertSeverity = "critical"
	AlertSeverityWarning  AlertSeverity = "warning"
	AlertSeverityInfo     AlertSeverity = "info"
)

// AlertStatus represents alert status
type AlertStatus string

const (
	AlertStatusFiring   AlertStatus = "firing"
	AlertStatusResolved AlertStatus = "resolved"
	AlertStatusAcked    AlertStatus = "acknowledged"
)

// AlertRule defines when to trigger an alert
type AlertRule struct {
	ID          string
	Name        string
	Description string
	MetricName  string
	Condition   AlertCondition
	Threshold   float64
	Duration    time.Duration
	Severity    AlertSeverity
	Labels      map[string]string
	Annotations map[string]string
	Enabled     bool
}

// AlertCondition represents alert comparison condition
type AlertCondition string

const (
	ConditionGreaterThan    AlertCondition = "gt"
	ConditionLessThan       AlertCondition = "lt"
	ConditionGreaterOrEqual AlertCondition = "gte"
	ConditionLessOrEqual    AlertCondition = "lte"
	ConditionEqual          AlertCondition = "eq"
	ConditionNotEqual       AlertCondition = "ne"
)

// GlobalMonitor provides global monitoring capabilities
type GlobalMonitor struct {
	mu          sync.RWMutex
	metrics     map[string]*MetricSeries
	alerts      map[string]*Alert
	alertRules  map[string]*AlertRule
	edgeManager *EdgeManager
	config      *MonitorConfig
	collectors  []MetricCollector
	handlers    []AlertHandler
	stopCh      chan struct{}
	wg          sync.WaitGroup
}

// MetricCollector collects metrics
type MetricCollector interface {
	Collect(ctx context.Context) ([]Metric, error)
	Name() string
}

// AlertHandler handles alert notifications
type AlertHandler interface {
	Handle(ctx context.Context, alert *Alert) error
	Name() string
}

// MonitorConfig configures the global monitor
type MonitorConfig struct {
	CollectionInterval      time.Duration
	RetentionPeriod         time.Duration
	MaxDataPoints           int
	MaxAlerts               int
	EnableAutoCollection    bool
	AlertEvaluationInterval time.Duration
}

// DefaultMonitorConfig returns default configuration
func DefaultMonitorConfig() *MonitorConfig {
	return &MonitorConfig{
		CollectionInterval:      30 * time.Second,
		RetentionPeriod:         24 * time.Hour,
		MaxDataPoints:           1000,
		MaxAlerts:               500,
		EnableAutoCollection:    false,
		AlertEvaluationInterval: 30 * time.Second,
	}
}

// NewGlobalMonitor creates a new global monitor
func NewGlobalMonitor(edgeManager *EdgeManager, config *MonitorConfig) *GlobalMonitor {
	if config == nil {
		config = DefaultMonitorConfig()
	}

	return &GlobalMonitor{
		metrics:     make(map[string]*MetricSeries),
		alerts:      make(map[string]*Alert),
		alertRules:  make(map[string]*AlertRule),
		edgeManager: edgeManager,
		config:      config,
		collectors:  make([]MetricCollector, 0),
		handlers:    make([]AlertHandler, 0),
		stopCh:      make(chan struct{}),
	}
}

// RecordMetric records a metric value
func (gm *GlobalMonitor) RecordMetric(m Metric) {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	if m.Timestamp.IsZero() {
		m.Timestamp = time.Now()
	}

	key := gm.metricKey(m.Name, m.Labels)
	series, ok := gm.metrics[key]
	if !ok {
		series = &MetricSeries{
			Name:       m.Name,
			Type:       m.Type,
			Labels:     m.Labels,
			DataPoints: make([]DataPoint, 0),
		}
		gm.metrics[key] = series
	}

	series.DataPoints = append(series.DataPoints, DataPoint{
		Value:     m.Value,
		Timestamp: m.Timestamp,
	})

	// Trim old data points
	gm.trimDataPoints(series)
}

func (gm *GlobalMonitor) metricKey(name string, labels map[string]string) string {
	key := name
	if len(labels) > 0 {
		// Sort labels for consistent key
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			key += fmt.Sprintf(",%s=%s", k, labels[k])
		}
	}
	return key
}

func (gm *GlobalMonitor) trimDataPoints(series *MetricSeries) {
	// Trim by retention period
	cutoff := time.Now().Add(-gm.config.RetentionPeriod)
	var trimmed []DataPoint
	for _, dp := range series.DataPoints {
		if dp.Timestamp.After(cutoff) {
			trimmed = append(trimmed, dp)
		}
	}

	// Trim by max count
	if len(trimmed) > gm.config.MaxDataPoints {
		trimmed = trimmed[len(trimmed)-gm.config.MaxDataPoints:]
	}

	series.DataPoints = trimmed
}

// GetMetric returns a metric series
func (gm *GlobalMonitor) GetMetric(name string, labels map[string]string) (*MetricSeries, bool) {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	key := gm.metricKey(name, labels)
	series, ok := gm.metrics[key]
	return series, ok
}

// GetMetricsByName returns all series for a metric name
func (gm *GlobalMonitor) GetMetricsByName(name string) []*MetricSeries {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	var result []*MetricSeries
	for _, series := range gm.metrics {
		if series.Name == name {
			result = append(result, series)
		}
	}
	return result
}

// GetAllMetrics returns all metric series
func (gm *GlobalMonitor) GetAllMetrics() map[string]*MetricSeries {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	result := make(map[string]*MetricSeries)
	for k, v := range gm.metrics {
		result[k] = v
	}
	return result
}

// GetLatestValue returns the latest value for a metric
func (gm *GlobalMonitor) GetLatestValue(name string, labels map[string]string) (float64, bool) {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	key := gm.metricKey(name, labels)
	series, ok := gm.metrics[key]
	if !ok || len(series.DataPoints) == 0 {
		return 0, false
	}

	return series.DataPoints[len(series.DataPoints)-1].Value, true
}

// AddAlertRule adds an alert rule
func (gm *GlobalMonitor) AddAlertRule(rule *AlertRule) error {
	if rule.ID == "" {
		return fmt.Errorf("rule ID required")
	}
	if rule.MetricName == "" {
		return fmt.Errorf("metric name required")
	}

	gm.mu.Lock()
	defer gm.mu.Unlock()

	gm.alertRules[rule.ID] = rule
	return nil
}

// GetAlertRule returns an alert rule
func (gm *GlobalMonitor) GetAlertRule(id string) (*AlertRule, bool) {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	rule, ok := gm.alertRules[id]
	return rule, ok
}

// GetAlertRules returns all alert rules
func (gm *GlobalMonitor) GetAlertRules() []*AlertRule {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	rules := make([]*AlertRule, 0, len(gm.alertRules))
	for _, r := range gm.alertRules {
		rules = append(rules, r)
	}
	return rules
}

// RemoveAlertRule removes an alert rule
func (gm *GlobalMonitor) RemoveAlertRule(id string) bool {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	if _, ok := gm.alertRules[id]; !ok {
		return false
	}
	delete(gm.alertRules, id)
	return true
}

// EnableAlertRule enables an alert rule
func (gm *GlobalMonitor) EnableAlertRule(id string) error {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	rule, ok := gm.alertRules[id]
	if !ok {
		return fmt.Errorf("rule not found: %s", id)
	}
	rule.Enabled = true
	return nil
}

// DisableAlertRule disables an alert rule
func (gm *GlobalMonitor) DisableAlertRule(id string) error {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	rule, ok := gm.alertRules[id]
	if !ok {
		return fmt.Errorf("rule not found: %s", id)
	}
	rule.Enabled = false
	return nil
}

// FireAlert fires an alert
func (gm *GlobalMonitor) FireAlert(alert *Alert) {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	if alert.ID == "" {
		alert.ID = fmt.Sprintf("alert-%d", time.Now().UnixNano())
	}
	if alert.FiredAt.IsZero() {
		alert.FiredAt = time.Now()
	}
	if alert.Status == "" {
		alert.Status = AlertStatusFiring
	}

	gm.alerts[alert.ID] = alert

	// Trim old alerts
	gm.trimAlerts()

	// Notify handlers (async)
	for _, handler := range gm.handlers {
		go func(h AlertHandler) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = h.Handle(ctx, alert)
		}(handler)
	}
}

func (gm *GlobalMonitor) trimAlerts() {
	if len(gm.alerts) <= gm.config.MaxAlerts {
		return
	}

	// Get all alerts sorted by time
	alerts := make([]*Alert, 0, len(gm.alerts))
	for _, a := range gm.alerts {
		alerts = append(alerts, a)
	}
	sort.Slice(alerts, func(i, j int) bool {
		return alerts[i].FiredAt.Before(alerts[j].FiredAt)
	})

	// Remove oldest
	toRemove := len(alerts) - gm.config.MaxAlerts
	for i := 0; i < toRemove; i++ {
		delete(gm.alerts, alerts[i].ID)
	}
}

// GetAlert returns an alert
func (gm *GlobalMonitor) GetAlert(id string) (*Alert, bool) {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	alert, ok := gm.alerts[id]
	return alert, ok
}

// GetAlerts returns alerts filtered by status
func (gm *GlobalMonitor) GetAlerts(status AlertStatus) []*Alert {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	var result []*Alert
	for _, a := range gm.alerts {
		if status == "" || a.Status == status {
			result = append(result, a)
		}
	}
	return result
}

// GetFiringAlerts returns all firing alerts
func (gm *GlobalMonitor) GetFiringAlerts() []*Alert {
	return gm.GetAlerts(AlertStatusFiring)
}

// AcknowledgeAlert acknowledges an alert
func (gm *GlobalMonitor) AcknowledgeAlert(id, ackedBy string) error {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	alert, ok := gm.alerts[id]
	if !ok {
		return fmt.Errorf("alert not found: %s", id)
	}

	alert.Status = AlertStatusAcked
	alert.AckedAt = time.Now()
	alert.AckedBy = ackedBy
	return nil
}

// ResolveAlert resolves an alert
func (gm *GlobalMonitor) ResolveAlert(id string) error {
	gm.mu.Lock()
	defer gm.mu.Unlock()

	alert, ok := gm.alerts[id]
	if !ok {
		return fmt.Errorf("alert not found: %s", id)
	}

	alert.Status = AlertStatusResolved
	alert.ResolvedAt = time.Now()
	return nil
}

// RegisterCollector registers a metric collector
func (gm *GlobalMonitor) RegisterCollector(collector MetricCollector) {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	gm.collectors = append(gm.collectors, collector)
}

// RegisterHandler registers an alert handler
func (gm *GlobalMonitor) RegisterHandler(handler AlertHandler) {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	gm.handlers = append(gm.handlers, handler)
}

// Start starts automatic collection and alert evaluation
func (gm *GlobalMonitor) Start() {
	if !gm.config.EnableAutoCollection {
		return
	}

	gm.wg.Add(2)
	go gm.collectionLoop()
	go gm.alertEvaluationLoop()
}

// Stop stops automatic collection
func (gm *GlobalMonitor) Stop() {
	close(gm.stopCh)
	gm.wg.Wait()
}

func (gm *GlobalMonitor) collectionLoop() {
	defer gm.wg.Done()

	ticker := time.NewTicker(gm.config.CollectionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			gm.collectAll()
		case <-gm.stopCh:
			return
		}
	}
}

func (gm *GlobalMonitor) collectAll() {
	gm.mu.RLock()
	collectors := make([]MetricCollector, len(gm.collectors))
	copy(collectors, gm.collectors)
	gm.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), gm.config.CollectionInterval)
	defer cancel()

	for _, collector := range collectors {
		metrics, err := collector.Collect(ctx)
		if err != nil {
			continue
		}
		for _, m := range metrics {
			gm.RecordMetric(m)
		}
	}
}

func (gm *GlobalMonitor) alertEvaluationLoop() {
	defer gm.wg.Done()

	ticker := time.NewTicker(gm.config.AlertEvaluationInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			gm.evaluateAlertRules()
		case <-gm.stopCh:
			return
		}
	}
}

func (gm *GlobalMonitor) evaluateAlertRules() {
	gm.mu.RLock()
	rules := make([]*AlertRule, 0, len(gm.alertRules))
	for _, r := range gm.alertRules {
		if r.Enabled {
			rules = append(rules, r)
		}
	}
	gm.mu.RUnlock()

	for _, rule := range rules {
		gm.evaluateRule(rule)
	}
}

func (gm *GlobalMonitor) evaluateRule(rule *AlertRule) {
	series := gm.GetMetricsByName(rule.MetricName)

	for _, s := range series {
		if len(s.DataPoints) == 0 {
			continue
		}

		latestValue := s.DataPoints[len(s.DataPoints)-1].Value
		shouldFire := gm.checkCondition(latestValue, rule.Condition, rule.Threshold)

		if shouldFire {
			// Check if alert already exists
			alertID := fmt.Sprintf("%s-%s", rule.ID, gm.metricKey(s.Name, s.Labels))
			if _, exists := gm.GetAlert(alertID); !exists {
				gm.FireAlert(&Alert{
					ID:       alertID,
					Name:     rule.Name,
					Severity: rule.Severity,
					Status:   AlertStatusFiring,
					Message:  fmt.Sprintf("%s: %v %s %v", rule.Name, latestValue, rule.Condition, rule.Threshold),
					Labels:   rule.Labels,
				})
			}
		}
	}
}

func (gm *GlobalMonitor) checkCondition(value float64, condition AlertCondition, threshold float64) bool {
	switch condition {
	case ConditionGreaterThan:
		return value > threshold
	case ConditionLessThan:
		return value < threshold
	case ConditionGreaterOrEqual:
		return value >= threshold
	case ConditionLessOrEqual:
		return value <= threshold
	case ConditionEqual:
		return value == threshold
	case ConditionNotEqual:
		return value != threshold
	default:
		return false
	}
}

// MonitoringReport generates a monitoring report
type MonitoringReport struct {
	GeneratedAt     time.Time
	TotalMetrics    int
	TotalDataPoints int
	TotalAlertRules int
	EnabledRules    int
	TotalAlerts     int
	FiringAlerts    int
	AckedAlerts     int
	ResolvedAlerts  int
	CollectorCount  int
	HandlerCount    int
}

// GenerateReport generates a monitoring report
func (gm *GlobalMonitor) GenerateReport() *MonitoringReport {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	report := &MonitoringReport{
		GeneratedAt:     time.Now(),
		TotalMetrics:    len(gm.metrics),
		TotalAlertRules: len(gm.alertRules),
		TotalAlerts:     len(gm.alerts),
		CollectorCount:  len(gm.collectors),
		HandlerCount:    len(gm.handlers),
	}

	for _, series := range gm.metrics {
		report.TotalDataPoints += len(series.DataPoints)
	}

	for _, rule := range gm.alertRules {
		if rule.Enabled {
			report.EnabledRules++
		}
	}

	for _, alert := range gm.alerts {
		switch alert.Status {
		case AlertStatusFiring:
			report.FiringAlerts++
		case AlertStatusAcked:
			report.AckedAlerts++
		case AlertStatusResolved:
			report.ResolvedAlerts++
		}
	}

	return report
}

// CommonAlertRules returns common alert rule configurations
func CommonAlertRules() []*AlertRule {
	return []*AlertRule{
		{
			ID:          "high-error-rate",
			Name:        "High Error Rate",
			Description: "Error rate exceeds 5%",
			MetricName:  "error_rate",
			Condition:   ConditionGreaterThan,
			Threshold:   0.05,
			Severity:    AlertSeverityCritical,
			Enabled:     true,
		},
		{
			ID:          "high-latency",
			Name:        "High Latency",
			Description: "P99 latency exceeds 1 second",
			MetricName:  "latency_p99",
			Condition:   ConditionGreaterThan,
			Threshold:   1000,
			Severity:    AlertSeverityWarning,
			Enabled:     true,
		},
		{
			ID:          "low-availability",
			Name:        "Low Availability",
			Description: "Availability below 99.9%",
			MetricName:  "availability",
			Condition:   ConditionLessThan,
			Threshold:   0.999,
			Severity:    AlertSeverityCritical,
			Enabled:     true,
		},
		{
			ID:          "high-cpu",
			Name:        "High CPU Usage",
			Description: "CPU usage exceeds 80%",
			MetricName:  "cpu_percent",
			Condition:   ConditionGreaterThan,
			Threshold:   80,
			Severity:    AlertSeverityWarning,
			Enabled:     true,
		},
		{
			ID:          "high-memory",
			Name:        "High Memory Usage",
			Description: "Memory usage exceeds 85%",
			MetricName:  "memory_percent",
			Condition:   ConditionGreaterThan,
			Threshold:   85,
			Severity:    AlertSeverityWarning,
			Enabled:     true,
		},
		{
			ID:          "disk-full",
			Name:        "Disk Nearly Full",
			Description: "Disk usage exceeds 90%",
			MetricName:  "disk_percent",
			Condition:   ConditionGreaterThan,
			Threshold:   90,
			Severity:    AlertSeverityCritical,
			Enabled:     true,
		},
	}
}

// EdgeCollector collects metrics from edge locations
type EdgeCollector struct {
	edgeManager *EdgeManager
}

// NewEdgeCollector creates a new edge collector
func NewEdgeCollector(em *EdgeManager) *EdgeCollector {
	return &EdgeCollector{edgeManager: em}
}

// Name returns the collector name
func (ec *EdgeCollector) Name() string {
	return "edge_collector"
}

// Collect collects metrics from edge locations
func (ec *EdgeCollector) Collect(ctx context.Context) ([]Metric, error) {
	locations := ec.edgeManager.GetLocations()
	metrics := make([]Metric, 0)

	for _, loc := range locations {
		status, ok := ec.edgeManager.GetStatus(loc.ID)
		if !ok {
			continue
		}

		labels := map[string]string{
			"location": loc.ID,
			"region":   loc.Region,
			"country":  loc.Country,
		}

		metrics = append(metrics,
			Metric{
				Name:   "edge_cpu_percent",
				Type:   MetricTypeGauge,
				Value:  status.CPUPercent,
				Labels: labels,
			},
			Metric{
				Name:   "edge_memory_percent",
				Type:   MetricTypeGauge,
				Value:  status.MemoryPercent,
				Labels: labels,
			},
			Metric{
				Name:   "edge_request_count",
				Type:   MetricTypeCounter,
				Value:  float64(status.RequestCount),
				Labels: labels,
			},
			Metric{
				Name:   "edge_bytes_served",
				Type:   MetricTypeCounter,
				Value:  float64(status.BytesServed),
				Labels: labels,
			},
			Metric{
				Name:   "edge_error_count",
				Type:   MetricTypeCounter,
				Value:  float64(status.ErrorCount),
				Labels: labels,
			},
			Metric{
				Name:   "edge_active_conns",
				Type:   MetricTypeGauge,
				Value:  float64(status.ActiveConns),
				Labels: labels,
			},
			Metric{
				Name:   "edge_latency_ms",
				Type:   MetricTypeGauge,
				Value:  float64(status.Latency.Milliseconds()),
				Labels: labels,
			},
		)

		// Health as 0/1
		healthValue := 0.0
		if status.Healthy {
			healthValue = 1.0
		}
		metrics = append(metrics, Metric{
			Name:   "edge_healthy",
			Type:   MetricTypeGauge,
			Value:  healthValue,
			Labels: labels,
		})
	}

	return metrics, nil
}
