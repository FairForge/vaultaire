// internal/k8s/autoscaling.go
package k8s

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// AutoscalingManager manages Kubernetes autoscaling resources
type AutoscalingManager struct {
	namespace string
	labels    map[string]string
}

// NewAutoscalingManager creates a new autoscaling manager
func NewAutoscalingManager(namespace string) *AutoscalingManager {
	return &AutoscalingManager{
		namespace: namespace,
		labels: map[string]string{
			"app.kubernetes.io/managed-by": "vaultaire",
		},
	}
}

// HPAConfig configures a HorizontalPodAutoscaler
type HPAConfig struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string

	// Target reference
	TargetKind       string
	TargetName       string
	TargetAPIVersion string

	// Scaling bounds
	MinReplicas int32
	MaxReplicas int32

	// Metrics
	Metrics []MetricSpec

	// Behavior
	Behavior *HPABehavior
}

// MetricSpec defines a metric for autoscaling
type MetricSpec struct {
	Type              MetricType
	Resource          *ResourceMetricSource
	Pods              *PodsMetricSource
	Object            *ObjectMetricSource
	External          *ExternalMetricSource
	ContainerResource *ContainerResourceMetricSource
}

// MetricType defines the type of metric
type MetricType string

const (
	MetricTypeResource          MetricType = "Resource"
	MetricTypePods              MetricType = "Pods"
	MetricTypeObject            MetricType = "Object"
	MetricTypeExternal          MetricType = "External"
	MetricTypeContainerResource MetricType = "ContainerResource"
)

// ResourceMetricSource defines resource-based metrics
type ResourceMetricSource struct {
	Name   string
	Target MetricTarget
}

// PodsMetricSource defines pod-based metrics
type PodsMetricSource struct {
	Metric MetricIdentifier
	Target MetricTarget
}

// ObjectMetricSource defines object-based metrics
type ObjectMetricSource struct {
	DescribedObject CrossVersionObjectReference
	Metric          MetricIdentifier
	Target          MetricTarget
}

// ExternalMetricSource defines external metrics
type ExternalMetricSource struct {
	Metric MetricIdentifier
	Target MetricTarget
}

// ContainerResourceMetricSource defines container resource metrics
type ContainerResourceMetricSource struct {
	Name      string
	Container string
	Target    MetricTarget
}

// MetricIdentifier identifies a metric
type MetricIdentifier struct {
	Name     string
	Selector *HPALabelSelectorDef
}

// LabelSelector defines label selection
type HPALabelSelectorDef struct {
	MatchLabels      map[string]string
	MatchExpressions []LabelSelectorRequirement
}

// LabelSelectorRequirement defines a label requirement
type LabelSelectorRequirement struct {
	Key      string
	Operator string
	Values   []string
}

// CrossVersionObjectReference references an object
type CrossVersionObjectReference struct {
	APIVersion string
	Kind       string
	Name       string
}

// MetricTarget defines the target value for a metric
type MetricTarget struct {
	Type               MetricTargetType
	Value              string
	AverageValue       string
	AverageUtilization *int32
}

// MetricTargetType defines target type
type MetricTargetType string

const (
	MetricTargetTypeUtilization  MetricTargetType = "Utilization"
	MetricTargetTypeValue        MetricTargetType = "Value"
	MetricTargetTypeAverageValue MetricTargetType = "AverageValue"
)

// HPABehavior defines scaling behavior
type HPABehavior struct {
	ScaleUp   *HPAScalingRules
	ScaleDown *HPAScalingRules
}

// HPAScalingRules defines scaling rules
type HPAScalingRules struct {
	StabilizationWindowSeconds *int32
	SelectPolicy               string // Max, Min, Disabled
	Policies                   []HPAScalingPolicy
}

// HPAScalingPolicy defines a scaling policy
type HPAScalingPolicy struct {
	Type          string // Pods, Percent
	Value         int32
	PeriodSeconds int32
}

// HPAResource represents a HorizontalPodAutoscaler
type HPAResource struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   ManifestMetadata `yaml:"metadata"`
	Spec       HPASpec          `yaml:"spec"`
}

// HPASpec defines HPA specification
type HPASpec struct {
	ScaleTargetRef HPAScaleTargetRef `yaml:"scaleTargetRef"`
	MinReplicas    *int32            `yaml:"minReplicas,omitempty"`
	MaxReplicas    int32             `yaml:"maxReplicas"`
	Metrics        []HPAMetricSpec   `yaml:"metrics,omitempty"`
	Behavior       *HPABehaviorSpec  `yaml:"behavior,omitempty"`
}

// HPAScaleTargetRef references the target
type HPAScaleTargetRef struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Name       string `yaml:"name"`
}

// HPAMetricSpec defines a metric in HPA spec
type HPAMetricSpec struct {
	Type              string                            `yaml:"type"`
	Resource          *HPAResourceMetricSource          `yaml:"resource,omitempty"`
	Pods              *HPAPodsMetricSource              `yaml:"pods,omitempty"`
	Object            *HPAObjectMetricSource            `yaml:"object,omitempty"`
	External          *HPAExternalMetricSource          `yaml:"external,omitempty"`
	ContainerResource *HPAContainerResourceMetricSource `yaml:"containerResource,omitempty"`
}

// HPAResourceMetricSource for HPA spec
type HPAResourceMetricSource struct {
	Name   string          `yaml:"name"`
	Target HPAMetricTarget `yaml:"target"`
}

// HPAPodsMetricSource for HPA spec
type HPAPodsMetricSource struct {
	Metric HPAMetricIdentifier `yaml:"metric"`
	Target HPAMetricTarget     `yaml:"target"`
}

// HPAObjectMetricSource for HPA spec
type HPAObjectMetricSource struct {
	DescribedObject HPAScaleTargetRef   `yaml:"describedObject"`
	Metric          HPAMetricIdentifier `yaml:"metric"`
	Target          HPAMetricTarget     `yaml:"target"`
}

// HPAExternalMetricSource for HPA spec
type HPAExternalMetricSource struct {
	Metric HPAMetricIdentifier `yaml:"metric"`
	Target HPAMetricTarget     `yaml:"target"`
}

// HPAContainerResourceMetricSource for HPA spec
type HPAContainerResourceMetricSource struct {
	Name      string          `yaml:"name"`
	Container string          `yaml:"container"`
	Target    HPAMetricTarget `yaml:"target"`
}

// HPAMetricIdentifier for HPA spec
type HPAMetricIdentifier struct {
	Name     string            `yaml:"name"`
	Selector *HPALabelSelector `yaml:"selector,omitempty"`
}

// HPALabelSelector for HPA spec
type HPALabelSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels,omitempty"`
}

// HPAMetricTarget for HPA spec
type HPAMetricTarget struct {
	Type               string `yaml:"type"`
	Value              string `yaml:"value,omitempty"`
	AverageValue       string `yaml:"averageValue,omitempty"`
	AverageUtilization *int32 `yaml:"averageUtilization,omitempty"`
}

// HPABehaviorSpec for HPA spec
type HPABehaviorSpec struct {
	ScaleUp   *HPAScalingRulesSpec `yaml:"scaleUp,omitempty"`
	ScaleDown *HPAScalingRulesSpec `yaml:"scaleDown,omitempty"`
}

// HPAScalingRulesSpec for HPA spec
type HPAScalingRulesSpec struct {
	StabilizationWindowSeconds *int32                 `yaml:"stabilizationWindowSeconds,omitempty"`
	SelectPolicy               string                 `yaml:"selectPolicy,omitempty"`
	Policies                   []HPAScalingPolicySpec `yaml:"policies,omitempty"`
}

// HPAScalingPolicySpec for HPA spec
type HPAScalingPolicySpec struct {
	Type          string `yaml:"type"`
	Value         int32  `yaml:"value"`
	PeriodSeconds int32  `yaml:"periodSeconds"`
}

// GenerateHPA creates a HorizontalPodAutoscaler resource
func (am *AutoscalingManager) GenerateHPA(config HPAConfig) *HPAResource {
	if config.Namespace == "" {
		config.Namespace = am.namespace
	}
	if config.TargetKind == "" {
		config.TargetKind = "Deployment"
	}
	if config.TargetAPIVersion == "" {
		config.TargetAPIVersion = "apps/v1"
	}

	labels := copyStringMap(am.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	hpa := &HPAResource{
		APIVersion: "autoscaling/v2",
		Kind:       "HorizontalPodAutoscaler",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Namespace:   config.Namespace,
			Labels:      labels,
			Annotations: config.Annotations,
		},
		Spec: HPASpec{
			ScaleTargetRef: HPAScaleTargetRef{
				APIVersion: config.TargetAPIVersion,
				Kind:       config.TargetKind,
				Name:       config.TargetName,
			},
			MaxReplicas: config.MaxReplicas,
		},
	}

	if config.MinReplicas > 0 {
		hpa.Spec.MinReplicas = &config.MinReplicas
	}

	// Convert metrics
	if len(config.Metrics) > 0 {
		hpa.Spec.Metrics = convertMetrics(config.Metrics)
	}

	// Convert behavior
	if config.Behavior != nil {
		hpa.Spec.Behavior = convertBehavior(config.Behavior)
	}

	return hpa
}

func convertMetrics(metrics []MetricSpec) []HPAMetricSpec {
	result := make([]HPAMetricSpec, 0, len(metrics))
	for _, m := range metrics {
		spec := HPAMetricSpec{Type: string(m.Type)}

		if m.Resource != nil {
			spec.Resource = &HPAResourceMetricSource{
				Name: m.Resource.Name,
				Target: HPAMetricTarget{
					Type:               string(m.Resource.Target.Type),
					Value:              m.Resource.Target.Value,
					AverageValue:       m.Resource.Target.AverageValue,
					AverageUtilization: m.Resource.Target.AverageUtilization,
				},
			}
		}

		if m.Pods != nil {
			spec.Pods = &HPAPodsMetricSource{
				Metric: HPAMetricIdentifier{Name: m.Pods.Metric.Name},
				Target: HPAMetricTarget{
					Type:         string(m.Pods.Target.Type),
					AverageValue: m.Pods.Target.AverageValue,
				},
			}
		}

		if m.External != nil {
			spec.External = &HPAExternalMetricSource{
				Metric: HPAMetricIdentifier{Name: m.External.Metric.Name},
				Target: HPAMetricTarget{
					Type:         string(m.External.Target.Type),
					Value:        m.External.Target.Value,
					AverageValue: m.External.Target.AverageValue,
				},
			}
		}

		if m.ContainerResource != nil {
			spec.ContainerResource = &HPAContainerResourceMetricSource{
				Name:      m.ContainerResource.Name,
				Container: m.ContainerResource.Container,
				Target: HPAMetricTarget{
					Type:               string(m.ContainerResource.Target.Type),
					AverageUtilization: m.ContainerResource.Target.AverageUtilization,
				},
			}
		}

		result = append(result, spec)
	}
	return result
}

func convertBehavior(behavior *HPABehavior) *HPABehaviorSpec {
	spec := &HPABehaviorSpec{}

	if behavior.ScaleUp != nil {
		spec.ScaleUp = &HPAScalingRulesSpec{
			StabilizationWindowSeconds: behavior.ScaleUp.StabilizationWindowSeconds,
			SelectPolicy:               behavior.ScaleUp.SelectPolicy,
		}
		for _, p := range behavior.ScaleUp.Policies {
			spec.ScaleUp.Policies = append(spec.ScaleUp.Policies, HPAScalingPolicySpec(p))
		}
	}

	if behavior.ScaleDown != nil {
		spec.ScaleDown = &HPAScalingRulesSpec{
			StabilizationWindowSeconds: behavior.ScaleDown.StabilizationWindowSeconds,
			SelectPolicy:               behavior.ScaleDown.SelectPolicy,
		}
		for _, p := range behavior.ScaleDown.Policies {
			spec.ScaleDown.Policies = append(spec.ScaleDown.Policies, HPAScalingPolicySpec(p))
		}
	}

	return spec
}

// ToYAML converts HPA to YAML
func (h *HPAResource) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(h); err != nil {
		return "", fmt.Errorf("failed to encode HPA: %w", err)
	}
	return buf.String(), nil
}

// VPAConfig configures a VerticalPodAutoscaler
type VPAConfig struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string

	// Target reference
	TargetKind       string
	TargetName       string
	TargetAPIVersion string

	// Update policy
	UpdateMode string // Off, Initial, Recreate, Auto

	// Resource policy
	ContainerPolicies []VPAContainerPolicy
}

// VPAContainerPolicy defines resource policy for a container
type VPAContainerPolicy struct {
	ContainerName       string
	Mode                string // Auto, Off
	MinAllowed          ResourceList
	MaxAllowed          ResourceList
	ControlledResources []string // cpu, memory
	ControlledValues    string   // RequestsOnly, RequestsAndLimits
}

// ResourceList defines resource quantities
type ResourceList struct {
	CPU    string
	Memory string
}

// VPAResource represents a VerticalPodAutoscaler
type VPAResource struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   ManifestMetadata `yaml:"metadata"`
	Spec       VPASpec          `yaml:"spec"`
}

// VPASpec defines VPA specification
type VPASpec struct {
	TargetRef      VPATargetRef       `yaml:"targetRef"`
	UpdatePolicy   *VPAUpdatePolicy   `yaml:"updatePolicy,omitempty"`
	ResourcePolicy *VPAResourcePolicy `yaml:"resourcePolicy,omitempty"`
}

// VPATargetRef references the target
type VPATargetRef struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Name       string `yaml:"name"`
}

// VPAUpdatePolicy defines update policy
type VPAUpdatePolicy struct {
	UpdateMode string `yaml:"updateMode"`
}

// VPAResourcePolicy defines resource policy
type VPAResourcePolicy struct {
	ContainerPolicies []VPAContainerPolicySpec `yaml:"containerPolicies"`
}

// VPAContainerPolicySpec defines container policy in spec
type VPAContainerPolicySpec struct {
	ContainerName       string            `yaml:"containerName"`
	Mode                string            `yaml:"mode,omitempty"`
	MinAllowed          map[string]string `yaml:"minAllowed,omitempty"`
	MaxAllowed          map[string]string `yaml:"maxAllowed,omitempty"`
	ControlledResources []string          `yaml:"controlledResources,omitempty"`
	ControlledValues    string            `yaml:"controlledValues,omitempty"`
}

// GenerateVPA creates a VerticalPodAutoscaler resource
func (am *AutoscalingManager) GenerateVPA(config VPAConfig) *VPAResource {
	if config.Namespace == "" {
		config.Namespace = am.namespace
	}
	if config.TargetKind == "" {
		config.TargetKind = "Deployment"
	}
	if config.TargetAPIVersion == "" {
		config.TargetAPIVersion = "apps/v1"
	}
	if config.UpdateMode == "" {
		config.UpdateMode = "Auto"
	}

	labels := copyStringMap(am.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	vpa := &VPAResource{
		APIVersion: "autoscaling.k8s.io/v1",
		Kind:       "VerticalPodAutoscaler",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Namespace:   config.Namespace,
			Labels:      labels,
			Annotations: config.Annotations,
		},
		Spec: VPASpec{
			TargetRef: VPATargetRef{
				APIVersion: config.TargetAPIVersion,
				Kind:       config.TargetKind,
				Name:       config.TargetName,
			},
			UpdatePolicy: &VPAUpdatePolicy{
				UpdateMode: config.UpdateMode,
			},
		},
	}

	// Convert container policies
	if len(config.ContainerPolicies) > 0 {
		vpa.Spec.ResourcePolicy = &VPAResourcePolicy{
			ContainerPolicies: make([]VPAContainerPolicySpec, 0, len(config.ContainerPolicies)),
		}
		for _, cp := range config.ContainerPolicies {
			spec := VPAContainerPolicySpec{
				ContainerName:       cp.ContainerName,
				Mode:                cp.Mode,
				ControlledResources: cp.ControlledResources,
				ControlledValues:    cp.ControlledValues,
			}
			if cp.MinAllowed.CPU != "" || cp.MinAllowed.Memory != "" {
				spec.MinAllowed = make(map[string]string)
				if cp.MinAllowed.CPU != "" {
					spec.MinAllowed["cpu"] = cp.MinAllowed.CPU
				}
				if cp.MinAllowed.Memory != "" {
					spec.MinAllowed["memory"] = cp.MinAllowed.Memory
				}
			}
			if cp.MaxAllowed.CPU != "" || cp.MaxAllowed.Memory != "" {
				spec.MaxAllowed = make(map[string]string)
				if cp.MaxAllowed.CPU != "" {
					spec.MaxAllowed["cpu"] = cp.MaxAllowed.CPU
				}
				if cp.MaxAllowed.Memory != "" {
					spec.MaxAllowed["memory"] = cp.MaxAllowed.Memory
				}
			}
			vpa.Spec.ResourcePolicy.ContainerPolicies = append(vpa.Spec.ResourcePolicy.ContainerPolicies, spec)
		}
	}

	return vpa
}

// ToYAML converts VPA to YAML
func (v *VPAResource) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(v); err != nil {
		return "", fmt.Errorf("failed to encode VPA: %w", err)
	}
	return buf.String(), nil
}

// PDBConfig configures a PodDisruptionBudget
type PDBConfig struct {
	Name           string
	Namespace      string
	Labels         map[string]string
	Annotations    map[string]string
	Selector       map[string]string
	MinAvailable   string // Can be number or percentage
	MaxUnavailable string // Can be number or percentage
}

// PDBResource represents a PodDisruptionBudget
type PDBResource struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   ManifestMetadata `yaml:"metadata"`
	Spec       PDBSpec          `yaml:"spec"`
}

// PDBSpec defines PDB specification
type PDBSpec struct {
	Selector       *PDBLabelSelector `yaml:"selector"`
	MinAvailable   string            `yaml:"minAvailable,omitempty"`
	MaxUnavailable string            `yaml:"maxUnavailable,omitempty"`
}

// PDBLabelSelector for PDB
type PDBLabelSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

// GeneratePDB creates a PodDisruptionBudget resource
func (am *AutoscalingManager) GeneratePDB(config PDBConfig) *PDBResource {
	if config.Namespace == "" {
		config.Namespace = am.namespace
	}

	labels := copyStringMap(am.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	pdb := &PDBResource{
		APIVersion: "policy/v1",
		Kind:       "PodDisruptionBudget",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Namespace:   config.Namespace,
			Labels:      labels,
			Annotations: config.Annotations,
		},
		Spec: PDBSpec{
			Selector: &PDBLabelSelector{
				MatchLabels: config.Selector,
			},
		},
	}

	if config.MinAvailable != "" {
		pdb.Spec.MinAvailable = config.MinAvailable
	}
	if config.MaxUnavailable != "" {
		pdb.Spec.MaxUnavailable = config.MaxUnavailable
	}

	return pdb
}

// ToYAML converts PDB to YAML
func (p *PDBResource) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(p); err != nil {
		return "", fmt.Errorf("failed to encode PDB: %w", err)
	}
	return buf.String(), nil
}

// HPABuilder provides a fluent interface for building HPA
type HPABuilder struct {
	config HPAConfig
}

// NewHPABuilder creates a new HPA builder
func NewHPABuilder(name, namespace, targetName string) *HPABuilder {
	return &HPABuilder{
		config: HPAConfig{
			Name:       name,
			Namespace:  namespace,
			TargetName: targetName,
			TargetKind: "Deployment",
		},
	}
}

// WithMinReplicas sets minimum replicas
func (b *HPABuilder) WithMinReplicas(min int32) *HPABuilder {
	b.config.MinReplicas = min
	return b
}

// WithMaxReplicas sets maximum replicas
func (b *HPABuilder) WithMaxReplicas(max int32) *HPABuilder {
	b.config.MaxReplicas = max
	return b
}

// WithCPUUtilization adds CPU utilization metric
func (b *HPABuilder) WithCPUUtilization(targetPercent int32) *HPABuilder {
	b.config.Metrics = append(b.config.Metrics, MetricSpec{
		Type: MetricTypeResource,
		Resource: &ResourceMetricSource{
			Name: "cpu",
			Target: MetricTarget{
				Type:               MetricTargetTypeUtilization,
				AverageUtilization: &targetPercent,
			},
		},
	})
	return b
}

// WithMemoryUtilization adds memory utilization metric
func (b *HPABuilder) WithMemoryUtilization(targetPercent int32) *HPABuilder {
	b.config.Metrics = append(b.config.Metrics, MetricSpec{
		Type: MetricTypeResource,
		Resource: &ResourceMetricSource{
			Name: "memory",
			Target: MetricTarget{
				Type:               MetricTargetTypeUtilization,
				AverageUtilization: &targetPercent,
			},
		},
	})
	return b
}

// WithCustomMetric adds a custom metric
func (b *HPABuilder) WithCustomMetric(name string, targetAverageValue string) *HPABuilder {
	b.config.Metrics = append(b.config.Metrics, MetricSpec{
		Type: MetricTypePods,
		Pods: &PodsMetricSource{
			Metric: MetricIdentifier{Name: name},
			Target: MetricTarget{
				Type:         MetricTargetTypeAverageValue,
				AverageValue: targetAverageValue,
			},
		},
	})
	return b
}

// WithExternalMetric adds an external metric
func (b *HPABuilder) WithExternalMetric(name string, targetValue string) *HPABuilder {
	b.config.Metrics = append(b.config.Metrics, MetricSpec{
		Type: MetricTypeExternal,
		External: &ExternalMetricSource{
			Metric: MetricIdentifier{Name: name},
			Target: MetricTarget{
				Type:  MetricTargetTypeValue,
				Value: targetValue,
			},
		},
	})
	return b
}

// WithScaleUpPolicy adds scale up policy
func (b *HPABuilder) WithScaleUpPolicy(policyType string, value, periodSeconds int32) *HPABuilder {
	if b.config.Behavior == nil {
		b.config.Behavior = &HPABehavior{}
	}
	if b.config.Behavior.ScaleUp == nil {
		b.config.Behavior.ScaleUp = &HPAScalingRules{}
	}
	b.config.Behavior.ScaleUp.Policies = append(b.config.Behavior.ScaleUp.Policies, HPAScalingPolicy{
		Type:          policyType,
		Value:         value,
		PeriodSeconds: periodSeconds,
	})
	return b
}

// WithScaleDownPolicy adds scale down policy
func (b *HPABuilder) WithScaleDownPolicy(policyType string, value, periodSeconds int32) *HPABuilder {
	if b.config.Behavior == nil {
		b.config.Behavior = &HPABehavior{}
	}
	if b.config.Behavior.ScaleDown == nil {
		b.config.Behavior.ScaleDown = &HPAScalingRules{}
	}
	b.config.Behavior.ScaleDown.Policies = append(b.config.Behavior.ScaleDown.Policies, HPAScalingPolicy{
		Type:          policyType,
		Value:         value,
		PeriodSeconds: periodSeconds,
	})
	return b
}

// WithScaleUpStabilization sets scale up stabilization window
func (b *HPABuilder) WithScaleUpStabilization(seconds int32) *HPABuilder {
	if b.config.Behavior == nil {
		b.config.Behavior = &HPABehavior{}
	}
	if b.config.Behavior.ScaleUp == nil {
		b.config.Behavior.ScaleUp = &HPAScalingRules{}
	}
	b.config.Behavior.ScaleUp.StabilizationWindowSeconds = &seconds
	return b
}

// WithScaleDownStabilization sets scale down stabilization window
func (b *HPABuilder) WithScaleDownStabilization(seconds int32) *HPABuilder {
	if b.config.Behavior == nil {
		b.config.Behavior = &HPABehavior{}
	}
	if b.config.Behavior.ScaleDown == nil {
		b.config.Behavior.ScaleDown = &HPAScalingRules{}
	}
	b.config.Behavior.ScaleDown.StabilizationWindowSeconds = &seconds
	return b
}

// Build creates the HPA resource
func (b *HPABuilder) Build() *HPAResource {
	am := NewAutoscalingManager(b.config.Namespace)
	return am.GenerateHPA(b.config)
}

// GenerateVaultaireAutoscaling creates autoscaling resources for Vaultaire
func GenerateVaultaireAutoscaling(namespace string) (*HPAResource, *VPAResource, *PDBResource) {
	am := NewAutoscalingManager(namespace)

	// HPA for API server
	cpu := int32(70)
	memory := int32(80)
	scaleUpWindow := int32(0)
	scaleDownWindow := int32(300)

	hpa := am.GenerateHPA(HPAConfig{
		Name:        "vaultaire-api",
		TargetName:  "vaultaire-api",
		MinReplicas: 2,
		MaxReplicas: 20,
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
		},
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

	// VPA for workers (recommendation only)
	vpa := am.GenerateVPA(VPAConfig{
		Name:       "vaultaire-worker",
		TargetName: "vaultaire-worker",
		UpdateMode: "Off", // Recommendation only
		ContainerPolicies: []VPAContainerPolicy{
			{
				ContainerName:       "worker",
				MinAllowed:          ResourceList{CPU: "100m", Memory: "128Mi"},
				MaxAllowed:          ResourceList{CPU: "4", Memory: "8Gi"},
				ControlledResources: []string{"cpu", "memory"},
			},
		},
	})

	// PDB for API server
	pdb := am.GeneratePDB(PDBConfig{
		Name:         "vaultaire-api",
		Selector:     map[string]string{"app": "vaultaire-api"},
		MinAvailable: "50%",
	})

	return hpa, vpa, pdb
}

// AutoscalingSet represents a collection of autoscaling resources
type AutoscalingSet struct {
	HPAs []*HPAResource
	VPAs []*VPAResource
	PDBs []*PDBResource
}

// ToYAML converts all resources to multi-document YAML
func (s *AutoscalingSet) ToYAML() (string, error) {
	var parts []string

	for _, hpa := range s.HPAs {
		yaml, err := hpa.ToYAML()
		if err != nil {
			return "", err
		}
		parts = append(parts, yaml)
	}

	for _, vpa := range s.VPAs {
		yaml, err := vpa.ToYAML()
		if err != nil {
			return "", err
		}
		parts = append(parts, yaml)
	}

	for _, pdb := range s.PDBs {
		yaml, err := pdb.ToYAML()
		if err != nil {
			return "", err
		}
		parts = append(parts, yaml)
	}

	return strings.Join(parts, "---\n"), nil
}
