// internal/k8s/helm_test.go
package k8s

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNewHelmChartGenerator(t *testing.T) {
	gen := NewHelmChartGenerator("vaultaire", "1.0.0", "v1.0.0")

	if gen.Chart.Name != "vaultaire" {
		t.Errorf("expected name 'vaultaire', got '%s'", gen.Chart.Name)
	}
	if gen.Chart.Version != "1.0.0" {
		t.Errorf("expected version '1.0.0', got '%s'", gen.Chart.Version)
	}
	if gen.Chart.AppVersion != "v1.0.0" {
		t.Errorf("expected appVersion 'v1.0.0', got '%s'", gen.Chart.AppVersion)
	}
	if gen.Chart.Type != "application" {
		t.Errorf("expected type 'application', got '%s'", gen.Chart.Type)
	}
}

func TestDefaultHelmValues(t *testing.T) {
	values := DefaultHelmValues()

	if values.ReplicaCount != 1 {
		t.Errorf("expected replicaCount 1, got %d", values.ReplicaCount)
	}
	if values.Image.PullPolicy != "IfNotPresent" {
		t.Errorf("expected pullPolicy 'IfNotPresent', got '%s'", values.Image.PullPolicy)
	}
	if !values.ServiceAccount.Create {
		t.Error("expected serviceAccount.create to be true")
	}
	if values.Service.Type != "ClusterIP" {
		t.Errorf("expected service type 'ClusterIP', got '%s'", values.Service.Type)
	}
	if !values.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("expected readOnlyRootFilesystem to be true")
	}
}

func TestWithMaintainer(t *testing.T) {
	gen := NewHelmChartGenerator("test", "1.0.0", "v1.0.0")
	gen.WithMaintainer("John Doe", "john@example.com", "https://example.com")

	if len(gen.Chart.Maintainers) != 1 {
		t.Fatalf("expected 1 maintainer, got %d", len(gen.Chart.Maintainers))
	}
	if gen.Chart.Maintainers[0].Name != "John Doe" {
		t.Errorf("expected maintainer name 'John Doe', got '%s'", gen.Chart.Maintainers[0].Name)
	}
	if gen.Chart.Maintainers[0].Email != "john@example.com" {
		t.Errorf("expected maintainer email 'john@example.com', got '%s'", gen.Chart.Maintainers[0].Email)
	}
}

func TestWithDependency(t *testing.T) {
	gen := NewHelmChartGenerator("test", "1.0.0", "v1.0.0")
	gen.WithDependency("postgresql", "12.0.0", "https://charts.bitnami.com/bitnami")

	if len(gen.Chart.Dependencies) != 1 {
		t.Fatalf("expected 1 dependency, got %d", len(gen.Chart.Dependencies))
	}
	if gen.Chart.Dependencies[0].Name != "postgresql" {
		t.Errorf("expected dependency name 'postgresql', got '%s'", gen.Chart.Dependencies[0].Name)
	}
}

func TestWithKeywords(t *testing.T) {
	gen := NewHelmChartGenerator("test", "1.0.0", "v1.0.0")
	gen.WithKeywords("storage", "s3", "cloud")

	if len(gen.Chart.Keywords) != 3 {
		t.Fatalf("expected 3 keywords, got %d", len(gen.Chart.Keywords))
	}
}

func TestGenerateChartYAML(t *testing.T) {
	gen := NewHelmChartGenerator("vaultaire", "1.0.0", "v1.0.0")
	gen.WithMaintainer("FairForge", "support@fairforge.com", "")
	gen.WithKeywords("storage", "s3")

	yaml, err := gen.GenerateChartYAML()
	if err != nil {
		t.Fatalf("failed to generate Chart.yaml: %v", err)
	}

	if !strings.Contains(yaml, "apiVersion: v2") {
		t.Error("expected apiVersion v2")
	}
	if !strings.Contains(yaml, "name: vaultaire") {
		t.Error("expected name vaultaire")
	}
	if !strings.Contains(yaml, "version: 1.0.0") {
		t.Error("expected version 1.0.0")
	}
	if !strings.Contains(yaml, "appVersion: v1.0.0") {
		t.Error("expected appVersion v1.0.0")
	}
	if !strings.Contains(yaml, "type: application") {
		t.Error("expected type application")
	}
}

func TestGenerateValuesYAML(t *testing.T) {
	gen := NewHelmChartGenerator("vaultaire", "1.0.0", "v1.0.0")

	yaml, err := gen.GenerateValuesYAML()
	if err != nil {
		t.Fatalf("failed to generate values.yaml: %v", err)
	}

	if !strings.Contains(yaml, "replicaCount: 1") {
		t.Error("expected replicaCount in values")
	}
	if !strings.Contains(yaml, "repository: ghcr.io/fairforge/vaultaire") {
		t.Error("expected image repository in values")
	}
	if !strings.Contains(yaml, "pullPolicy: IfNotPresent") {
		t.Error("expected pullPolicy in values")
	}
}

func TestGenerateTemplates(t *testing.T) {
	gen := NewHelmChartGenerator("vaultaire", "1.0.0", "v1.0.0")

	templates := gen.GenerateTemplates()

	expectedTemplates := []string{
		"_helpers.tpl",
		"serviceaccount.yaml",
		"configmap.yaml",
		"secret.yaml",
		"deployment.yaml",
		"service.yaml",
		"ingress.yaml",
		"hpa.yaml",
		"pvc.yaml",
		"servicemonitor.yaml",
		"NOTES.txt",
	}

	if len(templates) != len(expectedTemplates) {
		t.Errorf("expected %d templates, got %d", len(expectedTemplates), len(templates))
	}

	templateNames := make(map[string]bool)
	for _, tpl := range templates {
		templateNames[tpl.Name] = true
	}

	for _, expected := range expectedTemplates {
		if !templateNames[expected] {
			t.Errorf("missing template: %s", expected)
		}
	}
}

func TestHelpersTpl(t *testing.T) {
	gen := NewHelmChartGenerator("vaultaire", "1.0.0", "v1.0.0")
	templates := gen.GenerateTemplates()

	var helpersTpl string
	for _, tpl := range templates {
		if tpl.Name == "_helpers.tpl" {
			helpersTpl = tpl.Content
			break
		}
	}

	if !strings.Contains(helpersTpl, "define \"vaultaire.name\"") {
		t.Error("expected vaultaire.name helper")
	}
	if !strings.Contains(helpersTpl, "define \"vaultaire.fullname\"") {
		t.Error("expected vaultaire.fullname helper")
	}
	if !strings.Contains(helpersTpl, "define \"vaultaire.labels\"") {
		t.Error("expected vaultaire.labels helper")
	}
	if !strings.Contains(helpersTpl, "define \"vaultaire.selectorLabels\"") {
		t.Error("expected vaultaire.selectorLabels helper")
	}
}

func TestDeploymentTpl(t *testing.T) {
	gen := NewHelmChartGenerator("vaultaire", "1.0.0", "v1.0.0")
	templates := gen.GenerateTemplates()

	var deploymentTpl string
	for _, tpl := range templates {
		if tpl.Name == "deployment.yaml" {
			deploymentTpl = tpl.Content
			break
		}
	}

	// Check for key sections
	if !strings.Contains(deploymentTpl, "kind: Deployment") {
		t.Error("expected kind Deployment")
	}
	if !strings.Contains(deploymentTpl, "securityContext") {
		t.Error("expected securityContext")
	}
	if !strings.Contains(deploymentTpl, "livenessProbe") {
		t.Error("expected livenessProbe")
	}
	if !strings.Contains(deploymentTpl, "readinessProbe") {
		t.Error("expected readinessProbe")
	}
	if !strings.Contains(deploymentTpl, "resources") {
		t.Error("expected resources")
	}
}

func TestServiceTpl(t *testing.T) {
	gen := NewHelmChartGenerator("vaultaire", "1.0.0", "v1.0.0")
	templates := gen.GenerateTemplates()

	var serviceTpl string
	for _, tpl := range templates {
		if tpl.Name == "service.yaml" {
			serviceTpl = tpl.Content
			break
		}
	}

	if !strings.Contains(serviceTpl, "kind: Service") {
		t.Error("expected kind Service")
	}
	if !strings.Contains(serviceTpl, ".Values.service.type") {
		t.Error("expected service type from values")
	}
	if !strings.Contains(serviceTpl, ".Values.service.port") {
		t.Error("expected service port from values")
	}
}

func TestIngressTpl(t *testing.T) {
	gen := NewHelmChartGenerator("vaultaire", "1.0.0", "v1.0.0")
	templates := gen.GenerateTemplates()

	var ingressTpl string
	for _, tpl := range templates {
		if tpl.Name == "ingress.yaml" {
			ingressTpl = tpl.Content
			break
		}
	}

	if !strings.Contains(ingressTpl, "{{- if .Values.ingress.enabled -}}") {
		t.Error("expected ingress enabled check")
	}
	if !strings.Contains(ingressTpl, "kind: Ingress") {
		t.Error("expected kind Ingress")
	}
	if !strings.Contains(ingressTpl, "networking.k8s.io/v1") {
		t.Error("expected networking.k8s.io/v1 apiVersion")
	}
}

func TestHPATpl(t *testing.T) {
	gen := NewHelmChartGenerator("vaultaire", "1.0.0", "v1.0.0")
	templates := gen.GenerateTemplates()

	var hpaTpl string
	for _, tpl := range templates {
		if tpl.Name == "hpa.yaml" {
			hpaTpl = tpl.Content
			break
		}
	}

	if !strings.Contains(hpaTpl, "{{- if .Values.autoscaling.enabled }}") {
		t.Error("expected autoscaling enabled check")
	}
	if !strings.Contains(hpaTpl, "kind: HorizontalPodAutoscaler") {
		t.Error("expected kind HorizontalPodAutoscaler")
	}
}

func TestValidateChart(t *testing.T) {
	tests := []struct {
		name        string
		chart       *HelmChartGenerator
		expectError bool
	}{
		{
			name:        "valid chart",
			chart:       NewHelmChartGenerator("test", "1.0.0", "v1.0.0"),
			expectError: false,
		},
		{
			name: "missing name",
			chart: &HelmChartGenerator{
				Chart: HelmChart{
					Version: "1.0.0",
				},
				Values: DefaultHelmValues(),
			},
			expectError: true,
		},
		{
			name: "missing version",
			chart: &HelmChartGenerator{
				Chart: HelmChart{
					Name: "test",
				},
				Values: DefaultHelmValues(),
			},
			expectError: true,
		},
		{
			name: "invalid semver",
			chart: &HelmChartGenerator{
				Chart: HelmChart{
					Name:    "test",
					Version: "invalid",
				},
				Values: DefaultHelmValues(),
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errors := tt.chart.ValidateChart()
			hasError := len(errors) > 0
			if hasError != tt.expectError {
				t.Errorf("expected error: %v, got errors: %v", tt.expectError, errors)
			}
		})
	}
}

func TestIsValidSemVer(t *testing.T) {
	tests := []struct {
		version string
		valid   bool
	}{
		{"1.0.0", true},
		{"0.1.0", true},
		{"1.0", true},
		{"1.0.0-alpha", true},
		{"1.0.0-beta.1", true},
		{"invalid", false},
		{"1.0.0.0", false},
		{"v1.0.0", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			result := isValidSemVer(tt.version)
			if result != tt.valid {
				t.Errorf("isValidSemVer(%s) = %v, want %v", tt.version, result, tt.valid)
			}
		})
	}
}

func TestWriteToDirectory(t *testing.T) {
	gen := NewHelmChartGenerator("vaultaire", "1.0.0", "v1.0.0")
	gen.WithMaintainer("FairForge", "support@fairforge.com", "")
	gen.WithKeywords("storage", "s3", "cloud")
	gen.WithDependency("postgresql", "12.0.0", "https://charts.bitnami.com/bitnami")

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "helm-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	// Write chart
	if err := gen.WriteToDirectory(tmpDir); err != nil {
		t.Fatalf("failed to write chart: %v", err)
	}

	// Verify files exist
	chartDir := filepath.Join(tmpDir, "vaultaire")

	expectedFiles := []string{
		"Chart.yaml",
		"values.yaml",
		".helmignore",
		"templates/_helpers.tpl",
		"templates/deployment.yaml",
		"templates/service.yaml",
		"templates/serviceaccount.yaml",
		"templates/configmap.yaml",
		"templates/secret.yaml",
		"templates/ingress.yaml",
		"templates/hpa.yaml",
		"templates/pvc.yaml",
		"templates/servicemonitor.yaml",
		"templates/NOTES.txt",
	}

	for _, file := range expectedFiles {
		path := filepath.Join(chartDir, file)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected file %s to exist", file)
		}
	}

	// Verify Chart.yaml content
	chartYAML, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		t.Fatalf("failed to read Chart.yaml: %v", err)
	}

	var chart ChartYAML
	if err := yaml.Unmarshal(chartYAML, &chart); err != nil {
		t.Fatalf("failed to parse Chart.yaml: %v", err)
	}

	if chart.Name != "vaultaire" {
		t.Errorf("expected chart name 'vaultaire', got '%s'", chart.Name)
	}
	if len(chart.Dependencies) != 1 {
		t.Errorf("expected 1 dependency, got %d", len(chart.Dependencies))
	}
}

func TestValuesYAMLParseable(t *testing.T) {
	gen := NewHelmChartGenerator("vaultaire", "1.0.0", "v1.0.0")

	yamlContent, err := gen.GenerateValuesYAML()
	if err != nil {
		t.Fatalf("failed to generate values.yaml: %v", err)
	}

	var parsed map[string]interface{}
	if err := yaml.Unmarshal([]byte(yamlContent), &parsed); err != nil {
		t.Fatalf("values.yaml is not valid YAML: %v", err)
	}

	// Check key sections exist
	if _, ok := parsed["replicaCount"]; !ok {
		t.Error("missing replicaCount")
	}
	if _, ok := parsed["image"]; !ok {
		t.Error("missing image")
	}
	if _, ok := parsed["service"]; !ok {
		t.Error("missing service")
	}
	if _, ok := parsed["ingress"]; !ok {
		t.Error("missing ingress")
	}
	if _, ok := parsed["resources"]; !ok {
		t.Error("missing resources")
	}
}

func TestChartYAMLParseable(t *testing.T) {
	gen := NewHelmChartGenerator("vaultaire", "1.0.0", "v1.0.0")
	gen.WithMaintainer("Test", "test@test.com", "")
	gen.WithDependency("redis", "17.0.0", "https://charts.bitnami.com/bitnami")

	yamlContent, err := gen.GenerateChartYAML()
	if err != nil {
		t.Fatalf("failed to generate Chart.yaml: %v", err)
	}

	var parsed ChartYAML
	if err := yaml.Unmarshal([]byte(yamlContent), &parsed); err != nil {
		t.Fatalf("Chart.yaml is not valid YAML: %v", err)
	}

	if parsed.APIVersion != "v2" {
		t.Errorf("expected apiVersion v2, got %s", parsed.APIVersion)
	}
	if len(parsed.Maintainers) != 1 {
		t.Errorf("expected 1 maintainer, got %d", len(parsed.Maintainers))
	}
	if len(parsed.Dependencies) != 1 {
		t.Errorf("expected 1 dependency, got %d", len(parsed.Dependencies))
	}
}

func TestSecurityContextValues(t *testing.T) {
	values := DefaultHelmValues()

	// Verify security best practices
	if values.SecurityContext.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation should be false")
	}
	if !values.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem should be true")
	}
	if !values.SecurityContext.RunAsNonRoot {
		t.Error("runAsNonRoot should be true")
	}
	if len(values.SecurityContext.Capabilities.Drop) == 0 {
		t.Error("should drop ALL capabilities")
	}
	if values.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Error("should drop ALL capabilities")
	}
}

func TestProbesConfig(t *testing.T) {
	values := DefaultHelmValues()

	if !values.Probes.Liveness.Enabled {
		t.Error("liveness probe should be enabled by default")
	}
	if !values.Probes.Readiness.Enabled {
		t.Error("readiness probe should be enabled by default")
	}
	if values.Probes.Liveness.Path != "/health" {
		t.Errorf("expected liveness path '/health', got '%s'", values.Probes.Liveness.Path)
	}
	if values.Probes.Liveness.InitialDelaySeconds != 30 {
		t.Errorf("expected liveness initialDelaySeconds 30, got %d", values.Probes.Liveness.InitialDelaySeconds)
	}
}

func TestAutoscalingConfig(t *testing.T) {
	values := DefaultHelmValues()

	if values.Autoscaling.Enabled {
		t.Error("autoscaling should be disabled by default")
	}
	if values.Autoscaling.MinReplicas != 1 {
		t.Errorf("expected minReplicas 1, got %d", values.Autoscaling.MinReplicas)
	}
	if values.Autoscaling.MaxReplicas != 10 {
		t.Errorf("expected maxReplicas 10, got %d", values.Autoscaling.MaxReplicas)
	}
}

func TestDatabaseSubcharts(t *testing.T) {
	values := DefaultHelmValues()

	// PostgreSQL
	if !values.PostgreSQL.Enabled {
		t.Error("postgresql should be enabled by default")
	}
	if values.PostgreSQL.Auth.Database != "vaultaire" {
		t.Errorf("expected database 'vaultaire', got '%s'", values.PostgreSQL.Auth.Database)
	}

	// Redis
	if !values.Redis.Enabled {
		t.Error("redis should be enabled by default")
	}
	if !values.Redis.Auth.Enabled {
		t.Error("redis auth should be enabled")
	}
}

func TestIngressConfig(t *testing.T) {
	values := DefaultHelmValues()

	if values.Ingress.Enabled {
		t.Error("ingress should be disabled by default")
	}
	if len(values.Ingress.Hosts) == 0 {
		t.Error("should have default host configuration")
	}
	if values.Ingress.Hosts[0].Host != "vaultaire.local" {
		t.Errorf("expected default host 'vaultaire.local', got '%s'", values.Ingress.Hosts[0].Host)
	}
}

func TestMetricsConfig(t *testing.T) {
	values := DefaultHelmValues()

	if !values.Metrics.Enabled {
		t.Error("metrics should be enabled by default")
	}
	if values.Metrics.Port != 9090 {
		t.Errorf("expected metrics port 9090, got %d", values.Metrics.Port)
	}
	if values.Metrics.ServiceMonitor.Enabled {
		t.Error("serviceMonitor should be disabled by default")
	}
}

func TestFluentChaining(t *testing.T) {
	gen := NewHelmChartGenerator("test", "1.0.0", "v1.0.0").
		WithMaintainer("A", "a@a.com", "").
		WithMaintainer("B", "b@b.com", "").
		WithDependency("dep1", "1.0.0", "http://example.com").
		WithKeywords("k1", "k2")

	if len(gen.Chart.Maintainers) != 2 {
		t.Error("expected 2 maintainers")
	}
	if len(gen.Chart.Dependencies) != 1 {
		t.Error("expected 1 dependency")
	}
	if len(gen.Chart.Keywords) != 2 {
		t.Error("expected 2 keywords")
	}
}

func TestHelmFuncMap(t *testing.T) {
	funcs := helmFuncMap()

	// Test quote
	quoteFunc := funcs["quote"].(func(string) string)
	if quoteFunc("test") != `"test"` {
		t.Error("quote function failed")
	}

	// Test contains
	containsFunc := funcs["contains"].(func(string, string) bool)
	if !containsFunc("hello", "helloworld") {
		t.Error("contains function failed")
	}

	// Test trunc
	truncFunc := funcs["trunc"].(func(int, string) string)
	if truncFunc(3, "hello") != "hel" {
		t.Error("trunc function failed")
	}

	// Test trimSuffix
	trimSuffixFunc := funcs["trimSuffix"].(func(string, string) string)
	if trimSuffixFunc("-test", "hello-test") != "hello" {
		t.Error("trimSuffix function failed")
	}
}
