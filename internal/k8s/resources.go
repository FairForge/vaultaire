// internal/k8s/resources.go
package k8s

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ResourceManager manages Kubernetes resource configurations
type ResourceManager struct {
	namespace string
	labels    map[string]string
}

// NewResourceManager creates a new resource manager
func NewResourceManager(namespace string) *ResourceManager {
	return &ResourceManager{
		namespace: namespace,
		labels: map[string]string{
			"app.kubernetes.io/managed-by": "vaultaire",
		},
	}
}

// ResourceQuantities defines resource quantities (extended from base)
type ResourceQuantities struct {
	CPU              string `yaml:"cpu,omitempty"`
	Memory           string `yaml:"memory,omitempty"`
	EphemeralStorage string `yaml:"ephemeral-storage,omitempty"`
	// Extended resources
	NvidiaGPU    string `yaml:"nvidia.com/gpu,omitempty"`
	AMDGPU       string `yaml:"amd.com/gpu,omitempty"`
	HugePages2Mi string `yaml:"hugepages-2Mi,omitempty"`
	HugePages1Gi string `yaml:"hugepages-1Gi,omitempty"`
}

// LimitRangeConfig configures a LimitRange
type LimitRangeConfig struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
	Limits      []LimitRangeItem
}

// LimitRangeItem defines a limit range item
type LimitRangeItem struct {
	Type                 string             // Container, Pod, PersistentVolumeClaim
	Default              ResourceQuantities // Default limits
	DefaultRequest       ResourceQuantities // Default requests
	Max                  ResourceQuantities // Maximum allowed
	Min                  ResourceQuantities // Minimum required
	MaxLimitRequestRatio ResourceQuantities // Max limit/request ratio
}

// LimitRangeResource represents a LimitRange
type LimitRangeResource struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   ManifestMetadata `yaml:"metadata"`
	Spec       LimitRangeSpec   `yaml:"spec"`
}

// LimitRangeSpec defines LimitRange specification
type LimitRangeSpec struct {
	Limits []LimitRangeItemSpec `yaml:"limits"`
}

// LimitRangeItemSpec defines a limit range item in spec
type LimitRangeItemSpec struct {
	Type                 string            `yaml:"type"`
	Default              map[string]string `yaml:"default,omitempty"`
	DefaultRequest       map[string]string `yaml:"defaultRequest,omitempty"`
	Max                  map[string]string `yaml:"max,omitempty"`
	Min                  map[string]string `yaml:"min,omitempty"`
	MaxLimitRequestRatio map[string]string `yaml:"maxLimitRequestRatio,omitempty"`
}

// GenerateLimitRange creates a LimitRange resource
func (rm *ResourceManager) GenerateLimitRange(config LimitRangeConfig) *LimitRangeResource {
	if config.Namespace == "" {
		config.Namespace = rm.namespace
	}

	labels := copyStringMap(rm.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	limits := make([]LimitRangeItemSpec, 0, len(config.Limits))
	for _, l := range config.Limits {
		item := LimitRangeItemSpec{
			Type:                 l.Type,
			Default:              resourceQuantitiesToMap(l.Default),
			DefaultRequest:       resourceQuantitiesToMap(l.DefaultRequest),
			Max:                  resourceQuantitiesToMap(l.Max),
			Min:                  resourceQuantitiesToMap(l.Min),
			MaxLimitRequestRatio: resourceQuantitiesToMap(l.MaxLimitRequestRatio),
		}
		limits = append(limits, item)
	}

	return &LimitRangeResource{
		APIVersion: "v1",
		Kind:       "LimitRange",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Namespace:   config.Namespace,
			Labels:      labels,
			Annotations: config.Annotations,
		},
		Spec: LimitRangeSpec{
			Limits: limits,
		},
	}
}

// ToYAML converts LimitRange to YAML
func (lr *LimitRangeResource) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(lr); err != nil {
		return "", fmt.Errorf("failed to encode LimitRange: %w", err)
	}
	return buf.String(), nil
}

// ResourceQuotaConfig configures a ResourceQuota
type ResourceQuotaConfig struct {
	Name          string
	Namespace     string
	Labels        map[string]string
	Annotations   map[string]string
	Hard          ResourceQuotaSpec
	Scopes        []string
	ScopeSelector *ScopeSelector
}

// ResourceQuotaSpec defines quota limits
type ResourceQuotaSpec struct {
	// Compute resources
	CPU              string
	Memory           string
	EphemeralStorage string

	// Storage resources
	RequestsStorage        string
	PersistentVolumeClaims string
	StorageClassName       map[string]string // per storage class

	// Object counts
	Pods                   string
	Services               string
	Secrets                string
	ConfigMaps             string
	ReplicationControllers string
	ResourceQuotas         string
	ServicesLoadBalancers  string
	ServicesNodePorts      string

	// Requests and Limits
	RequestsCPU    string
	RequestsMemory string
	LimitsCPU      string
	LimitsMemory   string

	// Extended resources
	RequestsNvidiaGPU string
	LimitsNvidiaGPU   string
}

// ScopeSelector for quota scope selection
type ScopeSelector struct {
	MatchExpressions []ScopeSelectorRequirement
}

// ScopeSelectorRequirement defines a scope requirement
type ScopeSelectorRequirement struct {
	ScopeName string
	Operator  string // In, NotIn, Exists, DoesNotExist
	Values    []string
}

// ResourceQuotaResource represents a ResourceQuota
type ResourceQuotaResource struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   ManifestMetadata   `yaml:"metadata"`
	Spec       ResourceQuotaRSpec `yaml:"spec"`
}

// ResourceQuotaRSpec defines ResourceQuota specification
type ResourceQuotaRSpec struct {
	Hard          map[string]string  `yaml:"hard"`
	Scopes        []string           `yaml:"scopes,omitempty"`
	ScopeSelector *ScopeSelectorSpec `yaml:"scopeSelector,omitempty"`
}

// ScopeSelectorSpec for ResourceQuota spec
type ScopeSelectorSpec struct {
	MatchExpressions []ScopeSelectorRequirementSpec `yaml:"matchExpressions"`
}

// ScopeSelectorRequirementSpec for ResourceQuota spec
type ScopeSelectorRequirementSpec struct {
	ScopeName string   `yaml:"scopeName"`
	Operator  string   `yaml:"operator"`
	Values    []string `yaml:"values,omitempty"`
}

// GenerateResourceQuota creates a ResourceQuota resource
func (rm *ResourceManager) GenerateResourceQuota(config ResourceQuotaConfig) *ResourceQuotaResource {
	if config.Namespace == "" {
		config.Namespace = rm.namespace
	}

	labels := copyStringMap(rm.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	hard := make(map[string]string)

	// Compute resources
	if config.Hard.CPU != "" {
		hard["cpu"] = config.Hard.CPU
	}
	if config.Hard.Memory != "" {
		hard["memory"] = config.Hard.Memory
	}
	if config.Hard.EphemeralStorage != "" {
		hard["ephemeral-storage"] = config.Hard.EphemeralStorage
	}

	// Requests and limits
	if config.Hard.RequestsCPU != "" {
		hard["requests.cpu"] = config.Hard.RequestsCPU
	}
	if config.Hard.RequestsMemory != "" {
		hard["requests.memory"] = config.Hard.RequestsMemory
	}
	if config.Hard.LimitsCPU != "" {
		hard["limits.cpu"] = config.Hard.LimitsCPU
	}
	if config.Hard.LimitsMemory != "" {
		hard["limits.memory"] = config.Hard.LimitsMemory
	}

	// Storage
	if config.Hard.RequestsStorage != "" {
		hard["requests.storage"] = config.Hard.RequestsStorage
	}
	if config.Hard.PersistentVolumeClaims != "" {
		hard["persistentvolumeclaims"] = config.Hard.PersistentVolumeClaims
	}
	for sc, quota := range config.Hard.StorageClassName {
		hard[fmt.Sprintf("%s.storageclass.storage.k8s.io/requests.storage", sc)] = quota
	}

	// Object counts
	if config.Hard.Pods != "" {
		hard["pods"] = config.Hard.Pods
	}
	if config.Hard.Services != "" {
		hard["services"] = config.Hard.Services
	}
	if config.Hard.Secrets != "" {
		hard["secrets"] = config.Hard.Secrets
	}
	if config.Hard.ConfigMaps != "" {
		hard["configmaps"] = config.Hard.ConfigMaps
	}
	if config.Hard.ReplicationControllers != "" {
		hard["replicationcontrollers"] = config.Hard.ReplicationControllers
	}
	if config.Hard.ResourceQuotas != "" {
		hard["resourcequotas"] = config.Hard.ResourceQuotas
	}
	if config.Hard.ServicesLoadBalancers != "" {
		hard["services.loadbalancers"] = config.Hard.ServicesLoadBalancers
	}
	if config.Hard.ServicesNodePorts != "" {
		hard["services.nodeports"] = config.Hard.ServicesNodePorts
	}

	// Extended resources
	if config.Hard.RequestsNvidiaGPU != "" {
		hard["requests.nvidia.com/gpu"] = config.Hard.RequestsNvidiaGPU
	}
	if config.Hard.LimitsNvidiaGPU != "" {
		hard["limits.nvidia.com/gpu"] = config.Hard.LimitsNvidiaGPU
	}

	rq := &ResourceQuotaResource{
		APIVersion: "v1",
		Kind:       "ResourceQuota",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Namespace:   config.Namespace,
			Labels:      labels,
			Annotations: config.Annotations,
		},
		Spec: ResourceQuotaRSpec{
			Hard:   hard,
			Scopes: config.Scopes,
		},
	}

	if config.ScopeSelector != nil {
		rq.Spec.ScopeSelector = &ScopeSelectorSpec{
			MatchExpressions: make([]ScopeSelectorRequirementSpec, 0, len(config.ScopeSelector.MatchExpressions)),
		}
		for _, me := range config.ScopeSelector.MatchExpressions {
			rq.Spec.ScopeSelector.MatchExpressions = append(rq.Spec.ScopeSelector.MatchExpressions, ScopeSelectorRequirementSpec(me))
		}
	}

	return rq
}

// ToYAML converts ResourceQuota to YAML
func (rq *ResourceQuotaResource) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(rq); err != nil {
		return "", fmt.Errorf("failed to encode ResourceQuota: %w", err)
	}
	return buf.String(), nil
}

// PriorityClassConfig configures a PriorityClass
type PriorityClassConfig struct {
	Name             string
	Labels           map[string]string
	Annotations      map[string]string
	Value            int32
	GlobalDefault    bool
	PreemptionPolicy string // Never, PreemptLowerPriority
	Description      string
}

// PriorityClassResource represents a PriorityClass
type PriorityClassResource struct {
	APIVersion       string           `yaml:"apiVersion"`
	Kind             string           `yaml:"kind"`
	Metadata         ManifestMetadata `yaml:"metadata"`
	Value            int32            `yaml:"value"`
	GlobalDefault    bool             `yaml:"globalDefault,omitempty"`
	PreemptionPolicy string           `yaml:"preemptionPolicy,omitempty"`
	Description      string           `yaml:"description,omitempty"`
}

// GeneratePriorityClass creates a PriorityClass resource
func (rm *ResourceManager) GeneratePriorityClass(config PriorityClassConfig) *PriorityClassResource {
	labels := copyStringMap(rm.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	return &PriorityClassResource{
		APIVersion: "scheduling.k8s.io/v1",
		Kind:       "PriorityClass",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Labels:      labels,
			Annotations: config.Annotations,
		},
		Value:            config.Value,
		GlobalDefault:    config.GlobalDefault,
		PreemptionPolicy: config.PreemptionPolicy,
		Description:      config.Description,
	}
}

// ToYAML converts PriorityClass to YAML
func (pc *PriorityClassResource) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(pc); err != nil {
		return "", fmt.Errorf("failed to encode PriorityClass: %w", err)
	}
	return buf.String(), nil
}

// ResourceProfile defines a standard resource profile
type ResourceProfile struct {
	Name        string
	Description string
	Requests    map[string]string
	Limits      map[string]string
}

// StandardResourceProfiles returns common resource profiles
func StandardResourceProfiles() map[string]ResourceProfile {
	return map[string]ResourceProfile{
		"tiny": {
			Name:        "tiny",
			Description: "Minimal resources for lightweight containers",
			Requests:    map[string]string{"cpu": "10m", "memory": "32Mi"},
			Limits:      map[string]string{"cpu": "100m", "memory": "128Mi"},
		},
		"small": {
			Name:        "small",
			Description: "Small workload resources",
			Requests:    map[string]string{"cpu": "100m", "memory": "128Mi"},
			Limits:      map[string]string{"cpu": "500m", "memory": "512Mi"},
		},
		"medium": {
			Name:        "medium",
			Description: "Medium workload resources",
			Requests:    map[string]string{"cpu": "250m", "memory": "256Mi"},
			Limits:      map[string]string{"cpu": "1", "memory": "1Gi"},
		},
		"large": {
			Name:        "large",
			Description: "Large workload resources",
			Requests:    map[string]string{"cpu": "500m", "memory": "512Mi"},
			Limits:      map[string]string{"cpu": "2", "memory": "2Gi"},
		},
		"xlarge": {
			Name:        "xlarge",
			Description: "Extra large workload resources",
			Requests:    map[string]string{"cpu": "1", "memory": "1Gi"},
			Limits:      map[string]string{"cpu": "4", "memory": "4Gi"},
		},
		"memory-optimized": {
			Name:        "memory-optimized",
			Description: "Memory-intensive workloads",
			Requests:    map[string]string{"cpu": "250m", "memory": "2Gi"},
			Limits:      map[string]string{"cpu": "1", "memory": "8Gi"},
		},
		"cpu-optimized": {
			Name:        "cpu-optimized",
			Description: "CPU-intensive workloads",
			Requests:    map[string]string{"cpu": "2", "memory": "512Mi"},
			Limits:      map[string]string{"cpu": "8", "memory": "2Gi"},
		},
		"gpu": {
			Name:        "gpu",
			Description: "GPU workloads",
			Requests:    map[string]string{"cpu": "1", "memory": "4Gi", "nvidia.com/gpu": "1"},
			Limits:      map[string]string{"cpu": "4", "memory": "16Gi", "nvidia.com/gpu": "1"},
		},
	}
}

// GetResourceProfile returns a resource profile by name
func GetResourceProfile(name string) (ResourceProfile, bool) {
	profiles := StandardResourceProfiles()
	profile, ok := profiles[name]
	return profile, ok
}

// resourceQuantitiesToMap converts ResourceQuantities to map
func resourceQuantitiesToMap(rq ResourceQuantities) map[string]string {
	m := make(map[string]string)
	if rq.CPU != "" {
		m["cpu"] = rq.CPU
	}
	if rq.Memory != "" {
		m["memory"] = rq.Memory
	}
	if rq.EphemeralStorage != "" {
		m["ephemeral-storage"] = rq.EphemeralStorage
	}
	if rq.NvidiaGPU != "" {
		m["nvidia.com/gpu"] = rq.NvidiaGPU
	}
	if rq.AMDGPU != "" {
		m["amd.com/gpu"] = rq.AMDGPU
	}
	if rq.HugePages2Mi != "" {
		m["hugepages-2Mi"] = rq.HugePages2Mi
	}
	if rq.HugePages1Gi != "" {
		m["hugepages-1Gi"] = rq.HugePages1Gi
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// GenerateVaultaireLimitRange creates a LimitRange for Vaultaire namespace
func GenerateVaultaireLimitRange(namespace string) *LimitRangeResource {
	rm := NewResourceManager(namespace)

	return rm.GenerateLimitRange(LimitRangeConfig{
		Name: "vaultaire-limits",
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
			{
				Type: "PersistentVolumeClaim",
				Max: ResourceQuantities{
					EphemeralStorage: "100Gi",
				},
				Min: ResourceQuantities{
					EphemeralStorage: "1Gi",
				},
			},
		},
	})
}

// GenerateVaultaireResourceQuota creates a ResourceQuota for Vaultaire namespace
func GenerateVaultaireResourceQuota(namespace string) *ResourceQuotaResource {
	rm := NewResourceManager(namespace)

	return rm.GenerateResourceQuota(ResourceQuotaConfig{
		Name: "vaultaire-quota",
		Hard: ResourceQuotaSpec{
			RequestsCPU:            "20",
			RequestsMemory:         "40Gi",
			LimitsCPU:              "40",
			LimitsMemory:           "80Gi",
			Pods:                   "100",
			Services:               "20",
			Secrets:                "100",
			ConfigMaps:             "100",
			PersistentVolumeClaims: "20",
			RequestsStorage:        "500Gi",
		},
	})
}

// GenerateVaultairePriorityClasses creates PriorityClasses for Vaultaire
func GenerateVaultairePriorityClasses() []*PriorityClassResource {
	rm := NewResourceManager("")

	return []*PriorityClassResource{
		rm.GeneratePriorityClass(PriorityClassConfig{
			Name:             "vaultaire-critical",
			Value:            1000000,
			PreemptionPolicy: "PreemptLowerPriority",
			Description:      "Critical Vaultaire components that must always run",
		}),
		rm.GeneratePriorityClass(PriorityClassConfig{
			Name:             "vaultaire-high",
			Value:            100000,
			PreemptionPolicy: "PreemptLowerPriority",
			Description:      "High priority Vaultaire workloads",
		}),
		rm.GeneratePriorityClass(PriorityClassConfig{
			Name:             "vaultaire-normal",
			Value:            10000,
			PreemptionPolicy: "PreemptLowerPriority",
			Description:      "Normal priority Vaultaire workloads",
		}),
		rm.GeneratePriorityClass(PriorityClassConfig{
			Name:             "vaultaire-low",
			Value:            1000,
			PreemptionPolicy: "Never",
			Description:      "Low priority Vaultaire workloads (batch jobs)",
		}),
	}
}
