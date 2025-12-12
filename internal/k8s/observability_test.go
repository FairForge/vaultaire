// internal/k8s/observability_test.go
package k8s

import (
	"strings"
	"testing"
)

func TestNewObservabilityManager(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	if om.namespace != "monitoring" {
		t.Errorf("expected namespace 'monitoring', got '%s'", om.namespace)
	}
}

func TestGenerateServiceMonitor(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	sm := om.GenerateServiceMonitor(ServiceMonitorCfg{
		Name:     "test-monitor",
		Selector: map[string]string{"app": "test"},
		Endpoints: []MonitorEndpoint{
			{
				Port:     "metrics",
				Path:     "/metrics",
				Interval: "30s",
			},
		},
	})

	if sm.Kind != "ServiceMonitor" {
		t.Errorf("expected kind ServiceMonitor, got %s", sm.Kind)
	}
	if sm.APIVersion != "monitoring.coreos.com/v1" {
		t.Errorf("expected apiVersion monitoring.coreos.com/v1, got %s", sm.APIVersion)
	}
	if sm.Spec.Selector.MatchLabels["app"] != "test" {
		t.Error("expected selector app=test")
	}
	if len(sm.Spec.Endpoints) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(sm.Spec.Endpoints))
	}
	if sm.Spec.Endpoints[0].Port != "metrics" {
		t.Error("expected port metrics")
	}
}

func TestGenerateServiceMonitorWithNamespaceSelector(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	sm := om.GenerateServiceMonitor(ServiceMonitorCfg{
		Name:     "test-monitor",
		Selector: map[string]string{"app": "test"},
		NamespaceSelector: &NamespaceSelector{
			MatchNames: []string{"app-ns-1", "app-ns-2"},
		},
		Endpoints: []MonitorEndpoint{
			{Port: "metrics"},
		},
	})

	if sm.Spec.NamespaceSelector == nil {
		t.Fatal("expected namespace selector")
	}
	if len(sm.Spec.NamespaceSelector.MatchNames) != 2 {
		t.Error("expected 2 namespace matches")
	}
}

func TestGenerateServiceMonitorAnyNamespace(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	sm := om.GenerateServiceMonitor(ServiceMonitorCfg{
		Name:     "test-monitor",
		Selector: map[string]string{"app": "test"},
		NamespaceSelector: &NamespaceSelector{
			Any: true,
		},
		Endpoints: []MonitorEndpoint{
			{Port: "metrics"},
		},
	})

	if sm.Spec.NamespaceSelector == nil || !sm.Spec.NamespaceSelector.Any {
		t.Error("expected any namespace selector")
	}
}

func TestGenerateServiceMonitorWithTLS(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	sm := om.GenerateServiceMonitor(ServiceMonitorCfg{
		Name:     "test-monitor",
		Selector: map[string]string{"app": "test"},
		Endpoints: []MonitorEndpoint{
			{
				Port:   "metrics",
				Scheme: "https",
				TLSConfig: &MonitorTLSConfig{
					CAFile:     "/etc/prometheus/ca.crt",
					ServerName: "test-service.default.svc",
				},
			},
		},
	})

	if sm.Spec.Endpoints[0].Scheme != "https" {
		t.Error("expected https scheme")
	}
	if sm.Spec.Endpoints[0].TLSConfig == nil {
		t.Fatal("expected TLS config")
	}
	if sm.Spec.Endpoints[0].TLSConfig.CAFile != "/etc/prometheus/ca.crt" {
		t.Error("expected CA file")
	}
}

func TestGenerateServiceMonitorWithBasicAuth(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	sm := om.GenerateServiceMonitor(ServiceMonitorCfg{
		Name:     "test-monitor",
		Selector: map[string]string{"app": "test"},
		Endpoints: []MonitorEndpoint{
			{
				Port: "metrics",
				BasicAuth: &MonitorBasicAuth{
					Username: SecretKeySelector{Name: "metrics-auth", Key: "username"},
					Password: SecretKeySelector{Name: "metrics-auth", Key: "password"},
				},
			},
		},
	})

	if sm.Spec.Endpoints[0].BasicAuth == nil {
		t.Fatal("expected basic auth")
	}
	if sm.Spec.Endpoints[0].BasicAuth.Username.Name != "metrics-auth" {
		t.Error("expected username secret name")
	}
}

func TestGenerateServiceMonitorWithRelabeling(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	sm := om.GenerateServiceMonitor(ServiceMonitorCfg{
		Name:     "test-monitor",
		Selector: map[string]string{"app": "test"},
		Endpoints: []MonitorEndpoint{
			{
				Port: "metrics",
				MetricRelabelings: []RelabelConfig{
					{
						SourceLabels: []string{"__name__"},
						Regex:        "expensive_metric.*",
						Action:       "drop",
					},
				},
			},
		},
	})

	if len(sm.Spec.Endpoints[0].MetricRelabelings) != 1 {
		t.Fatal("expected 1 metric relabeling")
	}
	if sm.Spec.Endpoints[0].MetricRelabelings[0].Action != "drop" {
		t.Error("expected drop action")
	}
}

func TestServiceMonitorToYAML(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	sm := om.GenerateServiceMonitor(ServiceMonitorCfg{
		Name:     "test-monitor",
		Selector: map[string]string{"app": "test"},
		Endpoints: []MonitorEndpoint{
			{Port: "metrics"},
		},
	})

	yaml, err := sm.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "kind: ServiceMonitor") {
		t.Error("expected YAML to contain kind")
	}
	if !strings.Contains(yaml, "monitoring.coreos.com/v1") {
		t.Error("expected YAML to contain apiVersion")
	}
}

func TestGeneratePodMonitor(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	pm := om.GeneratePodMonitor(PodMonitorCfg{
		Name:     "test-pod-monitor",
		Selector: map[string]string{"app": "test"},
		PodMetricsEndpoints: []PodMetricsEndpoint{
			{
				Port:     "metrics",
				Path:     "/metrics",
				Interval: "30s",
			},
		},
	})

	if pm.Kind != "PodMonitor" {
		t.Errorf("expected kind PodMonitor, got %s", pm.Kind)
	}
	if len(pm.Spec.PodMetricsEndpoints) != 1 {
		t.Error("expected 1 pod metrics endpoint")
	}
}

func TestPodMonitorToYAML(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	pm := om.GeneratePodMonitor(PodMonitorCfg{
		Name:     "test-pod-monitor",
		Selector: map[string]string{"app": "test"},
		PodMetricsEndpoints: []PodMetricsEndpoint{
			{Port: "metrics"},
		},
	})

	yaml, err := pm.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "kind: PodMonitor") {
		t.Error("expected YAML to contain kind")
	}
}

func TestGeneratePrometheusRule(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	pr := om.GeneratePrometheusRule(PrometheusRuleCfg{
		Name: "test-rules",
		Groups: []RuleGroup{
			{
				Name: "test.rules",
				Rules: []Rule{
					{
						Alert: "TestAlert",
						Expr:  "up == 0",
						For:   "5m",
						Labels: map[string]string{
							"severity": "critical",
						},
						Annotations: map[string]string{
							"summary": "Test alert fired",
						},
					},
				},
			},
		},
	})

	if pr.Kind != "PrometheusRule" {
		t.Errorf("expected kind PrometheusRule, got %s", pr.Kind)
	}
	if len(pr.Spec.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(pr.Spec.Groups))
	}
	if pr.Spec.Groups[0].Name != "test.rules" {
		t.Error("expected group name test.rules")
	}
	if len(pr.Spec.Groups[0].Rules) != 1 {
		t.Error("expected 1 rule")
	}
}

func TestGeneratePrometheusRuleRecording(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	pr := om.GeneratePrometheusRule(PrometheusRuleCfg{
		Name: "test-recording-rules",
		Groups: []RuleGroup{
			{
				Name:     "test.recording",
				Interval: "30s",
				Rules: []Rule{
					{
						Record: "job:http_requests:rate5m",
						Expr:   "sum(rate(http_requests_total[5m])) by (job)",
					},
				},
			},
		},
	})

	if pr.Spec.Groups[0].Rules[0].Record != "job:http_requests:rate5m" {
		t.Error("expected recording rule name")
	}
	if pr.Spec.Groups[0].Rules[0].Alert != "" {
		t.Error("recording rule should not have alert")
	}
}

func TestPrometheusRuleToYAML(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	pr := om.GeneratePrometheusRule(PrometheusRuleCfg{
		Name: "test-rules",
		Groups: []RuleGroup{
			{
				Name:  "test.rules",
				Rules: []Rule{{Alert: "Test", Expr: "up == 0"}},
			},
		},
	})

	yaml, err := pr.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "kind: PrometheusRule") {
		t.Error("expected YAML to contain kind")
	}
}

func TestGenerateGrafanaDashboard(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	dashboardJSON := `{"title": "Test Dashboard", "panels": []}`

	cm := om.GenerateGrafanaDashboard(GrafanaDashboardCfg{
		Name:   "test-dashboard",
		Folder: "Vaultaire",
		JSON:   dashboardJSON,
	})

	if cm.Kind != "ConfigMap" {
		t.Errorf("expected kind ConfigMap, got %s", cm.Kind)
	}
	if cm.Metadata.Labels["grafana_dashboard"] != "1" {
		t.Error("expected grafana_dashboard label")
	}
	if cm.Metadata.Annotations["grafana_folder"] != "Vaultaire" {
		t.Error("expected grafana_folder annotation")
	}
	if cm.Data["test-dashboard.json"] != dashboardJSON {
		t.Error("expected dashboard JSON")
	}
}

func TestGenerateVaultaireObservability(t *testing.T) {
	sm, pr := GenerateVaultaireObservability("vaultaire")

	if sm.Metadata.Name != "vaultaire-api" {
		t.Errorf("expected ServiceMonitor name 'vaultaire-api', got '%s'", sm.Metadata.Name)
	}
	if pr.Metadata.Name != "vaultaire-alerts" {
		t.Errorf("expected PrometheusRule name 'vaultaire-alerts', got '%s'", pr.Metadata.Name)
	}

	// Check that we have alert rules
	var hasAlertRules bool
	var hasRecordingRules bool
	for _, g := range pr.Spec.Groups {
		for _, r := range g.Rules {
			if r.Alert != "" {
				hasAlertRules = true
			}
			if r.Record != "" {
				hasRecordingRules = true
			}
		}
	}

	if !hasAlertRules {
		t.Error("expected alert rules")
	}
	if !hasRecordingRules {
		t.Error("expected recording rules")
	}
}

func TestObservabilitySet(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	set := &ObservabilitySet{
		ServiceMonitors: []*ServiceMonitorResource{
			om.GenerateServiceMonitor(ServiceMonitorCfg{
				Name:      "sm-1",
				Selector:  map[string]string{"app": "test"},
				Endpoints: []MonitorEndpoint{{Port: "metrics"}},
			}),
		},
		PodMonitors: []*PodMonitorResource{
			om.GeneratePodMonitor(PodMonitorCfg{
				Name:                "pm-1",
				Selector:            map[string]string{"app": "test"},
				PodMetricsEndpoints: []PodMetricsEndpoint{{Port: "metrics"}},
			}),
		},
		PrometheusRules: []*PrometheusRuleResource{
			om.GeneratePrometheusRule(PrometheusRuleCfg{
				Name:   "pr-1",
				Groups: []RuleGroup{{Name: "test", Rules: []Rule{{Alert: "Test", Expr: "up==0"}}}},
			}),
		},
	}

	yaml, err := set.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	docs := strings.Split(yaml, "---")
	if len(docs) != 3 {
		t.Errorf("expected 3 YAML documents, got %d", len(docs))
	}
}

func TestServiceMonitorNamespaceDefault(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	sm := om.GenerateServiceMonitor(ServiceMonitorCfg{
		Name:      "test-monitor",
		Selector:  map[string]string{"app": "test"},
		Endpoints: []MonitorEndpoint{{Port: "metrics"}},
	})

	if sm.Metadata.Namespace != "monitoring" {
		t.Errorf("expected namespace 'monitoring', got '%s'", sm.Metadata.Namespace)
	}
}

func TestServiceMonitorNamespaceOverride(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	sm := om.GenerateServiceMonitor(ServiceMonitorCfg{
		Name:      "test-monitor",
		Namespace: "custom",
		Selector:  map[string]string{"app": "test"},
		Endpoints: []MonitorEndpoint{{Port: "metrics"}},
	})

	if sm.Metadata.Namespace != "custom" {
		t.Errorf("expected namespace 'custom', got '%s'", sm.Metadata.Namespace)
	}
}

func TestServiceMonitorJobLabel(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	sm := om.GenerateServiceMonitor(ServiceMonitorCfg{
		Name:         "test-monitor",
		Selector:     map[string]string{"app": "test"},
		Endpoints:    []MonitorEndpoint{{Port: "metrics"}},
		JobLabel:     "app",
		TargetLabels: []string{"version", "environment"},
	})

	if sm.Spec.JobLabel != "app" {
		t.Error("expected jobLabel")
	}
	if len(sm.Spec.TargetLabels) != 2 {
		t.Error("expected 2 target labels")
	}
}

func TestServiceMonitorSampleLimit(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	sm := om.GenerateServiceMonitor(ServiceMonitorCfg{
		Name:        "test-monitor",
		Selector:    map[string]string{"app": "test"},
		Endpoints:   []MonitorEndpoint{{Port: "metrics"}},
		SampleLimit: 10000,
	})

	if sm.Spec.SampleLimit != 10000 {
		t.Errorf("expected sampleLimit 10000, got %d", sm.Spec.SampleLimit)
	}
}

func TestPrometheusRuleMultipleGroups(t *testing.T) {
	om := NewObservabilityManager("monitoring")

	pr := om.GeneratePrometheusRule(PrometheusRuleCfg{
		Name: "multi-group-rules",
		Groups: []RuleGroup{
			{
				Name:  "alerts",
				Rules: []Rule{{Alert: "Alert1", Expr: "up == 0"}},
			},
			{
				Name:  "recording",
				Rules: []Rule{{Record: "metric:rate5m", Expr: "rate(metric[5m])"}},
			},
		},
	})

	if len(pr.Spec.Groups) != 2 {
		t.Errorf("expected 2 groups, got %d", len(pr.Spec.Groups))
	}
}
