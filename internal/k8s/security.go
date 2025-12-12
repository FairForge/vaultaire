// internal/k8s/security.go
package k8s

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// SecurityManager manages Kubernetes security resources
type SecurityManager struct {
	namespace string
	labels    map[string]string
}

// NewSecurityManager creates a new security manager
func NewSecurityManager(namespace string) *SecurityManager {
	return &SecurityManager{
		namespace: namespace,
		labels: map[string]string{
			"app.kubernetes.io/managed-by": "vaultaire",
		},
	}
}

// SecurityPodContext defines pod-level security settings (renamed to avoid conflict)
type SecurityPodContext struct {
	RunAsUser           *int64
	RunAsGroup          *int64
	RunAsNonRoot        *bool
	FSGroup             *int64
	FSGroupChangePolicy string // OnRootMismatch, Always
	SupplementalGroups  []int64
	Sysctls             []Sysctl
	SeccompProfile      *SeccompProfile
}

// SecurityContainerContext defines container-level security settings
type SecurityContainerContext struct {
	RunAsUser                *int64
	RunAsGroup               *int64
	RunAsNonRoot             *bool
	ReadOnlyRootFilesystem   *bool
	AllowPrivilegeEscalation *bool
	Privileged               *bool
	Capabilities             *SecurityCapabilities
	SeccompProfile           *SeccompProfile
	SELinuxOptions           *SELinuxOptions
	ProcMount                string // Default, Unmasked
}

// Sysctl defines a sysctl parameter
type Sysctl struct {
	Name  string
	Value string
}

// SeccompProfile defines seccomp settings
type SeccompProfile struct {
	Type             string // Localhost, RuntimeDefault, Unconfined
	LocalhostProfile string
}

// SecurityCapabilities defines Linux capabilities (renamed to avoid conflict)
type SecurityCapabilities struct {
	Add  []string
	Drop []string
}

// SELinuxOptions defines SELinux settings
type SELinuxOptions struct {
	User  string
	Role  string
	Type  string
	Level string
}

// RoleConfig configures a Role or ClusterRole
type RoleConfig struct {
	Name        string
	Namespace   string // empty for ClusterRole
	Labels      map[string]string
	Annotations map[string]string
	Rules       []PolicyRule
}

// PolicyRule defines a policy rule
type PolicyRule struct {
	APIGroups       []string
	Resources       []string
	ResourceNames   []string
	Verbs           []string
	NonResourceURLs []string
}

// RoleResource represents a Role
type RoleResource struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   ManifestMetadata `yaml:"metadata"`
	Rules      []PolicyRuleSpec `yaml:"rules"`
}

// PolicyRuleSpec for Role spec
type PolicyRuleSpec struct {
	APIGroups       []string `yaml:"apiGroups,omitempty"`
	Resources       []string `yaml:"resources,omitempty"`
	ResourceNames   []string `yaml:"resourceNames,omitempty"`
	Verbs           []string `yaml:"verbs"`
	NonResourceURLs []string `yaml:"nonResourceURLs,omitempty"`
}

// GenerateRole creates a Role resource
func (sm *SecurityManager) GenerateRole(config RoleConfig) *RoleResource {
	if config.Namespace == "" {
		config.Namespace = sm.namespace
	}

	labels := copyStringMap(sm.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	rules := make([]PolicyRuleSpec, 0, len(config.Rules))
	for _, r := range config.Rules {
		rules = append(rules, PolicyRuleSpec(r))
	}

	return &RoleResource{
		APIVersion: "rbac.authorization.k8s.io/v1",
		Kind:       "Role",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Namespace:   config.Namespace,
			Labels:      labels,
			Annotations: config.Annotations,
		},
		Rules: rules,
	}
}

// GenerateClusterRole creates a ClusterRole resource
func (sm *SecurityManager) GenerateClusterRole(config RoleConfig) *RoleResource {
	labels := copyStringMap(sm.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	rules := make([]PolicyRuleSpec, 0, len(config.Rules))
	for _, r := range config.Rules {
		rules = append(rules, PolicyRuleSpec(r))
	}

	return &RoleResource{
		APIVersion: "rbac.authorization.k8s.io/v1",
		Kind:       "ClusterRole",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Labels:      labels,
			Annotations: config.Annotations,
		},
		Rules: rules,
	}
}

// ToYAML converts Role to YAML
func (r *RoleResource) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(r); err != nil {
		return "", fmt.Errorf("failed to encode Role: %w", err)
	}
	return buf.String(), nil
}

// RoleBindingConfig configures a RoleBinding or ClusterRoleBinding
type RoleBindingConfig struct {
	Name        string
	Namespace   string // empty for ClusterRoleBinding
	Labels      map[string]string
	Annotations map[string]string
	RoleRef     RoleRef
	Subjects    []Subject
}

// RoleRef references a Role or ClusterRole
type RoleRef struct {
	APIGroup string
	Kind     string // Role or ClusterRole
	Name     string
}

// Subject defines who the binding applies to
type Subject struct {
	Kind      string // User, Group, ServiceAccount
	Name      string
	Namespace string // only for ServiceAccount
	APIGroup  string // only for User/Group
}

// RoleBindingResource represents a RoleBinding
type RoleBindingResource struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   ManifestMetadata `yaml:"metadata"`
	RoleRef    RoleRefSpec      `yaml:"roleRef"`
	Subjects   []SubjectSpec    `yaml:"subjects"`
}

// RoleRefSpec for RoleBinding spec
type RoleRefSpec struct {
	APIGroup string `yaml:"apiGroup"`
	Kind     string `yaml:"kind"`
	Name     string `yaml:"name"`
}

// SubjectSpec for RoleBinding spec
type SubjectSpec struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"`
	APIGroup  string `yaml:"apiGroup,omitempty"`
}

// GenerateRoleBinding creates a RoleBinding resource
func (sm *SecurityManager) GenerateRoleBinding(config RoleBindingConfig) *RoleBindingResource {
	if config.Namespace == "" {
		config.Namespace = sm.namespace
	}

	labels := copyStringMap(sm.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	subjects := make([]SubjectSpec, 0, len(config.Subjects))
	for _, s := range config.Subjects {
		subjects = append(subjects, SubjectSpec(s))
	}

	return &RoleBindingResource{
		APIVersion: "rbac.authorization.k8s.io/v1",
		Kind:       "RoleBinding",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Namespace:   config.Namespace,
			Labels:      labels,
			Annotations: config.Annotations,
		},
		RoleRef: RoleRefSpec{
			APIGroup: config.RoleRef.APIGroup,
			Kind:     config.RoleRef.Kind,
			Name:     config.RoleRef.Name,
		},
		Subjects: subjects,
	}
}

// GenerateClusterRoleBinding creates a ClusterRoleBinding resource
func (sm *SecurityManager) GenerateClusterRoleBinding(config RoleBindingConfig) *RoleBindingResource {
	labels := copyStringMap(sm.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	subjects := make([]SubjectSpec, 0, len(config.Subjects))
	for _, s := range config.Subjects {
		subjects = append(subjects, SubjectSpec(s))
	}

	return &RoleBindingResource{
		APIVersion: "rbac.authorization.k8s.io/v1",
		Kind:       "ClusterRoleBinding",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Labels:      labels,
			Annotations: config.Annotations,
		},
		RoleRef: RoleRefSpec{
			APIGroup: config.RoleRef.APIGroup,
			Kind:     config.RoleRef.Kind,
			Name:     config.RoleRef.Name,
		},
		Subjects: subjects,
	}
}

// ToYAML converts RoleBinding to YAML
func (rb *RoleBindingResource) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(rb); err != nil {
		return "", fmt.Errorf("failed to encode RoleBinding: %w", err)
	}
	return buf.String(), nil
}

// PodSecurityStandard defines Pod Security Standards levels
type PodSecurityStandard string

const (
	PSSPrivileged PodSecurityStandard = "privileged"
	PSSBaseline   PodSecurityStandard = "baseline"
	PSSRestricted PodSecurityStandard = "restricted"
)

// PodSecurityMode defines Pod Security Standards enforcement mode
type PodSecurityMode string

const (
	PSSModeEnforce PodSecurityMode = "enforce"
	PSSModeAudit   PodSecurityMode = "audit"
	PSSModeWarn    PodSecurityMode = "warn"
)

// GetPodSecurityLabels returns namespace labels for Pod Security Standards
func GetPodSecurityLabels(level PodSecurityStandard, mode PodSecurityMode, version string) map[string]string {
	if version == "" {
		version = "latest"
	}
	return map[string]string{
		fmt.Sprintf("pod-security.kubernetes.io/%s", mode):         string(level),
		fmt.Sprintf("pod-security.kubernetes.io/%s-version", mode): version,
	}
}

// RestrictedSecurityContext returns a restricted container security context
func RestrictedSecurityContext() *SecurityContainerContext {
	nonRoot := true
	readOnly := true
	noPrivEsc := false
	privileged := false

	return &SecurityContainerContext{
		RunAsNonRoot:             &nonRoot,
		ReadOnlyRootFilesystem:   &readOnly,
		AllowPrivilegeEscalation: &noPrivEsc,
		Privileged:               &privileged,
		Capabilities: &SecurityCapabilities{
			Drop: []string{"ALL"},
		},
		SeccompProfile: &SeccompProfile{
			Type: "RuntimeDefault",
		},
	}
}

// BaselineSecurityContext returns a baseline container security context
func BaselineSecurityContext() *SecurityContainerContext {
	noPrivEsc := false
	privileged := false

	return &SecurityContainerContext{
		AllowPrivilegeEscalation: &noPrivEsc,
		Privileged:               &privileged,
		Capabilities: &SecurityCapabilities{
			Drop: []string{"ALL"},
			Add:  []string{"NET_BIND_SERVICE"},
		},
	}
}

// ContainerSecurityContextSpec for manifest
type ContainerSecurityContextSpec struct {
	RunAsUser                *int64                    `yaml:"runAsUser,omitempty"`
	RunAsGroup               *int64                    `yaml:"runAsGroup,omitempty"`
	RunAsNonRoot             *bool                     `yaml:"runAsNonRoot,omitempty"`
	ReadOnlyRootFilesystem   *bool                     `yaml:"readOnlyRootFilesystem,omitempty"`
	AllowPrivilegeEscalation *bool                     `yaml:"allowPrivilegeEscalation,omitempty"`
	Privileged               *bool                     `yaml:"privileged,omitempty"`
	Capabilities             *SecurityCapabilitiesSpec `yaml:"capabilities,omitempty"`
	SeccompProfile           *SeccompProfileSpec       `yaml:"seccompProfile,omitempty"`
	SELinuxOptions           *SELinuxOptionsSpec       `yaml:"seLinuxOptions,omitempty"`
	ProcMount                string                    `yaml:"procMount,omitempty"`
}

// SecurityPodContextSpec for manifest
type SecurityPodContextSpec struct {
	RunAsUser           *int64              `yaml:"runAsUser,omitempty"`
	RunAsGroup          *int64              `yaml:"runAsGroup,omitempty"`
	RunAsNonRoot        *bool               `yaml:"runAsNonRoot,omitempty"`
	FSGroup             *int64              `yaml:"fsGroup,omitempty"`
	FSGroupChangePolicy string              `yaml:"fsGroupChangePolicy,omitempty"`
	SupplementalGroups  []int64             `yaml:"supplementalGroups,omitempty"`
	Sysctls             []SysctlSpec        `yaml:"sysctls,omitempty"`
	SeccompProfile      *SeccompProfileSpec `yaml:"seccompProfile,omitempty"`
}

// SecurityCapabilitiesSpec for manifest
type SecurityCapabilitiesSpec struct {
	Add  []string `yaml:"add,omitempty"`
	Drop []string `yaml:"drop,omitempty"`
}

// SeccompProfileSpec for manifest
type SeccompProfileSpec struct {
	Type             string `yaml:"type"`
	LocalhostProfile string `yaml:"localhostProfile,omitempty"`
}

// SELinuxOptionsSpec for manifest
type SELinuxOptionsSpec struct {
	User  string `yaml:"user,omitempty"`
	Role  string `yaml:"role,omitempty"`
	Type  string `yaml:"type,omitempty"`
	Level string `yaml:"level,omitempty"`
}

// SysctlSpec for manifest
type SysctlSpec struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// ToSpec converts SecurityContainerContext to spec format
func (csc *SecurityContainerContext) ToSpec() *ContainerSecurityContextSpec {
	if csc == nil {
		return nil
	}

	spec := &ContainerSecurityContextSpec{
		RunAsUser:                csc.RunAsUser,
		RunAsGroup:               csc.RunAsGroup,
		RunAsNonRoot:             csc.RunAsNonRoot,
		ReadOnlyRootFilesystem:   csc.ReadOnlyRootFilesystem,
		AllowPrivilegeEscalation: csc.AllowPrivilegeEscalation,
		Privileged:               csc.Privileged,
		ProcMount:                csc.ProcMount,
	}

	if csc.Capabilities != nil {
		spec.Capabilities = &SecurityCapabilitiesSpec{
			Add:  csc.Capabilities.Add,
			Drop: csc.Capabilities.Drop,
		}
	}

	if csc.SeccompProfile != nil {
		spec.SeccompProfile = &SeccompProfileSpec{
			Type:             csc.SeccompProfile.Type,
			LocalhostProfile: csc.SeccompProfile.LocalhostProfile,
		}
	}

	if csc.SELinuxOptions != nil {
		spec.SELinuxOptions = &SELinuxOptionsSpec{
			User:  csc.SELinuxOptions.User,
			Role:  csc.SELinuxOptions.Role,
			Type:  csc.SELinuxOptions.Type,
			Level: csc.SELinuxOptions.Level,
		}
	}

	return spec
}

// ToSpec converts SecurityPodContext to spec format
func (psc *SecurityPodContext) ToSpec() *SecurityPodContextSpec {
	if psc == nil {
		return nil
	}

	spec := &SecurityPodContextSpec{
		RunAsUser:           psc.RunAsUser,
		RunAsGroup:          psc.RunAsGroup,
		RunAsNonRoot:        psc.RunAsNonRoot,
		FSGroup:             psc.FSGroup,
		FSGroupChangePolicy: psc.FSGroupChangePolicy,
		SupplementalGroups:  psc.SupplementalGroups,
	}

	for _, s := range psc.Sysctls {
		spec.Sysctls = append(spec.Sysctls, SysctlSpec(s))
	}

	if psc.SeccompProfile != nil {
		spec.SeccompProfile = &SeccompProfileSpec{
			Type:             psc.SeccompProfile.Type,
			LocalhostProfile: psc.SeccompProfile.LocalhostProfile,
		}
	}

	return spec
}

// GenerateVaultaireRBAC creates RBAC resources for Vaultaire
func GenerateVaultaireRBAC(namespace string) ([]*RoleResource, []*RoleBindingResource) {
	sm := NewSecurityManager(namespace)

	roles := []*RoleResource{
		// API server role
		sm.GenerateRole(RoleConfig{
			Name: "vaultaire-api",
			Rules: []PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"configmaps", "secrets"}, Verbs: []string{"get", "list", "watch"}},
				{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
				{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
			},
		}),
		// Worker role
		sm.GenerateRole(RoleConfig{
			Name: "vaultaire-worker",
			Rules: []PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "watch"}},
				{APIGroups: []string{""}, Resources: []string{"persistentvolumeclaims"}, Verbs: []string{"get", "list", "watch", "create", "update", "delete"}},
				{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get", "list", "watch", "create", "delete"}},
			},
		}),
	}

	bindings := []*RoleBindingResource{
		sm.GenerateRoleBinding(RoleBindingConfig{
			Name: "vaultaire-api",
			RoleRef: RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     "vaultaire-api",
			},
			Subjects: []Subject{
				{Kind: "ServiceAccount", Name: "vaultaire-api", Namespace: namespace},
			},
		}),
		sm.GenerateRoleBinding(RoleBindingConfig{
			Name: "vaultaire-worker",
			RoleRef: RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     "vaultaire-worker",
			},
			Subjects: []Subject{
				{Kind: "ServiceAccount", Name: "vaultaire-worker", Namespace: namespace},
			},
		}),
	}

	return roles, bindings
}

// SecuritySet represents a collection of security resources
type SecuritySet struct {
	Roles        []*RoleResource
	RoleBindings []*RoleBindingResource
}

// ToYAML converts all security resources to multi-document YAML
func (s *SecuritySet) ToYAML() (string, error) {
	var parts []string

	for _, r := range s.Roles {
		yaml, err := r.ToYAML()
		if err != nil {
			return "", err
		}
		parts = append(parts, yaml)
	}

	for _, rb := range s.RoleBindings {
		yaml, err := rb.ToYAML()
		if err != nil {
			return "", err
		}
		parts = append(parts, yaml)
	}

	return strings.Join(parts, "---\n"), nil
}
