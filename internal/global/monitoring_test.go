// internal/global/monitoring_test.go
package global

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestNewGlobalMonitor(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	if gm == nil {
		t.Fatal("expected non-nil monitor")
	}
	if gm.config == nil {
		t.Error("expected default config")
	}
}

func TestGlobalMonitorRecordMetric(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	gm.RecordMetric(Metric{
		Name:  "test_metric",
		Type:  MetricTypeGauge,
		Value: 42.5,
	})

	series, ok := gm.GetMetric("test_metric", nil)
	if !ok {
		t.Fatal("metric not found")
	}
	if len(series.DataPoints) != 1 {
		t.Errorf("expected 1 data point, got %d", len(series.DataPoints))
	}
	if series.DataPoints[0].Value != 42.5 {
		t.Error("value not recorded correctly")
	}
}

func TestGlobalMonitorRecordMetricWithLabels(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	labels := map[string]string{"region": "us-east", "host": "server1"}
	gm.RecordMetric(Metric{
		Name:   "cpu_usage",
		Type:   MetricTypeGauge,
		Value:  75.5,
		Labels: labels,
	})

	series, ok := gm.GetMetric("cpu_usage", labels)
	if !ok {
		t.Fatal("metric not found")
	}
	if series.Labels["region"] != "us-east" {
		t.Error("labels not saved")
	}
}

func TestGlobalMonitorGetLatestValue(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	gm.RecordMetric(Metric{Name: "test", Value: 1})
	gm.RecordMetric(Metric{Name: "test", Value: 2})
	gm.RecordMetric(Metric{Name: "test", Value: 3})

	val, ok := gm.GetLatestValue("test", nil)
	if !ok {
		t.Fatal("value not found")
	}
	if val != 3 {
		t.Errorf("expected 3, got %v", val)
	}
}

func TestGlobalMonitorGetLatestValueNotFound(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	_, ok := gm.GetLatestValue("nonexistent", nil)
	if ok {
		t.Error("expected not found")
	}
}

func TestGlobalMonitorGetMetricsByName(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	gm.RecordMetric(Metric{Name: "cpu", Value: 50, Labels: map[string]string{"host": "a"}})
	gm.RecordMetric(Metric{Name: "cpu", Value: 60, Labels: map[string]string{"host": "b"}})
	gm.RecordMetric(Metric{Name: "memory", Value: 70})

	cpuSeries := gm.GetMetricsByName("cpu")
	if len(cpuSeries) != 2 {
		t.Errorf("expected 2 cpu series, got %d", len(cpuSeries))
	}
}

func TestGlobalMonitorGetAllMetrics(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	gm.RecordMetric(Metric{Name: "m1", Value: 1})
	gm.RecordMetric(Metric{Name: "m2", Value: 2})

	all := gm.GetAllMetrics()
	if len(all) != 2 {
		t.Errorf("expected 2 metrics, got %d", len(all))
	}
}

func TestGlobalMonitorAddAlertRule(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	err := gm.AddAlertRule(&AlertRule{
		ID:         "test-rule",
		Name:       "Test Rule",
		MetricName: "cpu",
		Condition:  ConditionGreaterThan,
		Threshold:  80,
		Severity:   AlertSeverityWarning,
		Enabled:    true,
	})

	if err != nil {
		t.Fatalf("failed to add rule: %v", err)
	}

	rule, ok := gm.GetAlertRule("test-rule")
	if !ok {
		t.Fatal("rule not found")
	}
	if rule.Name != "Test Rule" {
		t.Error("rule not saved correctly")
	}
}

func TestGlobalMonitorAddAlertRuleValidation(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	// No ID
	err := gm.AddAlertRule(&AlertRule{MetricName: "cpu"})
	if err == nil {
		t.Error("expected error for missing ID")
	}

	// No metric name
	err = gm.AddAlertRule(&AlertRule{ID: "test"})
	if err == nil {
		t.Error("expected error for missing metric name")
	}
}

func TestGlobalMonitorGetAlertRules(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	_ = gm.AddAlertRule(&AlertRule{ID: "r1", MetricName: "m1"})
	_ = gm.AddAlertRule(&AlertRule{ID: "r2", MetricName: "m2"})

	rules := gm.GetAlertRules()
	if len(rules) != 2 {
		t.Errorf("expected 2 rules, got %d", len(rules))
	}
}

func TestGlobalMonitorRemoveAlertRule(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	_ = gm.AddAlertRule(&AlertRule{ID: "test", MetricName: "cpu"})

	removed := gm.RemoveAlertRule("test")
	if !removed {
		t.Error("expected rule to be removed")
	}

	_, ok := gm.GetAlertRule("test")
	if ok {
		t.Error("rule should not exist")
	}

	removed = gm.RemoveAlertRule("nonexistent")
	if removed {
		t.Error("should not remove nonexistent")
	}
}

func TestGlobalMonitorEnableDisableAlertRule(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	_ = gm.AddAlertRule(&AlertRule{ID: "test", MetricName: "cpu", Enabled: true})

	err := gm.DisableAlertRule("test")
	if err != nil {
		t.Fatalf("disable failed: %v", err)
	}

	rule, _ := gm.GetAlertRule("test")
	if rule.Enabled {
		t.Error("expected disabled")
	}

	err = gm.EnableAlertRule("test")
	if err != nil {
		t.Fatalf("enable failed: %v", err)
	}

	rule, _ = gm.GetAlertRule("test")
	if !rule.Enabled {
		t.Error("expected enabled")
	}
}

func TestGlobalMonitorEnableDisableNonexistent(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	err := gm.EnableAlertRule("nonexistent")
	if err == nil {
		t.Error("expected error")
	}

	err = gm.DisableAlertRule("nonexistent")
	if err == nil {
		t.Error("expected error")
	}
}

func TestGlobalMonitorFireAlert(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	gm.FireAlert(&Alert{
		Name:     "Test Alert",
		Severity: AlertSeverityCritical,
		Message:  "Something is wrong",
	})

	alerts := gm.GetAlerts("")
	if len(alerts) != 1 {
		t.Errorf("expected 1 alert, got %d", len(alerts))
	}
	if alerts[0].Name != "Test Alert" {
		t.Error("alert not saved correctly")
	}
	if alerts[0].ID == "" {
		t.Error("expected auto-generated ID")
	}
}

func TestGlobalMonitorGetFiringAlerts(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	gm.FireAlert(&Alert{ID: "a1", Name: "Alert 1", Status: AlertStatusFiring})
	gm.FireAlert(&Alert{ID: "a2", Name: "Alert 2", Status: AlertStatusResolved})

	firing := gm.GetFiringAlerts()
	if len(firing) != 1 {
		t.Errorf("expected 1 firing alert, got %d", len(firing))
	}
}

func TestGlobalMonitorAcknowledgeAlert(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	gm.FireAlert(&Alert{ID: "test-alert", Name: "Test"})

	err := gm.AcknowledgeAlert("test-alert", "admin")
	if err != nil {
		t.Fatalf("ack failed: %v", err)
	}

	alert, _ := gm.GetAlert("test-alert")
	if alert.Status != AlertStatusAcked {
		t.Error("expected acked status")
	}
	if alert.AckedBy != "admin" {
		t.Error("acked by not set")
	}
}

func TestGlobalMonitorAcknowledgeNonexistent(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	err := gm.AcknowledgeAlert("nonexistent", "admin")
	if err == nil {
		t.Error("expected error")
	}
}

func TestGlobalMonitorResolveAlert(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	gm.FireAlert(&Alert{ID: "test-alert", Name: "Test"})

	err := gm.ResolveAlert("test-alert")
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}

	alert, _ := gm.GetAlert("test-alert")
	if alert.Status != AlertStatusResolved {
		t.Error("expected resolved status")
	}
}

func TestGlobalMonitorResolveNonexistent(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	err := gm.ResolveAlert("nonexistent")
	if err == nil {
		t.Error("expected error")
	}
}

func TestGlobalMonitorGenerateReport(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	gm.RecordMetric(Metric{Name: "m1", Value: 1})
	gm.RecordMetric(Metric{Name: "m1", Value: 2})
	gm.RecordMetric(Metric{Name: "m2", Value: 3})

	_ = gm.AddAlertRule(&AlertRule{ID: "r1", MetricName: "m1", Enabled: true})
	_ = gm.AddAlertRule(&AlertRule{ID: "r2", MetricName: "m2", Enabled: false})

	gm.FireAlert(&Alert{ID: "a1", Status: AlertStatusFiring})
	gm.FireAlert(&Alert{ID: "a2", Status: AlertStatusResolved})

	report := gm.GenerateReport()

	if report.TotalMetrics != 2 {
		t.Errorf("expected 2 metrics, got %d", report.TotalMetrics)
	}
	if report.TotalDataPoints != 3 {
		t.Errorf("expected 3 data points, got %d", report.TotalDataPoints)
	}
	if report.TotalAlertRules != 2 {
		t.Errorf("expected 2 rules, got %d", report.TotalAlertRules)
	}
	if report.EnabledRules != 1 {
		t.Errorf("expected 1 enabled, got %d", report.EnabledRules)
	}
	if report.FiringAlerts != 1 {
		t.Errorf("expected 1 firing, got %d", report.FiringAlerts)
	}
	if report.ResolvedAlerts != 1 {
		t.Errorf("expected 1 resolved, got %d", report.ResolvedAlerts)
	}
}

func TestDefaultMonitorConfig(t *testing.T) {
	config := DefaultMonitorConfig()

	if config.CollectionInterval != 30*time.Second {
		t.Error("unexpected collection interval")
	}
	if config.RetentionPeriod != 24*time.Hour {
		t.Error("unexpected retention period")
	}
	if config.EnableAutoCollection {
		t.Error("auto collection should be disabled by default")
	}
}

func TestCommonAlertRules(t *testing.T) {
	rules := CommonAlertRules()

	if len(rules) == 0 {
		t.Fatal("expected common rules")
	}

	// Check for high error rate rule
	found := false
	for _, r := range rules {
		if r.ID == "high-error-rate" {
			found = true
			if r.Severity != AlertSeverityCritical {
				t.Error("expected critical severity")
			}
		}
	}
	if !found {
		t.Error("expected high-error-rate rule")
	}
}

func TestMetricTypes(t *testing.T) {
	types := []MetricType{
		MetricTypeCounter,
		MetricTypeGauge,
		MetricTypeHistogram,
		MetricTypeSummary,
	}

	for _, mt := range types {
		if mt == "" {
			t.Error("metric type should not be empty")
		}
	}
}

func TestAlertSeverities(t *testing.T) {
	severities := []AlertSeverity{
		AlertSeverityCritical,
		AlertSeverityWarning,
		AlertSeverityInfo,
	}

	for _, s := range severities {
		if s == "" {
			t.Error("severity should not be empty")
		}
	}
}

func TestAlertConditions(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	tests := []struct {
		value     float64
		condition AlertCondition
		threshold float64
		expected  bool
	}{
		{10, ConditionGreaterThan, 5, true},
		{10, ConditionGreaterThan, 15, false},
		{10, ConditionLessThan, 15, true},
		{10, ConditionLessThan, 5, false},
		{10, ConditionGreaterOrEqual, 10, true},
		{10, ConditionLessOrEqual, 10, true},
		{10, ConditionEqual, 10, true},
		{10, ConditionEqual, 11, false},
		{10, ConditionNotEqual, 11, true},
		{10, ConditionNotEqual, 10, false},
	}

	for _, tc := range tests {
		result := gm.checkCondition(tc.value, tc.condition, tc.threshold)
		if result != tc.expected {
			t.Errorf("checkCondition(%v, %v, %v) = %v, want %v",
				tc.value, tc.condition, tc.threshold, result, tc.expected)
		}
	}
}

func TestEdgeCollector(t *testing.T) {
	em := NewEdgeManager(nil)
	_ = em.RegisterLocation(&EdgeLocation{
		ID:      "loc-1",
		Region:  "us-east",
		Country: "US",
		Enabled: true,
	})
	em.UpdateStatus(&EdgeStatus{
		LocationID:    "loc-1",
		Healthy:       true,
		CPUPercent:    50,
		MemoryPercent: 60,
		RequestCount:  1000,
		BytesServed:   1000000,
		Latency:       25 * time.Millisecond,
	})

	collector := NewEdgeCollector(em)
	if collector.Name() != "edge_collector" {
		t.Error("unexpected collector name")
	}

	ctx := context.Background()
	metrics, err := collector.Collect(ctx)
	if err != nil {
		t.Fatalf("collect failed: %v", err)
	}

	if len(metrics) == 0 {
		t.Fatal("expected metrics")
	}

	// Check for expected metrics
	foundCPU := false
	foundHealthy := false
	for _, m := range metrics {
		if m.Name == "edge_cpu_percent" && m.Value == 50 {
			foundCPU = true
		}
		if m.Name == "edge_healthy" && m.Value == 1.0 {
			foundHealthy = true
		}
	}
	if !foundCPU {
		t.Error("expected CPU metric")
	}
	if !foundHealthy {
		t.Error("expected healthy metric")
	}
}

func TestGlobalMonitorRegisterCollector(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	collector := NewEdgeCollector(em)
	gm.RegisterCollector(collector)

	report := gm.GenerateReport()
	if report.CollectorCount != 1 {
		t.Errorf("expected 1 collector, got %d", report.CollectorCount)
	}
}

type mockHandler struct {
	handled []*Alert
}

func (m *mockHandler) Handle(ctx context.Context, alert *Alert) error {
	m.handled = append(m.handled, alert)
	return nil
}

func (m *mockHandler) Name() string {
	return "mock_handler"
}

func TestGlobalMonitorRegisterHandler(t *testing.T) {
	em := NewEdgeManager(nil)
	gm := NewGlobalMonitor(em, nil)

	handler := &mockHandler{}
	gm.RegisterHandler(handler)

	report := gm.GenerateReport()
	if report.HandlerCount != 1 {
		t.Errorf("expected 1 handler, got %d", report.HandlerCount)
	}
}

func TestGlobalMonitorTrimDataPoints(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &MonitorConfig{
		RetentionPeriod: time.Hour,
		MaxDataPoints:   5,
	}
	gm := NewGlobalMonitor(em, config)

	// Record more than max
	for i := 0; i < 10; i++ {
		gm.RecordMetric(Metric{Name: "test", Value: float64(i)})
	}

	series, _ := gm.GetMetric("test", nil)
	if len(series.DataPoints) != 5 {
		t.Errorf("expected 5 data points (max), got %d", len(series.DataPoints))
	}
}

func TestGlobalMonitorTrimAlerts(t *testing.T) {
	em := NewEdgeManager(nil)
	config := &MonitorConfig{
		MaxAlerts: 3,
	}
	gm := NewGlobalMonitor(em, config)

	// Fire more than max
	for i := 0; i < 5; i++ {
		gm.FireAlert(&Alert{
			ID:   fmt.Sprintf("alert-%d", i),
			Name: fmt.Sprintf("Alert %d", i),
		})
	}

	alerts := gm.GetAlerts("")
	if len(alerts) != 3 {
		t.Errorf("expected 3 alerts (max), got %d", len(alerts))
	}
}
