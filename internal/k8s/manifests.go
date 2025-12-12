// internal/k8s/manifests.go
package k8s

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

// ManifestKind represents Kubernetes resource types
type ManifestKind string

const (
	KindDeployment              ManifestKind = "Deployment"
	KindService                 ManifestKind = "Service"
	KindConfigMap               ManifestKind = "ConfigMap"
	KindSecret                  ManifestKind = "Secret"
	KindIngress                 ManifestKind = "Ingress"
	KindHorizontalPodAutoscaler ManifestKind = "HorizontalPodAutoscaler"
	KindPodDisruptionBudget     ManifestKind = "PodDisruptionBudget"
	KindServiceAccount          ManifestKind = "ServiceAccount"
	KindRole                    ManifestKind = "Role"
	KindRoleBinding             ManifestKind = "RoleBinding"
	KindClusterRole             ManifestKind = "ClusterRole"
	KindClusterRoleBinding      ManifestKind = "ClusterRoleBinding"
	KindNetworkPolicy           ManifestKind = "NetworkPolicy"
	KindPersistentVolumeClaim   ManifestKind = "PersistentVolumeClaim"
	KindStatefulSet             ManifestKind = "StatefulSet"
)

// Manifest represents a Kubernetes manifest
type Manifest struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       ManifestKind      `yaml:"kind"`
	Metadata   ManifestMetadata  `yaml:"metadata"`
	Spec       interface{}       `yaml:"spec,omitempty"`
	Data       map[string]string `yaml:"data,omitempty"`
	StringData map[string]string `yaml:"stringData,omitempty"`
	Type       string            `yaml:"type,omitempty"`
}

// ManifestMetadata contains Kubernetes metadata
type ManifestMetadata struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// DeploymentSpec represents a Kubernetes Deployment spec
type DeploymentSpec struct {
	Replicas int                 `yaml:"replicas"`
	Selector *LabelSelector      `yaml:"selector"`
	Template PodTemplateSpec     `yaml:"template"`
	Strategy *DeploymentStrategy `yaml:"strategy,omitempty"`
}

// DeploymentStrategy defines deployment update strategy
type DeploymentStrategy struct {
	Type          string                   `yaml:"type"`
	RollingUpdate *RollingUpdateDeployment `yaml:"rollingUpdate,omitempty"`
}

// RollingUpdateDeployment controls rolling update
type RollingUpdateDeployment struct {
	MaxUnavailable string `yaml:"maxUnavailable,omitempty"`
	MaxSurge       string `yaml:"maxSurge,omitempty"`
}

// LabelSelector for selecting pods
type LabelSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

// PodTemplateSpec defines pod template
type PodTemplateSpec struct {
	Metadata ManifestMetadata `yaml:"metadata"`
	Spec     PodSpec          `yaml:"spec"`
}

// PodSpec defines pod specification
type PodSpec struct {
	ServiceAccountName string              `yaml:"serviceAccountName,omitempty"`
	Containers         []Container         `yaml:"containers"`
	InitContainers     []Container         `yaml:"initContainers,omitempty"`
	Volumes            []Volume            `yaml:"volumes,omitempty"`
	NodeSelector       map[string]string   `yaml:"nodeSelector,omitempty"`
	Tolerations        []Toleration        `yaml:"tolerations,omitempty"`
	Affinity           *Affinity           `yaml:"affinity,omitempty"`
	SecurityContext    *PodSecurityContext `yaml:"securityContext,omitempty"`
}

// Container defines a container spec
type Container struct {
	Name            string               `yaml:"name"`
	Image           string               `yaml:"image"`
	ImagePullPolicy string               `yaml:"imagePullPolicy,omitempty"`
	Command         []string             `yaml:"command,omitempty"`
	Args            []string             `yaml:"args,omitempty"`
	Ports           []ContainerPort      `yaml:"ports,omitempty"`
	Env             []EnvVar             `yaml:"env,omitempty"`
	EnvFrom         []EnvFromSource      `yaml:"envFrom,omitempty"`
	Resources       ResourceRequirements `yaml:"resources,omitempty"`
	VolumeMounts    []VolumeMount        `yaml:"volumeMounts,omitempty"`
	LivenessProbe   *Probe               `yaml:"livenessProbe,omitempty"`
	ReadinessProbe  *Probe               `yaml:"readinessProbe,omitempty"`
	StartupProbe    *Probe               `yaml:"startupProbe,omitempty"`
	SecurityContext *SecurityContext     `yaml:"securityContext,omitempty"`
}

// ContainerPort defines exposed port
type ContainerPort struct {
	Name          string `yaml:"name,omitempty"`
	ContainerPort int    `yaml:"containerPort"`
	Protocol      string `yaml:"protocol,omitempty"`
}

// EnvVar defines environment variable
type EnvVar struct {
	Name      string        `yaml:"name"`
	Value     string        `yaml:"value,omitempty"`
	ValueFrom *EnvVarSource `yaml:"valueFrom,omitempty"`
}

// EnvVarSource for referencing secrets/configmaps
type EnvVarSource struct {
	SecretKeyRef    *KeySelector   `yaml:"secretKeyRef,omitempty"`
	ConfigMapKeyRef *KeySelector   `yaml:"configMapKeyRef,omitempty"`
	FieldRef        *FieldSelector `yaml:"fieldRef,omitempty"`
}

// KeySelector selects a key from secret/configmap
type KeySelector struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

// FieldSelector selects a field from pod
type FieldSelector struct {
	FieldPath string `yaml:"fieldPath"`
}

// EnvFromSource for bulk env from secrets/configmaps
type EnvFromSource struct {
	SecretRef    *SecretEnvSource    `yaml:"secretRef,omitempty"`
	ConfigMapRef *ConfigMapEnvSource `yaml:"configMapRef,omitempty"`
	Prefix       string              `yaml:"prefix,omitempty"`
}

// SecretEnvSource references a secret
type SecretEnvSource struct {
	Name string `yaml:"name"`
}

// ConfigMapEnvSource references a configmap
type ConfigMapEnvSource struct {
	Name string `yaml:"name"`
}

// ResourceRequirements defines resource limits/requests
type ResourceRequirements struct {
	Limits   map[string]string `yaml:"limits,omitempty"`
	Requests map[string]string `yaml:"requests,omitempty"`
}

// VolumeMount defines volume mount
type VolumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	SubPath   string `yaml:"subPath,omitempty"`
	ReadOnly  bool   `yaml:"readOnly,omitempty"`
}

// Volume defines a volume
type Volume struct {
	Name                  string                             `yaml:"name"`
	ConfigMap             *ConfigMapVolumeSource             `yaml:"configMap,omitempty"`
	Secret                *SecretVolumeSource                `yaml:"secret,omitempty"`
	EmptyDir              *EmptyDirVolumeSource              `yaml:"emptyDir,omitempty"`
	PersistentVolumeClaim *PersistentVolumeClaimVolumeSource `yaml:"persistentVolumeClaim,omitempty"`
}

// ConfigMapVolumeSource references configmap for volume
type ConfigMapVolumeSource struct {
	Name string `yaml:"name"`
}

// SecretVolumeSource references secret for volume
type SecretVolumeSource struct {
	SecretName string `yaml:"secretName"`
}

// EmptyDirVolumeSource for ephemeral storage
type EmptyDirVolumeSource struct {
	Medium    string `yaml:"medium,omitempty"`
	SizeLimit string `yaml:"sizeLimit,omitempty"`
}

// PersistentVolumeClaimVolumeSource references PVC
type PersistentVolumeClaimVolumeSource struct {
	ClaimName string `yaml:"claimName"`
}

// Probe defines health check probe
type Probe struct {
	HTTPGet             *HTTPGetAction   `yaml:"httpGet,omitempty"`
	TCPSocket           *TCPSocketAction `yaml:"tcpSocket,omitempty"`
	Exec                *ExecAction      `yaml:"exec,omitempty"`
	InitialDelaySeconds int              `yaml:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int              `yaml:"periodSeconds,omitempty"`
	TimeoutSeconds      int              `yaml:"timeoutSeconds,omitempty"`
	SuccessThreshold    int              `yaml:"successThreshold,omitempty"`
	FailureThreshold    int              `yaml:"failureThreshold,omitempty"`
}

// HTTPGetAction for HTTP probe
type HTTPGetAction struct {
	Path   string `yaml:"path"`
	Port   int    `yaml:"port"`
	Scheme string `yaml:"scheme,omitempty"`
}

// TCPSocketAction for TCP probe
type TCPSocketAction struct {
	Port int `yaml:"port"`
}

// ExecAction for exec probe
type ExecAction struct {
	Command []string `yaml:"command"`
}

// SecurityContext for container security
type SecurityContext struct {
	RunAsUser                int64         `yaml:"runAsUser,omitempty"`
	RunAsGroup               int64         `yaml:"runAsGroup,omitempty"`
	RunAsNonRoot             *bool         `yaml:"runAsNonRoot,omitempty"`
	ReadOnlyRootFilesystem   *bool         `yaml:"readOnlyRootFilesystem,omitempty"`
	AllowPrivilegeEscalation *bool         `yaml:"allowPrivilegeEscalation,omitempty"`
	Capabilities             *Capabilities `yaml:"capabilities,omitempty"`
}

// PodSecurityContext for pod-level security
type PodSecurityContext struct {
	RunAsUser    int64 `yaml:"runAsUser,omitempty"`
	RunAsGroup   int64 `yaml:"runAsGroup,omitempty"`
	FSGroup      int64 `yaml:"fsGroup,omitempty"`
	RunAsNonRoot *bool `yaml:"runAsNonRoot,omitempty"`
}

// Capabilities for Linux capabilities
type Capabilities struct {
	Add  []string `yaml:"add,omitempty"`
	Drop []string `yaml:"drop,omitempty"`
}

// Toleration for node tolerations
type Toleration struct {
	Key               string `yaml:"key,omitempty"`
	Operator          string `yaml:"operator,omitempty"`
	Value             string `yaml:"value,omitempty"`
	Effect            string `yaml:"effect,omitempty"`
	TolerationSeconds *int64 `yaml:"tolerationSeconds,omitempty"`
}

// Affinity for pod scheduling
type Affinity struct {
	NodeAffinity    *NodeAffinity    `yaml:"nodeAffinity,omitempty"`
	PodAffinity     *PodAffinity     `yaml:"podAffinity,omitempty"`
	PodAntiAffinity *PodAntiAffinity `yaml:"podAntiAffinity,omitempty"`
}

// NodeAffinity for node selection
type NodeAffinity struct {
	RequiredDuringSchedulingIgnoredDuringExecution  *NodeSelector             `yaml:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
	PreferredDuringSchedulingIgnoredDuringExecution []PreferredSchedulingTerm `yaml:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

// NodeSelector for node requirements
type NodeSelector struct {
	NodeSelectorTerms []NodeSelectorTerm `yaml:"nodeSelectorTerms"`
}

// NodeSelectorTerm defines node selection criteria
type NodeSelectorTerm struct {
	MatchExpressions []NodeSelectorRequirement `yaml:"matchExpressions,omitempty"`
}

// NodeSelectorRequirement for node matching
type NodeSelectorRequirement struct {
	Key      string   `yaml:"key"`
	Operator string   `yaml:"operator"`
	Values   []string `yaml:"values,omitempty"`
}

// PreferredSchedulingTerm for soft node preferences
type PreferredSchedulingTerm struct {
	Weight     int              `yaml:"weight"`
	Preference NodeSelectorTerm `yaml:"preference"`
}

// PodAffinity for pod co-location
type PodAffinity struct {
	RequiredDuringSchedulingIgnoredDuringExecution  []PodAffinityTerm         `yaml:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
	PreferredDuringSchedulingIgnoredDuringExecution []WeightedPodAffinityTerm `yaml:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

// PodAntiAffinity for pod separation
type PodAntiAffinity struct {
	RequiredDuringSchedulingIgnoredDuringExecution  []PodAffinityTerm         `yaml:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
	PreferredDuringSchedulingIgnoredDuringExecution []WeightedPodAffinityTerm `yaml:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

// PodAffinityTerm defines pod affinity criteria
type PodAffinityTerm struct {
	LabelSelector *LabelSelector `yaml:"labelSelector,omitempty"`
	TopologyKey   string         `yaml:"topologyKey"`
	Namespaces    []string       `yaml:"namespaces,omitempty"`
}

// WeightedPodAffinityTerm for soft pod preferences
type WeightedPodAffinityTerm struct {
	Weight          int             `yaml:"weight"`
	PodAffinityTerm PodAffinityTerm `yaml:"podAffinityTerm"`
}

// ServiceSpec defines Service specification
type ServiceSpec struct {
	Type                  string            `yaml:"type,omitempty"`
	Selector              map[string]string `yaml:"selector"`
	Ports                 []ServicePort     `yaml:"ports"`
	ClusterIP             string            `yaml:"clusterIP,omitempty"`
	LoadBalancerIP        string            `yaml:"loadBalancerIP,omitempty"`
	ExternalTrafficPolicy string            `yaml:"externalTrafficPolicy,omitempty"`
	SessionAffinity       string            `yaml:"sessionAffinity,omitempty"`
}

// ServicePort defines service port
type ServicePort struct {
	Name       string `yaml:"name,omitempty"`
	Protocol   string `yaml:"protocol,omitempty"`
	Port       int    `yaml:"port"`
	TargetPort int    `yaml:"targetPort,omitempty"`
	NodePort   int    `yaml:"nodePort,omitempty"`
}

// ManifestGenerator generates Kubernetes manifests
type ManifestGenerator struct {
	AppName     string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
}

// NewManifestGenerator creates a new manifest generator
func NewManifestGenerator(appName, namespace string) *ManifestGenerator {
	return &ManifestGenerator{
		AppName:   appName,
		Namespace: namespace,
		Labels: map[string]string{
			"app.kubernetes.io/name":       appName,
			"app.kubernetes.io/managed-by": "vaultaire",
		},
		Annotations: make(map[string]string),
	}
}

// WithLabels adds additional labels
func (g *ManifestGenerator) WithLabels(labels map[string]string) *ManifestGenerator {
	for k, v := range labels {
		g.Labels[k] = v
	}
	return g
}

// WithAnnotations adds additional annotations
func (g *ManifestGenerator) WithAnnotations(annotations map[string]string) *ManifestGenerator {
	for k, v := range annotations {
		g.Annotations[k] = v
	}
	return g
}

// DeploymentConfig configures deployment generation
type DeploymentConfig struct {
	Name             string
	Image            string
	Replicas         int
	Port             int
	CPURequest       string
	CPULimit         string
	MemoryRequest    string
	MemoryLimit      string
	EnvVars          map[string]string
	SecretEnvVars    map[string]string // key -> secretName:secretKey
	ConfigMapEnvVars map[string]string // key -> configMapName:configMapKey
	HealthPath       string
	HealthPort       int
	Command          []string
	Args             []string
	Volumes          []VolumeConfig
	ServiceAccount   string
}

// VolumeConfig for volume configuration
type VolumeConfig struct {
	Name      string
	MountPath string
	ConfigMap string
	Secret    string
	PVC       string
	EmptyDir  bool
	ReadOnly  bool
}

// GenerateDeployment creates a Deployment manifest
func (g *ManifestGenerator) GenerateDeployment(cfg DeploymentConfig) (*Manifest, error) {
	if cfg.Name == "" {
		cfg.Name = g.AppName
	}
	if cfg.Replicas == 0 {
		cfg.Replicas = 1
	}
	if cfg.HealthPath == "" {
		cfg.HealthPath = "/health"
	}
	if cfg.HealthPort == 0 {
		cfg.HealthPort = cfg.Port
	}

	labels := copyLabels(g.Labels)
	labels["app.kubernetes.io/component"] = cfg.Name

	container := Container{
		Name:            cfg.Name,
		Image:           cfg.Image,
		ImagePullPolicy: "IfNotPresent",
		Command:         cfg.Command,
		Args:            cfg.Args,
		Ports: []ContainerPort{
			{
				Name:          "http",
				ContainerPort: cfg.Port,
				Protocol:      "TCP",
			},
		},
		Resources: ResourceRequirements{
			Requests: map[string]string{
				"cpu":    withDefault(cfg.CPURequest, "100m"),
				"memory": withDefault(cfg.MemoryRequest, "128Mi"),
			},
			Limits: map[string]string{
				"cpu":    withDefault(cfg.CPULimit, "500m"),
				"memory": withDefault(cfg.MemoryLimit, "512Mi"),
			},
		},
		LivenessProbe: &Probe{
			HTTPGet: &HTTPGetAction{
				Path: cfg.HealthPath,
				Port: cfg.HealthPort,
			},
			InitialDelaySeconds: 30,
			PeriodSeconds:       10,
			TimeoutSeconds:      5,
			FailureThreshold:    3,
		},
		ReadinessProbe: &Probe{
			HTTPGet: &HTTPGetAction{
				Path: cfg.HealthPath,
				Port: cfg.HealthPort,
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       5,
			TimeoutSeconds:      3,
			FailureThreshold:    3,
		},
		SecurityContext: &SecurityContext{
			RunAsNonRoot:             boolPtr(true),
			ReadOnlyRootFilesystem:   boolPtr(true),
			AllowPrivilegeEscalation: boolPtr(false),
			Capabilities: &Capabilities{
				Drop: []string{"ALL"},
			},
		},
	}

	// Add environment variables
	for k, v := range cfg.EnvVars {
		container.Env = append(container.Env, EnvVar{Name: k, Value: v})
	}

	// Add secret-based env vars
	for envName, ref := range cfg.SecretEnvVars {
		parts := strings.SplitN(ref, ":", 2)
		if len(parts) == 2 {
			container.Env = append(container.Env, EnvVar{
				Name: envName,
				ValueFrom: &EnvVarSource{
					SecretKeyRef: &KeySelector{
						Name: parts[0],
						Key:  parts[1],
					},
				},
			})
		}
	}

	// Add configmap-based env vars
	for envName, ref := range cfg.ConfigMapEnvVars {
		parts := strings.SplitN(ref, ":", 2)
		if len(parts) == 2 {
			container.Env = append(container.Env, EnvVar{
				Name: envName,
				ValueFrom: &EnvVarSource{
					ConfigMapKeyRef: &KeySelector{
						Name: parts[0],
						Key:  parts[1],
					},
				},
			})
		}
	}

	// Add volumes
	var volumes []Volume
	for _, vol := range cfg.Volumes {
		container.VolumeMounts = append(container.VolumeMounts, VolumeMount{
			Name:      vol.Name,
			MountPath: vol.MountPath,
			ReadOnly:  vol.ReadOnly,
		})

		v := Volume{Name: vol.Name}
		switch {
		case vol.ConfigMap != "":
			v.ConfigMap = &ConfigMapVolumeSource{Name: vol.ConfigMap}
		case vol.Secret != "":
			v.Secret = &SecretVolumeSource{SecretName: vol.Secret}
		case vol.PVC != "":
			v.PersistentVolumeClaim = &PersistentVolumeClaimVolumeSource{ClaimName: vol.PVC}
		case vol.EmptyDir:
			v.EmptyDir = &EmptyDirVolumeSource{}
		}
		volumes = append(volumes, v)
	}

	spec := DeploymentSpec{
		Replicas: cfg.Replicas,
		Selector: &LabelSelector{
			MatchLabels: map[string]string{
				"app.kubernetes.io/name":      g.AppName,
				"app.kubernetes.io/component": cfg.Name,
			},
		},
		Strategy: &DeploymentStrategy{
			Type: "RollingUpdate",
			RollingUpdate: &RollingUpdateDeployment{
				MaxUnavailable: "25%",
				MaxSurge:       "25%",
			},
		},
		Template: PodTemplateSpec{
			Metadata: ManifestMetadata{
				Labels:      labels,
				Annotations: copyLabels(g.Annotations),
			},
			Spec: PodSpec{
				ServiceAccountName: cfg.ServiceAccount,
				Containers:         []Container{container},
				Volumes:            volumes,
				SecurityContext: &PodSecurityContext{
					RunAsNonRoot: boolPtr(true),
					FSGroup:      1000,
				},
			},
		},
	}

	return &Manifest{
		APIVersion: "apps/v1",
		Kind:       KindDeployment,
		Metadata: ManifestMetadata{
			Name:        cfg.Name,
			Namespace:   g.Namespace,
			Labels:      labels,
			Annotations: g.Annotations,
		},
		Spec: spec,
	}, nil
}

// GenerateService creates a Service manifest
func (g *ManifestGenerator) GenerateService(name string, port, targetPort int, serviceType string) *Manifest {
	labels := copyLabels(g.Labels)
	labels["app.kubernetes.io/component"] = name

	if serviceType == "" {
		serviceType = "ClusterIP"
	}

	spec := ServiceSpec{
		Type: serviceType,
		Selector: map[string]string{
			"app.kubernetes.io/name":      g.AppName,
			"app.kubernetes.io/component": name,
		},
		Ports: []ServicePort{
			{
				Name:       "http",
				Protocol:   "TCP",
				Port:       port,
				TargetPort: targetPort,
			},
		},
	}

	return &Manifest{
		APIVersion: "v1",
		Kind:       KindService,
		Metadata: ManifestMetadata{
			Name:        name,
			Namespace:   g.Namespace,
			Labels:      labels,
			Annotations: g.Annotations,
		},
		Spec: spec,
	}
}

// GenerateConfigMap creates a ConfigMap manifest
func (g *ManifestGenerator) GenerateConfigMap(name string, data map[string]string) *Manifest {
	labels := copyLabels(g.Labels)

	return &Manifest{
		APIVersion: "v1",
		Kind:       KindConfigMap,
		Metadata: ManifestMetadata{
			Name:        name,
			Namespace:   g.Namespace,
			Labels:      labels,
			Annotations: g.Annotations,
		},
		Data: data,
	}
}

// GenerateSecret creates a Secret manifest
func (g *ManifestGenerator) GenerateSecret(name string, data map[string]string, secretType string) *Manifest {
	labels := copyLabels(g.Labels)

	if secretType == "" {
		secretType = "Opaque"
	}

	return &Manifest{
		APIVersion: "v1",
		Kind:       KindSecret,
		Metadata: ManifestMetadata{
			Name:        name,
			Namespace:   g.Namespace,
			Labels:      labels,
			Annotations: g.Annotations,
		},
		StringData: data,
		Type:       secretType,
	}
}

// GenerateServiceAccount creates a ServiceAccount manifest
func (g *ManifestGenerator) GenerateServiceAccount(name string) *Manifest {
	labels := copyLabels(g.Labels)

	return &Manifest{
		APIVersion: "v1",
		Kind:       KindServiceAccount,
		Metadata: ManifestMetadata{
			Name:        name,
			Namespace:   g.Namespace,
			Labels:      labels,
			Annotations: g.Annotations,
		},
	}
}

// ToYAML converts manifest to YAML string
func (m *Manifest) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(m); err != nil {
		return "", fmt.Errorf("failed to encode manifest: %w", err)
	}
	return buf.String(), nil
}

// ManifestSet represents a collection of manifests
type ManifestSet struct {
	Manifests []*Manifest
}

// Add adds a manifest to the set
func (ms *ManifestSet) Add(m *Manifest) {
	ms.Manifests = append(ms.Manifests, m)
}

// ToYAML converts all manifests to a single YAML document
func (ms *ManifestSet) ToYAML() (string, error) {
	var parts []string
	for _, m := range ms.Manifests {
		yaml, err := m.ToYAML()
		if err != nil {
			return "", err
		}
		parts = append(parts, yaml)
	}
	return strings.Join(parts, "---\n"), nil
}

// GenerateVaultaireManifests generates all manifests for Vaultaire deployment
func GenerateVaultaireManifests(namespace, image string, replicas int) (*ManifestSet, error) {
	gen := NewManifestGenerator("vaultaire", namespace)
	gen.WithLabels(map[string]string{
		"app.kubernetes.io/version": "1.0.0",
	})

	ms := &ManifestSet{}

	// ServiceAccount
	ms.Add(gen.GenerateServiceAccount("vaultaire"))

	// ConfigMap for configuration
	ms.Add(gen.GenerateConfigMap("vaultaire-config", map[string]string{
		"config.yaml": `
server:
  port: 8080
  host: 0.0.0.0
storage:
  type: s3
  endpoint: ${S3_ENDPOINT}
database:
  host: ${DB_HOST}
  port: "5432"
  name: vaultaire
`,
	}))

	// Secret for sensitive data
	ms.Add(gen.GenerateSecret("vaultaire-secrets", map[string]string{
		"db-password":   "CHANGE_ME",
		"s3-access-key": "CHANGE_ME",
		"s3-secret-key": "CHANGE_ME",
		"jwt-secret":    "CHANGE_ME",
	}, "Opaque"))

	// Deployment
	deployment, err := gen.GenerateDeployment(DeploymentConfig{
		Name:           "vaultaire",
		Image:          image,
		Replicas:       replicas,
		Port:           8080,
		CPURequest:     "200m",
		CPULimit:       "1000m",
		MemoryRequest:  "256Mi",
		MemoryLimit:    "1Gi",
		HealthPath:     "/health",
		HealthPort:     8080,
		ServiceAccount: "vaultaire",
		EnvVars: map[string]string{
			"VAULTAIRE_ENV": "production",
		},
		SecretEnvVars: map[string]string{
			"DB_PASSWORD":   "vaultaire-secrets:db-password",
			"S3_ACCESS_KEY": "vaultaire-secrets:s3-access-key",
			"S3_SECRET_KEY": "vaultaire-secrets:s3-secret-key",
			"JWT_SECRET":    "vaultaire-secrets:jwt-secret",
		},
		Volumes: []VolumeConfig{
			{
				Name:      "config",
				MountPath: "/etc/vaultaire",
				ConfigMap: "vaultaire-config",
				ReadOnly:  true,
			},
			{
				Name:      "tmp",
				MountPath: "/tmp",
				EmptyDir:  true,
			},
		},
	})
	if err != nil {
		return nil, err
	}
	ms.Add(deployment)

	// Service
	ms.Add(gen.GenerateService("vaultaire", 80, 8080, "ClusterIP"))

	return ms, nil
}

// Helper functions
func copyLabels(labels map[string]string) map[string]string {
	result := make(map[string]string)
	for k, v := range labels {
		result[k] = v
	}
	return result
}

func withDefault(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}

func boolPtr(b bool) *bool {
	return &b
}

// ManifestTemplate for custom templates
type ManifestTemplate struct {
	tmpl *template.Template
}

// NewManifestTemplate creates a new manifest template
func NewManifestTemplate(name, content string) (*ManifestTemplate, error) {
	tmpl, err := template.New(name).Parse(content)
	if err != nil {
		return nil, fmt.Errorf("failed to parse template: %w", err)
	}
	return &ManifestTemplate{tmpl: tmpl}, nil
}

// Execute renders the template with given data
func (mt *ManifestTemplate) Execute(data interface{}) (string, error) {
	var buf bytes.Buffer
	if err := mt.tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}
	return buf.String(), nil
}

// GeneratedAt returns generation timestamp for manifest comments
func GeneratedAt() string {
	return fmt.Sprintf("# Generated by Vaultaire at %s\n", time.Now().Format(time.RFC3339))
}
