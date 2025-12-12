// internal/k8s/autoscaling_test.go
package k8s

import (
	"strings"
	"testing"
)

func TestNewAutoscalingManager(t *testing.T) {
	am := NewAutoscalingManager("default")

	if am.namespace != "default" {
		t.Errorf("expected namespace 'default', got '%s'", am.namespace)
	}
}

func TestGenerateHPABasic(t *testing.T) {
	am := NewAutoscalingManager("default")

	cpu := int32(80)
	hpa := am.GenerateHPA(HPAConfig{
		Name:        "test-hpa",
		TargetName:  "test-deployment",
		MinReplicas: 2,
		MaxReplicas: 10,
		Metrics: []MetricSpec{
			{
				Type: MetricTypeResource,
				Resource: &ResourceMetricSource{
					Name: "cpu",
					Target: MetricTarget{
						Type:               MetricTargetTypeUtilization,
						AverageUtilization: &cpu,
					},
				},
			},
		},
	})

	if hpa.Kind != "HorizontalPodAutoscaler" {
		t.Errorf("expected kind HorizontalPodAutoscaler, got %s", hpa.Kind)
	}
	if hpa.APIVersion != "autoscaling/v2" {
		t.Errorf("expected apiVersion autoscaling/v2, got %s", hpa.APIVersion)
	}
	if hpa.Spec.MaxReplicas != 10 {
		t.Errorf("expected maxReplicas 10, got %d", hpa.Spec.MaxReplicas)
	}
	if *hpa.Spec.MinReplicas != 2 {
		t.Errorf("expected minReplicas 2, got %d", *hpa.Spec.MinReplicas)
	}
}

func TestGenerateHPADefaults(t *testing.T) {
	am := NewAutoscalingManager("production")

	hpa := am.GenerateHPA(HPAConfig{
		Name:        "test-hpa",
		TargetName:  "test-deployment",
		MaxReplicas: 5,
	})

	if hpa.Metadata.Namespace != "production" {
		t.Errorf("expected namespace 'production', got '%s'", hpa.Metadata.Namespace)
	}
	if hpa.Spec.ScaleTargetRef.Kind != "Deployment" {
		t.Errorf("expected default kind 'Deployment', got '%s'", hpa.Spec.ScaleTargetRef.Kind)
	}
	if hpa.Spec.ScaleTargetRef.APIVersion != "apps/v1" {
		t.Errorf("expected default apiVersion 'apps/v1', got '%s'", hpa.Spec.ScaleTargetRef.APIVersion)
	}
}

func TestGenerateHPAWithBehavior(t *testing.T) {
	am := NewAutoscalingManager("default")

	scaleUpWindow := int32(0)
	scaleDownWindow := int32(300)

	hpa := am.GenerateHPA(HPAConfig{
		Name:        "test-hpa",
		TargetName:  "test-deployment",
		MaxReplicas: 10,
		Behavior: &HPABehavior{
			ScaleUp: &HPAScalingRules{
				StabilizationWindowSeconds: &scaleUpWindow,
				SelectPolicy:               "Max",
				Policies: []HPAScalingPolicy{
					{Type: "Pods", Value: 4, PeriodSeconds: 15},
					{Type: "Percent", Value: 100, PeriodSeconds: 15},
				},
			},
			ScaleDown: &HPAScalingRules{
				StabilizationWindowSeconds: &scaleDownWindow,
				SelectPolicy:               "Min",
				Policies: []HPAScalingPolicy{
					{Type: "Pods", Value: 1, PeriodSeconds: 60},
				},
			},
		},
	})

	if hpa.Spec.Behavior == nil {
		t.Fatal("expected behavior to be set")
	}
	if *hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds != 0 {
		t.Error("expected scale up stabilization window 0")
	}
	if *hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds != 300 {
		t.Error("expected scale down stabilization window 300")
	}
	if len(hpa.Spec.Behavior.ScaleUp.Policies) != 2 {
		t.Errorf("expected 2 scale up policies, got %d", len(hpa.Spec.Behavior.ScaleUp.Policies))
	}
}

func TestHPAToYAML(t *testing.T) {
	am := NewAutoscalingManager("default")

	hpa := am.GenerateHPA(HPAConfig{
		Name:        "test-hpa",
		TargetName:  "test-deployment",
		MaxReplicas: 10,
	})

	yaml, err := hpa.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "kind: HorizontalPodAutoscaler") {
		t.Error("expected YAML to contain kind")
	}
	if !strings.Contains(yaml, "autoscaling/v2") {
		t.Error("expected YAML to contain apiVersion")
	}
}

func TestHPABuilder(t *testing.T) {
	hpa := NewHPABuilder("test-hpa", "default", "test-deployment").
		WithMinReplicas(2).
		WithMaxReplicas(10).
		WithCPUUtilization(70).
		WithMemoryUtilization(80).
		Build()

	if hpa.Metadata.Name != "test-hpa" {
		t.Errorf("expected name 'test-hpa', got '%s'", hpa.Metadata.Name)
	}
	if *hpa.Spec.MinReplicas != 2 {
		t.Errorf("expected minReplicas 2, got %d", *hpa.Spec.MinReplicas)
	}
	if len(hpa.Spec.Metrics) != 2 {
		t.Errorf("expected 2 metrics, got %d", len(hpa.Spec.Metrics))
	}
}

func TestHPABuilderWithCustomMetric(t *testing.T) {
	hpa := NewHPABuilder("test-hpa", "default", "test-deployment").
		WithMaxReplicas(10).
		WithCustomMetric("requests_per_second", "100").
		Build()

	if len(hpa.Spec.Metrics) != 1 {
		t.Fatal("expected 1 metric")
	}
	if hpa.Spec.Metrics[0].Type != string(MetricTypePods) {
		t.Errorf("expected Pods metric type, got %s", hpa.Spec.Metrics[0].Type)
	}
}

func TestHPABuilderWithExternalMetric(t *testing.T) {
	hpa := NewHPABuilder("test-hpa", "default", "test-deployment").
		WithMaxReplicas(10).
		WithExternalMetric("pubsub_messages", "100").
		Build()

	if len(hpa.Spec.Metrics) != 1 {
		t.Fatal("expected 1 metric")
	}
	if hpa.Spec.Metrics[0].Type != string(MetricTypeExternal) {
		t.Errorf("expected External metric type, got %s", hpa.Spec.Metrics[0].Type)
	}
}

func TestHPABuilderWithBehavior(t *testing.T) {
	hpa := NewHPABuilder("test-hpa", "default", "test-deployment").
		WithMaxReplicas(10).
		WithScaleUpPolicy("Pods", 4, 15).
		WithScaleUpPolicy("Percent", 100, 15).
		WithScaleDownPolicy("Pods", 1, 60).
		WithScaleUpStabilization(0).
		WithScaleDownStabilization(300).
		Build()

	if hpa.Spec.Behavior == nil {
		t.Fatal("expected behavior to be set")
	}
	if len(hpa.Spec.Behavior.ScaleUp.Policies) != 2 {
		t.Error("expected 2 scale up policies")
	}
	if len(hpa.Spec.Behavior.ScaleDown.Policies) != 1 {
		t.Error("expected 1 scale down policy")
	}
}

func TestGenerateVPA(t *testing.T) {
	am := NewAutoscalingManager("default")

	vpa := am.GenerateVPA(VPAConfig{
		Name:       "test-vpa",
		TargetName: "test-deployment",
		UpdateMode: "Auto",
		ContainerPolicies: []VPAContainerPolicy{
			{
				ContainerName: "app",
				MinAllowed:    ResourceList{CPU: "100m", Memory: "128Mi"},
				MaxAllowed:    ResourceList{CPU: "4", Memory: "8Gi"},
			},
		},
	})

	if vpa.Kind != "VerticalPodAutoscaler" {
		t.Errorf("expected kind VerticalPodAutoscaler, got %s", vpa.Kind)
	}
	if vpa.APIVersion != "autoscaling.k8s.io/v1" {
		t.Errorf("expected apiVersion autoscaling.k8s.io/v1, got %s", vpa.APIVersion)
	}
	if vpa.Spec.UpdatePolicy.UpdateMode != "Auto" {
		t.Errorf("expected updateMode Auto, got %s", vpa.Spec.UpdatePolicy.UpdateMode)
	}
}

func TestGenerateVPADefaults(t *testing.T) {
	am := NewAutoscalingManager("production")

	vpa := am.GenerateVPA(VPAConfig{
		Name:       "test-vpa",
		TargetName: "test-deployment",
	})

	if vpa.Metadata.Namespace != "production" {
		t.Errorf("expected namespace 'production', got '%s'", vpa.Metadata.Namespace)
	}
	if vpa.Spec.UpdatePolicy.UpdateMode != "Auto" {
		t.Errorf("expected default updateMode 'Auto', got '%s'", vpa.Spec.UpdatePolicy.UpdateMode)
	}
}

func TestGenerateVPAWithContainerPolicies(t *testing.T) {
	am := NewAutoscalingManager("default")

	vpa := am.GenerateVPA(VPAConfig{
		Name:       "test-vpa",
		TargetName: "test-deployment",
		ContainerPolicies: []VPAContainerPolicy{
			{
				ContainerName:       "app",
				Mode:                "Auto",
				MinAllowed:          ResourceList{CPU: "100m", Memory: "128Mi"},
				MaxAllowed:          ResourceList{CPU: "4", Memory: "8Gi"},
				ControlledResources: []string{"cpu", "memory"},
				ControlledValues:    "RequestsAndLimits",
			},
		},
	})

	if vpa.Spec.ResourcePolicy == nil {
		t.Fatal("expected resource policy")
	}
	if len(vpa.Spec.ResourcePolicy.ContainerPolicies) != 1 {
		t.Fatal("expected 1 container policy")
	}

	cp := vpa.Spec.ResourcePolicy.ContainerPolicies[0]
	if cp.ContainerName != "app" {
		t.Error("expected container name 'app'")
	}
	if cp.MinAllowed["cpu"] != "100m" {
		t.Error("expected minAllowed cpu '100m'")
	}
	if cp.MaxAllowed["memory"] != "8Gi" {
		t.Error("expected maxAllowed memory '8Gi'")
	}
}

func TestVPAToYAML(t *testing.T) {
	am := NewAutoscalingManager("default")

	vpa := am.GenerateVPA(VPAConfig{
		Name:       "test-vpa",
		TargetName: "test-deployment",
	})

	yaml, err := vpa.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "kind: VerticalPodAutoscaler") {
		t.Error("expected YAML to contain kind")
	}
}

func TestGeneratePDB(t *testing.T) {
	am := NewAutoscalingManager("default")

	pdb := am.GeneratePDB(PDBConfig{
		Name:         "test-pdb",
		Selector:     map[string]string{"app": "test"},
		MinAvailable: "50%",
	})

	if pdb.Kind != "PodDisruptionBudget" {
		t.Errorf("expected kind PodDisruptionBudget, got %s", pdb.Kind)
	}
	if pdb.APIVersion != "policy/v1" {
		t.Errorf("expected apiVersion policy/v1, got %s", pdb.APIVersion)
	}
	if pdb.Spec.MinAvailable != "50%" {
		t.Errorf("expected minAvailable '50%%', got '%s'", pdb.Spec.MinAvailable)
	}
}

func TestGeneratePDBMaxUnavailable(t *testing.T) {
	am := NewAutoscalingManager("default")

	pdb := am.GeneratePDB(PDBConfig{
		Name:           "test-pdb",
		Selector:       map[string]string{"app": "test"},
		MaxUnavailable: "1",
	})

	if pdb.Spec.MaxUnavailable != "1" {
		t.Errorf("expected maxUnavailable '1', got '%s'", pdb.Spec.MaxUnavailable)
	}
}

func TestPDBToYAML(t *testing.T) {
	am := NewAutoscalingManager("default")

	pdb := am.GeneratePDB(PDBConfig{
		Name:         "test-pdb",
		Selector:     map[string]string{"app": "test"},
		MinAvailable: "2",
	})

	yaml, err := pdb.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "kind: PodDisruptionBudget") {
		t.Error("expected YAML to contain kind")
	}
	if !strings.Contains(yaml, "policy/v1") {
		t.Error("expected YAML to contain apiVersion")
	}
}

func TestGenerateVaultaireAutoscaling(t *testing.T) {
	hpa, vpa, pdb := GenerateVaultaireAutoscaling("vaultaire")

	// Check HPA
	if hpa.Metadata.Name != "vaultaire-api" {
		t.Errorf("expected HPA name 'vaultaire-api', got '%s'", hpa.Metadata.Name)
	}
	if *hpa.Spec.MinReplicas != 2 {
		t.Error("expected minReplicas 2")
	}
	if hpa.Spec.MaxReplicas != 20 {
		t.Error("expected maxReplicas 20")
	}
	if len(hpa.Spec.Metrics) != 2 {
		t.Error("expected 2 metrics (cpu and memory)")
	}

	// Check VPA
	if vpa.Metadata.Name != "vaultaire-worker" {
		t.Errorf("expected VPA name 'vaultaire-worker', got '%s'", vpa.Metadata.Name)
	}
	if vpa.Spec.UpdatePolicy.UpdateMode != "Off" {
		t.Error("expected updateMode Off (recommendation only)")
	}

	// Check PDB
	if pdb.Metadata.Name != "vaultaire-api" {
		t.Errorf("expected PDB name 'vaultaire-api', got '%s'", pdb.Metadata.Name)
	}
	if pdb.Spec.MinAvailable != "50%" {
		t.Error("expected minAvailable 50%")
	}
}

func TestAutoscalingSet(t *testing.T) {
	am := NewAutoscalingManager("default")

	set := &AutoscalingSet{
		HPAs: []*HPAResource{
			am.GenerateHPA(HPAConfig{Name: "hpa-1", TargetName: "dep-1", MaxReplicas: 5}),
		},
		VPAs: []*VPAResource{
			am.GenerateVPA(VPAConfig{Name: "vpa-1", TargetName: "dep-1"}),
		},
		PDBs: []*PDBResource{
			am.GeneratePDB(PDBConfig{Name: "pdb-1", Selector: map[string]string{"app": "app1"}, MinAvailable: "1"}),
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

func TestHPAMultipleMetrics(t *testing.T) {
	am := NewAutoscalingManager("default")

	cpu := int32(70)
	memory := int32(80)

	hpa := am.GenerateHPA(HPAConfig{
		Name:        "test-hpa",
		TargetName:  "test-deployment",
		MaxReplicas: 10,
		Metrics: []MetricSpec{
			{
				Type: MetricTypeResource,
				Resource: &ResourceMetricSource{
					Name: "cpu",
					Target: MetricTarget{
						Type:               MetricTargetTypeUtilization,
						AverageUtilization: &cpu,
					},
				},
			},
			{
				Type: MetricTypeResource,
				Resource: &ResourceMetricSource{
					Name: "memory",
					Target: MetricTarget{
						Type:               MetricTargetTypeUtilization,
						AverageUtilization: &memory,
					},
				},
			},
			{
				Type: MetricTypePods,
				Pods: &PodsMetricSource{
					Metric: MetricIdentifier{Name: "requests_per_second"},
					Target: MetricTarget{
						Type:         MetricTargetTypeAverageValue,
						AverageValue: "1000",
					},
				},
			},
		},
	})

	if len(hpa.Spec.Metrics) != 3 {
		t.Errorf("expected 3 metrics, got %d", len(hpa.Spec.Metrics))
	}
}

func TestHPAContainerResource(t *testing.T) {
	am := NewAutoscalingManager("default")

	cpu := int32(80)
	hpa := am.GenerateHPA(HPAConfig{
		Name:        "test-hpa",
		TargetName:  "test-deployment",
		MaxReplicas: 10,
		Metrics: []MetricSpec{
			{
				Type: MetricTypeContainerResource,
				ContainerResource: &ContainerResourceMetricSource{
					Name:      "cpu",
					Container: "app",
					Target: MetricTarget{
						Type:               MetricTargetTypeUtilization,
						AverageUtilization: &cpu,
					},
				},
			},
		},
	})

	if len(hpa.Spec.Metrics) != 1 {
		t.Fatal("expected 1 metric")
	}
	if hpa.Spec.Metrics[0].ContainerResource == nil {
		t.Fatal("expected container resource metric")
	}
	if hpa.Spec.Metrics[0].ContainerResource.Container != "app" {
		t.Error("expected container 'app'")
	}
}
