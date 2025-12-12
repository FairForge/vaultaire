// internal/k8s/resources_test.go
package k8s

import (
	"strings"
	"testing"
)

func TestNewResourceManager(t *testing.T) {
	rm := NewResourceManager("default")

	if rm.namespace != "default" {
		t.Errorf("expected namespace 'default', got '%s'", rm.namespace)
	}
}

func TestGenerateLimitRange(t *testing.T) {
	rm := NewResourceManager("default")

	lr := rm.GenerateLimitRange(LimitRangeConfig{
		Name: "test-limits",
		Limits: []LimitRangeItem{
			{
				Type: "Container",
				Default: ResourceQuantities{
					CPU:    "500m",
					Memory: "512Mi",
				},
				DefaultRequest: ResourceQuantities{
					CPU:    "100m",
					Memory: "128Mi",
				},
				Max: ResourceQuantities{
					CPU:    "4",
					Memory: "8Gi",
				},
				Min: ResourceQuantities{
					CPU:    "10m",
					Memory: "16Mi",
				},
			},
		},
	})

	if lr.Kind != "LimitRange" {
		t.Errorf("expected kind LimitRange, got %s", lr.Kind)
	}
	if lr.APIVersion != "v1" {
		t.Errorf("expected apiVersion v1, got %s", lr.APIVersion)
	}
	if len(lr.Spec.Limits) != 1 {
		t.Fatalf("expected 1 limit, got %d", len(lr.Spec.Limits))
	}
	if lr.Spec.Limits[0].Type != "Container" {
		t.Error("expected type Container")
	}
	if lr.Spec.Limits[0].Default["cpu"] != "500m" {
		t.Error("expected default cpu 500m")
	}
}

func TestGenerateLimitRangeMultipleTypes(t *testing.T) {
	rm := NewResourceManager("default")

	lr := rm.GenerateLimitRange(LimitRangeConfig{
		Name: "test-limits",
		Limits: []LimitRangeItem{
			{
				Type:    "Container",
				Default: ResourceQuantities{CPU: "500m", Memory: "512Mi"},
			},
			{
				Type: "Pod",
				Max:  ResourceQuantities{CPU: "8", Memory: "16Gi"},
			},
			{
				Type: "PersistentVolumeClaim",
				Max:  ResourceQuantities{EphemeralStorage: "100Gi"},
			},
		},
	})

	if len(lr.Spec.Limits) != 3 {
		t.Errorf("expected 3 limits, got %d", len(lr.Spec.Limits))
	}
}

func TestLimitRangeToYAML(t *testing.T) {
	rm := NewResourceManager("default")

	lr := rm.GenerateLimitRange(LimitRangeConfig{
		Name: "test-limits",
		Limits: []LimitRangeItem{
			{
				Type:    "Container",
				Default: ResourceQuantities{CPU: "500m"},
			},
		},
	})

	yaml, err := lr.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "kind: LimitRange") {
		t.Error("expected YAML to contain kind")
	}
}

func TestGenerateResourceQuota(t *testing.T) {
	rm := NewResourceManager("default")

	rq := rm.GenerateResourceQuota(ResourceQuotaConfig{
		Name: "test-quota",
		Hard: ResourceQuotaSpec{
			RequestsCPU:    "10",
			RequestsMemory: "20Gi",
			LimitsCPU:      "20",
			LimitsMemory:   "40Gi",
			Pods:           "50",
			Services:       "10",
		},
	})

	if rq.Kind != "ResourceQuota" {
		t.Errorf("expected kind ResourceQuota, got %s", rq.Kind)
	}
	if rq.APIVersion != "v1" {
		t.Errorf("expected apiVersion v1, got %s", rq.APIVersion)
	}
	if rq.Spec.Hard["requests.cpu"] != "10" {
		t.Error("expected requests.cpu 10")
	}
	if rq.Spec.Hard["pods"] != "50" {
		t.Error("expected pods 50")
	}
}

func TestGenerateResourceQuotaWithStorage(t *testing.T) {
	rm := NewResourceManager("default")

	rq := rm.GenerateResourceQuota(ResourceQuotaConfig{
		Name: "test-quota",
		Hard: ResourceQuotaSpec{
			RequestsStorage:        "100Gi",
			PersistentVolumeClaims: "10",
			StorageClassName: map[string]string{
				"fast-ssd": "50Gi",
				"standard": "100Gi",
			},
		},
	})

	if rq.Spec.Hard["requests.storage"] != "100Gi" {
		t.Error("expected requests.storage 100Gi")
	}
	if rq.Spec.Hard["fast-ssd.storageclass.storage.k8s.io/requests.storage"] != "50Gi" {
		t.Error("expected storage class quota")
	}
}

func TestGenerateResourceQuotaWithScopes(t *testing.T) {
	rm := NewResourceManager("default")

	rq := rm.GenerateResourceQuota(ResourceQuotaConfig{
		Name:   "test-quota",
		Scopes: []string{"BestEffort", "NotTerminating"},
		Hard: ResourceQuotaSpec{
			Pods: "10",
		},
	})

	if len(rq.Spec.Scopes) != 2 {
		t.Errorf("expected 2 scopes, got %d", len(rq.Spec.Scopes))
	}
}

func TestGenerateResourceQuotaWithScopeSelector(t *testing.T) {
	rm := NewResourceManager("default")

	rq := rm.GenerateResourceQuota(ResourceQuotaConfig{
		Name: "test-quota",
		ScopeSelector: &ScopeSelector{
			MatchExpressions: []ScopeSelectorRequirement{
				{
					ScopeName: "PriorityClass",
					Operator:  "In",
					Values:    []string{"high", "medium"},
				},
			},
		},
		Hard: ResourceQuotaSpec{
			Pods: "20",
		},
	})

	if rq.Spec.ScopeSelector == nil {
		t.Fatal("expected scope selector")
	}
	if len(rq.Spec.ScopeSelector.MatchExpressions) != 1 {
		t.Error("expected 1 match expression")
	}
}

func TestResourceQuotaToYAML(t *testing.T) {
	rm := NewResourceManager("default")

	rq := rm.GenerateResourceQuota(ResourceQuotaConfig{
		Name: "test-quota",
		Hard: ResourceQuotaSpec{Pods: "50"},
	})

	yaml, err := rq.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "kind: ResourceQuota") {
		t.Error("expected YAML to contain kind")
	}
}

func TestGeneratePriorityClass(t *testing.T) {
	rm := NewResourceManager("")

	pc := rm.GeneratePriorityClass(PriorityClassConfig{
		Name:             "high-priority",
		Value:            100000,
		GlobalDefault:    false,
		PreemptionPolicy: "PreemptLowerPriority",
		Description:      "High priority workloads",
	})

	if pc.Kind != "PriorityClass" {
		t.Errorf("expected kind PriorityClass, got %s", pc.Kind)
	}
	if pc.APIVersion != "scheduling.k8s.io/v1" {
		t.Errorf("expected apiVersion scheduling.k8s.io/v1, got %s", pc.APIVersion)
	}
	if pc.Value != 100000 {
		t.Errorf("expected value 100000, got %d", pc.Value)
	}
	if pc.PreemptionPolicy != "PreemptLowerPriority" {
		t.Error("expected preemptionPolicy PreemptLowerPriority")
	}
}

func TestPriorityClassToYAML(t *testing.T) {
	rm := NewResourceManager("")

	pc := rm.GeneratePriorityClass(PriorityClassConfig{
		Name:  "test-priority",
		Value: 1000,
	})

	yaml, err := pc.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "kind: PriorityClass") {
		t.Error("expected YAML to contain kind")
	}
}

func TestStandardResourceProfiles(t *testing.T) {
	profiles := StandardResourceProfiles()

	expectedProfiles := []string{"tiny", "small", "medium", "large", "xlarge", "memory-optimized", "cpu-optimized", "gpu"}

	for _, name := range expectedProfiles {
		if _, ok := profiles[name]; !ok {
			t.Errorf("expected profile '%s' to exist", name)
		}
	}
}

func TestGetResourceProfile(t *testing.T) {
	profile, ok := GetResourceProfile("medium")
	if !ok {
		t.Fatal("expected medium profile to exist")
	}
	if profile.Requests["cpu"] != "250m" {
		t.Error("expected medium profile requests.cpu 250m")
	}
	if profile.Limits["memory"] != "1Gi" {
		t.Error("expected medium profile limits.memory 1Gi")
	}
}

func TestGetResourceProfileNotFound(t *testing.T) {
	_, ok := GetResourceProfile("nonexistent")
	if ok {
		t.Error("expected profile to not exist")
	}
}

func TestResourceQuantitiesToMap(t *testing.T) {
	rq := ResourceQuantities{
		CPU:       "500m",
		Memory:    "512Mi",
		NvidiaGPU: "1",
	}

	m := resourceQuantitiesToMap(rq)

	if m["cpu"] != "500m" {
		t.Error("expected cpu 500m")
	}
	if m["memory"] != "512Mi" {
		t.Error("expected memory 512Mi")
	}
	if m["nvidia.com/gpu"] != "1" {
		t.Error("expected nvidia.com/gpu 1")
	}
}

func TestResourceQuantitiesToMapEmpty(t *testing.T) {
	rq := ResourceQuantities{}
	m := resourceQuantitiesToMap(rq)

	if m != nil {
		t.Error("expected nil map for empty quantities")
	}
}

func TestGenerateVaultaireLimitRange(t *testing.T) {
	lr := GenerateVaultaireLimitRange("vaultaire")

	if lr.Metadata.Name != "vaultaire-limits" {
		t.Errorf("expected name 'vaultaire-limits', got '%s'", lr.Metadata.Name)
	}
	if lr.Metadata.Namespace != "vaultaire" {
		t.Errorf("expected namespace 'vaultaire', got '%s'", lr.Metadata.Namespace)
	}
	if len(lr.Spec.Limits) < 1 {
		t.Error("expected at least 1 limit")
	}
}

func TestGenerateVaultaireResourceQuota(t *testing.T) {
	rq := GenerateVaultaireResourceQuota("vaultaire")

	if rq.Metadata.Name != "vaultaire-quota" {
		t.Errorf("expected name 'vaultaire-quota', got '%s'", rq.Metadata.Name)
	}
	if rq.Spec.Hard["pods"] != "100" {
		t.Error("expected pods quota 100")
	}
	if rq.Spec.Hard["requests.cpu"] != "20" {
		t.Error("expected requests.cpu quota 20")
	}
}

func TestGenerateVaultairePriorityClasses(t *testing.T) {
	pcs := GenerateVaultairePriorityClasses()

	if len(pcs) != 4 {
		t.Errorf("expected 4 priority classes, got %d", len(pcs))
	}

	names := make(map[string]bool)
	for _, pc := range pcs {
		names[pc.Metadata.Name] = true
	}

	expectedNames := []string{"vaultaire-critical", "vaultaire-high", "vaultaire-normal", "vaultaire-low"}
	for _, name := range expectedNames {
		if !names[name] {
			t.Errorf("expected priority class '%s'", name)
		}
	}
}

func TestLimitRangeDefaultNamespace(t *testing.T) {
	rm := NewResourceManager("production")

	lr := rm.GenerateLimitRange(LimitRangeConfig{
		Name: "test-limits",
		Limits: []LimitRangeItem{
			{Type: "Container", Default: ResourceQuantities{CPU: "500m"}},
		},
	})

	if lr.Metadata.Namespace != "production" {
		t.Errorf("expected namespace 'production', got '%s'", lr.Metadata.Namespace)
	}
}

func TestResourceQuotaWithGPU(t *testing.T) {
	rm := NewResourceManager("default")

	rq := rm.GenerateResourceQuota(ResourceQuotaConfig{
		Name: "gpu-quota",
		Hard: ResourceQuotaSpec{
			RequestsNvidiaGPU: "4",
			LimitsNvidiaGPU:   "8",
		},
	})

	if rq.Spec.Hard["requests.nvidia.com/gpu"] != "4" {
		t.Error("expected GPU requests quota")
	}
	if rq.Spec.Hard["limits.nvidia.com/gpu"] != "8" {
		t.Error("expected GPU limits quota")
	}
}

func TestLimitRangeWithMaxLimitRequestRatio(t *testing.T) {
	rm := NewResourceManager("default")

	lr := rm.GenerateLimitRange(LimitRangeConfig{
		Name: "test-limits",
		Limits: []LimitRangeItem{
			{
				Type: "Container",
				MaxLimitRequestRatio: ResourceQuantities{
					CPU:    "10",
					Memory: "5",
				},
			},
		},
	})

	if lr.Spec.Limits[0].MaxLimitRequestRatio["cpu"] != "10" {
		t.Error("expected maxLimitRequestRatio cpu 10")
	}
}
