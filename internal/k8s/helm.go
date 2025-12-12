// internal/k8s/helm.go
package k8s

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// HelmChart represents a Helm chart structure
type HelmChart struct {
	Name         string
	Version      string
	AppVersion   string
	Description  string
	Type         string // application or library
	Keywords     []string
	Home         string
	Sources      []string
	Maintainers  []ChartMaintainer
	Icon         string
	Deprecated   bool
	KubeVersion  string
	Dependencies []ChartDependency
}

// ChartMaintainer represents a chart maintainer
type ChartMaintainer struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email,omitempty"`
	URL   string `yaml:"url,omitempty"`
}

// ChartDependency represents a chart dependency
type ChartDependency struct {
	Name       string   `yaml:"name"`
	Version    string   `yaml:"version"`
	Repository string   `yaml:"repository"`
	Condition  string   `yaml:"condition,omitempty"`
	Tags       []string `yaml:"tags,omitempty"`
	Alias      string   `yaml:"alias,omitempty"`
}

// ChartYAML represents Chart.yaml structure
type ChartYAML struct {
	APIVersion   string            `yaml:"apiVersion"`
	Name         string            `yaml:"name"`
	Version      string            `yaml:"version"`
	AppVersion   string            `yaml:"appVersion,omitempty"`
	Description  string            `yaml:"description,omitempty"`
	Type         string            `yaml:"type,omitempty"`
	Keywords     []string          `yaml:"keywords,omitempty"`
	Home         string            `yaml:"home,omitempty"`
	Sources      []string          `yaml:"sources,omitempty"`
	Maintainers  []ChartMaintainer `yaml:"maintainers,omitempty"`
	Icon         string            `yaml:"icon,omitempty"`
	Deprecated   bool              `yaml:"deprecated,omitempty"`
	KubeVersion  string            `yaml:"kubeVersion,omitempty"`
	Dependencies []ChartDependency `yaml:"dependencies,omitempty"`
}

// HelmValues represents values.yaml structure for Vaultaire
type HelmValues struct {
	ReplicaCount       int                      `yaml:"replicaCount"`
	Image              ImageConfig              `yaml:"image"`
	ImagePullSecrets   []NameRef                `yaml:"imagePullSecrets,omitempty"`
	NameOverride       string                   `yaml:"nameOverride,omitempty"`
	FullnameOverride   string                   `yaml:"fullnameOverride,omitempty"`
	ServiceAccount     ServiceAccountConfig     `yaml:"serviceAccount"`
	PodAnnotations     map[string]string        `yaml:"podAnnotations,omitempty"`
	PodSecurityContext PodSecurityContextConfig `yaml:"podSecurityContext,omitempty"`
	SecurityContext    SecurityContextConfig    `yaml:"securityContext,omitempty"`
	Service            ServiceConfig            `yaml:"service"`
	Ingress            IngressConfig            `yaml:"ingress"`
	Resources          ResourceConfig           `yaml:"resources,omitempty"`
	Autoscaling        AutoscalingConfig        `yaml:"autoscaling"`
	NodeSelector       map[string]string        `yaml:"nodeSelector,omitempty"`
	Tolerations        []TolerationConfig       `yaml:"tolerations,omitempty"`
	Affinity           map[string]interface{}   `yaml:"affinity,omitempty"`
	Env                []EnvConfig              `yaml:"env,omitempty"`
	EnvFrom            []EnvFromConfig          `yaml:"envFrom,omitempty"`
	Persistence        PersistenceConfig        `yaml:"persistence"`
	Config             map[string]interface{}   `yaml:"config,omitempty"`
	Secrets            map[string]string        `yaml:"secrets,omitempty"`
	Probes             ProbesConfig             `yaml:"probes"`
	Metrics            MetricsConfig            `yaml:"metrics"`
	PostgreSQL         PostgreSQLConfig         `yaml:"postgresql"`
	Redis              RedisConfig              `yaml:"redis"`
}

// ImageConfig for container image settings
type ImageConfig struct {
	Repository string `yaml:"repository"`
	Tag        string `yaml:"tag"`
	PullPolicy string `yaml:"pullPolicy"`
}

// NameRef for simple name references
type NameRef struct {
	Name string `yaml:"name"`
}

// ServiceAccountConfig for service account settings
type ServiceAccountConfig struct {
	Create      bool              `yaml:"create"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
	Name        string            `yaml:"name,omitempty"`
}

// PodSecurityContextConfig for pod security
type PodSecurityContextConfig struct {
	FSGroup      int64 `yaml:"fsGroup,omitempty"`
	RunAsUser    int64 `yaml:"runAsUser,omitempty"`
	RunAsGroup   int64 `yaml:"runAsGroup,omitempty"`
	RunAsNonRoot bool  `yaml:"runAsNonRoot,omitempty"`
}

// SecurityContextConfig for container security
type SecurityContextConfig struct {
	AllowPrivilegeEscalation bool      `yaml:"allowPrivilegeEscalation"`
	ReadOnlyRootFilesystem   bool      `yaml:"readOnlyRootFilesystem"`
	RunAsNonRoot             bool      `yaml:"runAsNonRoot"`
	RunAsUser                int64     `yaml:"runAsUser,omitempty"`
	Capabilities             CapConfig `yaml:"capabilities,omitempty"`
}

// CapConfig for Linux capabilities
type CapConfig struct {
	Drop []string `yaml:"drop,omitempty"`
	Add  []string `yaml:"add,omitempty"`
}

// ServiceConfig for Kubernetes service
type ServiceConfig struct {
	Type string `yaml:"type"`
	Port int    `yaml:"port"`
}

// IngressConfig for Kubernetes ingress
type IngressConfig struct {
	Enabled     bool              `yaml:"enabled"`
	ClassName   string            `yaml:"className,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
	Hosts       []IngressHost     `yaml:"hosts,omitempty"`
	TLS         []IngressTLS      `yaml:"tls,omitempty"`
}

// IngressHost for ingress host config
type IngressHost struct {
	Host  string        `yaml:"host"`
	Paths []IngressPath `yaml:"paths"`
}

// IngressPath for ingress path config
type IngressPath struct {
	Path     string `yaml:"path"`
	PathType string `yaml:"pathType"`
}

// IngressTLS for ingress TLS config
type IngressTLS struct {
	SecretName string   `yaml:"secretName"`
	Hosts      []string `yaml:"hosts"`
}

// ResourceConfig for resource limits/requests
type ResourceConfig struct {
	Limits   map[string]string `yaml:"limits,omitempty"`
	Requests map[string]string `yaml:"requests,omitempty"`
}

// AutoscalingConfig for HPA
type AutoscalingConfig struct {
	Enabled                           bool `yaml:"enabled"`
	MinReplicas                       int  `yaml:"minReplicas"`
	MaxReplicas                       int  `yaml:"maxReplicas"`
	TargetCPUUtilizationPercentage    int  `yaml:"targetCPUUtilizationPercentage,omitempty"`
	TargetMemoryUtilizationPercentage int  `yaml:"targetMemoryUtilizationPercentage,omitempty"`
}

// TolerationConfig for node tolerations
type TolerationConfig struct {
	Key      string `yaml:"key,omitempty"`
	Operator string `yaml:"operator,omitempty"`
	Value    string `yaml:"value,omitempty"`
	Effect   string `yaml:"effect,omitempty"`
}

// EnvConfig for environment variables
type EnvConfig struct {
	Name      string        `yaml:"name"`
	Value     string        `yaml:"value,omitempty"`
	ValueFrom *EnvValueFrom `yaml:"valueFrom,omitempty"`
}

// EnvValueFrom for env var sources
type EnvValueFrom struct {
	SecretKeyRef    *KeyRefConfig `yaml:"secretKeyRef,omitempty"`
	ConfigMapKeyRef *KeyRefConfig `yaml:"configMapKeyRef,omitempty"`
}

// KeyRefConfig for key references
type KeyRefConfig struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

// EnvFromConfig for bulk env sources
type EnvFromConfig struct {
	SecretRef    *NameRef `yaml:"secretRef,omitempty"`
	ConfigMapRef *NameRef `yaml:"configMapRef,omitempty"`
}

// PersistenceConfig for persistent storage
type PersistenceConfig struct {
	Enabled      bool              `yaml:"enabled"`
	StorageClass string            `yaml:"storageClass,omitempty"`
	AccessModes  []string          `yaml:"accessModes"`
	Size         string            `yaml:"size"`
	Annotations  map[string]string `yaml:"annotations,omitempty"`
}

// ProbesConfig for health probes
type ProbesConfig struct {
	Liveness  ProbeConfig `yaml:"liveness"`
	Readiness ProbeConfig `yaml:"readiness"`
	Startup   ProbeConfig `yaml:"startup,omitempty"`
}

// ProbeConfig for individual probe
type ProbeConfig struct {
	Enabled             bool   `yaml:"enabled"`
	Path                string `yaml:"path,omitempty"`
	Port                int    `yaml:"port,omitempty"`
	InitialDelaySeconds int    `yaml:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int    `yaml:"periodSeconds,omitempty"`
	TimeoutSeconds      int    `yaml:"timeoutSeconds,omitempty"`
	FailureThreshold    int    `yaml:"failureThreshold,omitempty"`
	SuccessThreshold    int    `yaml:"successThreshold,omitempty"`
}

// MetricsConfig for Prometheus metrics
type MetricsConfig struct {
	Enabled        bool                 `yaml:"enabled"`
	Port           int                  `yaml:"port"`
	ServiceMonitor ServiceMonitorConfig `yaml:"serviceMonitor"`
}

// ServiceMonitorConfig for Prometheus ServiceMonitor
type ServiceMonitorConfig struct {
	Enabled   bool              `yaml:"enabled"`
	Namespace string            `yaml:"namespace,omitempty"`
	Labels    map[string]string `yaml:"labels,omitempty"`
	Interval  string            `yaml:"interval,omitempty"`
}

// PostgreSQLConfig for PostgreSQL subchart
type PostgreSQLConfig struct {
	Enabled  bool              `yaml:"enabled"`
	Auth     PostgreSQLAuth    `yaml:"auth,omitempty"`
	Primary  PostgreSQLPrimary `yaml:"primary,omitempty"`
	External ExternalDB        `yaml:"external,omitempty"`
}

// PostgreSQLAuth for PostgreSQL authentication
type PostgreSQLAuth struct {
	Username       string            `yaml:"username,omitempty"`
	Password       string            `yaml:"password,omitempty"`
	Database       string            `yaml:"database,omitempty"`
	ExistingSecret string            `yaml:"existingSecret,omitempty"`
	SecretKeys     map[string]string `yaml:"secretKeys,omitempty"`
}

// PostgreSQLPrimary for PostgreSQL primary config
type PostgreSQLPrimary struct {
	Persistence PersistenceConfig `yaml:"persistence,omitempty"`
}

// ExternalDB for external database config
type ExternalDB struct {
	Host     string `yaml:"host,omitempty"`
	Port     int    `yaml:"port,omitempty"`
	Database string `yaml:"database,omitempty"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
}

// RedisConfig for Redis subchart
type RedisConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Auth     RedisAuth     `yaml:"auth,omitempty"`
	Master   RedisMaster   `yaml:"master,omitempty"`
	External ExternalRedis `yaml:"external,omitempty"`
}

// RedisAuth for Redis authentication
type RedisAuth struct {
	Enabled        bool   `yaml:"enabled"`
	Password       string `yaml:"password,omitempty"`
	ExistingSecret string `yaml:"existingSecret,omitempty"`
}

// RedisMaster for Redis master config
type RedisMaster struct {
	Persistence PersistenceConfig `yaml:"persistence,omitempty"`
}

// ExternalRedis for external Redis config
type ExternalRedis struct {
	Host     string `yaml:"host,omitempty"`
	Port     int    `yaml:"port,omitempty"`
	Password string `yaml:"password,omitempty"`
}

// HelmChartGenerator generates Helm charts
type HelmChartGenerator struct {
	Chart  HelmChart
	Values HelmValues
}

// NewHelmChartGenerator creates a new Helm chart generator
func NewHelmChartGenerator(name, version, appVersion string) *HelmChartGenerator {
	return &HelmChartGenerator{
		Chart: HelmChart{
			Name:        name,
			Version:     version,
			AppVersion:  appVersion,
			Description: fmt.Sprintf("A Helm chart for %s", name),
			Type:        "application",
			KubeVersion: ">= 1.19.0-0",
		},
		Values: DefaultHelmValues(),
	}
}

// DefaultHelmValues returns default values for Vaultaire
func DefaultHelmValues() HelmValues {
	return HelmValues{
		ReplicaCount: 1,
		Image: ImageConfig{
			Repository: "ghcr.io/fairforge/vaultaire",
			Tag:        "latest",
			PullPolicy: "IfNotPresent",
		},
		ServiceAccount: ServiceAccountConfig{
			Create: true,
		},
		PodSecurityContext: PodSecurityContextConfig{
			FSGroup:      1000,
			RunAsNonRoot: true,
		},
		SecurityContext: SecurityContextConfig{
			AllowPrivilegeEscalation: false,
			ReadOnlyRootFilesystem:   true,
			RunAsNonRoot:             true,
			RunAsUser:                1000,
			Capabilities: CapConfig{
				Drop: []string{"ALL"},
			},
		},
		Service: ServiceConfig{
			Type: "ClusterIP",
			Port: 80,
		},
		Ingress: IngressConfig{
			Enabled: false,
			Hosts: []IngressHost{
				{
					Host: "vaultaire.local",
					Paths: []IngressPath{
						{Path: "/", PathType: "Prefix"},
					},
				},
			},
		},
		Resources: ResourceConfig{
			Limits: map[string]string{
				"cpu":    "1000m",
				"memory": "1Gi",
			},
			Requests: map[string]string{
				"cpu":    "100m",
				"memory": "128Mi",
			},
		},
		Autoscaling: AutoscalingConfig{
			Enabled:                        false,
			MinReplicas:                    1,
			MaxReplicas:                    10,
			TargetCPUUtilizationPercentage: 80,
		},
		Persistence: PersistenceConfig{
			Enabled:     false,
			AccessModes: []string{"ReadWriteOnce"},
			Size:        "10Gi",
		},
		Probes: ProbesConfig{
			Liveness: ProbeConfig{
				Enabled:             true,
				Path:                "/health",
				Port:                8080,
				InitialDelaySeconds: 30,
				PeriodSeconds:       10,
				TimeoutSeconds:      5,
				FailureThreshold:    3,
			},
			Readiness: ProbeConfig{
				Enabled:             true,
				Path:                "/ready",
				Port:                8080,
				InitialDelaySeconds: 5,
				PeriodSeconds:       5,
				TimeoutSeconds:      3,
				FailureThreshold:    3,
			},
		},
		Metrics: MetricsConfig{
			Enabled: true,
			Port:    9090,
			ServiceMonitor: ServiceMonitorConfig{
				Enabled:  false,
				Interval: "30s",
			},
		},
		PostgreSQL: PostgreSQLConfig{
			Enabled: true,
			Auth: PostgreSQLAuth{
				Username: "vaultaire",
				Database: "vaultaire",
			},
			Primary: PostgreSQLPrimary{
				Persistence: PersistenceConfig{
					Enabled:     true,
					Size:        "10Gi",
					AccessModes: []string{"ReadWriteOnce"},
				},
			},
		},
		Redis: RedisConfig{
			Enabled: true,
			Auth: RedisAuth{
				Enabled: true,
			},
			Master: RedisMaster{
				Persistence: PersistenceConfig{
					Enabled:     true,
					Size:        "1Gi",
					AccessModes: []string{"ReadWriteOnce"},
				},
			},
		},
	}
}

// WithMaintainer adds a maintainer to the chart
func (g *HelmChartGenerator) WithMaintainer(name, email, url string) *HelmChartGenerator {
	g.Chart.Maintainers = append(g.Chart.Maintainers, ChartMaintainer{
		Name:  name,
		Email: email,
		URL:   url,
	})
	return g
}

// WithDependency adds a dependency to the chart
func (g *HelmChartGenerator) WithDependency(name, version, repo string) *HelmChartGenerator {
	g.Chart.Dependencies = append(g.Chart.Dependencies, ChartDependency{
		Name:       name,
		Version:    version,
		Repository: repo,
	})
	return g
}

// WithKeywords adds keywords to the chart
func (g *HelmChartGenerator) WithKeywords(keywords ...string) *HelmChartGenerator {
	g.Chart.Keywords = append(g.Chart.Keywords, keywords...)
	return g
}

// GenerateChartYAML generates Chart.yaml content
func (g *HelmChartGenerator) GenerateChartYAML() (string, error) {
	chart := ChartYAML{
		APIVersion:   "v2",
		Name:         g.Chart.Name,
		Version:      g.Chart.Version,
		AppVersion:   g.Chart.AppVersion,
		Description:  g.Chart.Description,
		Type:         g.Chart.Type,
		Keywords:     g.Chart.Keywords,
		Home:         g.Chart.Home,
		Sources:      g.Chart.Sources,
		Maintainers:  g.Chart.Maintainers,
		Icon:         g.Chart.Icon,
		KubeVersion:  g.Chart.KubeVersion,
		Dependencies: g.Chart.Dependencies,
	}

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(chart); err != nil {
		return "", fmt.Errorf("failed to encode Chart.yaml: %w", err)
	}
	return buf.String(), nil
}

// GenerateValuesYAML generates values.yaml content
func (g *HelmChartGenerator) GenerateValuesYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(g.Values); err != nil {
		return "", fmt.Errorf("failed to encode values.yaml: %w", err)
	}
	return buf.String(), nil
}

// HelmTemplate represents a Helm template file
type HelmTemplate struct {
	Name    string
	Content string
}

// GenerateTemplates generates all Helm template files
func (g *HelmChartGenerator) GenerateTemplates() []HelmTemplate {
	return []HelmTemplate{
		{Name: "_helpers.tpl", Content: g.generateHelpersTpl()},
		{Name: "serviceaccount.yaml", Content: g.generateServiceAccountTpl()},
		{Name: "configmap.yaml", Content: g.generateConfigMapTpl()},
		{Name: "secret.yaml", Content: g.generateSecretTpl()},
		{Name: "deployment.yaml", Content: g.generateDeploymentTpl()},
		{Name: "service.yaml", Content: g.generateServiceTpl()},
		{Name: "ingress.yaml", Content: g.generateIngressTpl()},
		{Name: "hpa.yaml", Content: g.generateHPATpl()},
		{Name: "pvc.yaml", Content: g.generatePVCTpl()},
		{Name: "servicemonitor.yaml", Content: g.generateServiceMonitorTpl()},
		{Name: "NOTES.txt", Content: g.generateNotesTpl()},
	}
}

func (g *HelmChartGenerator) generateHelpersTpl() string {
	return `{{/*
Expand the name of the chart.
*/}}
{{- define "` + g.Chart.Name + `.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "` + g.Chart.Name + `.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "` + g.Chart.Name + `.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "` + g.Chart.Name + `.labels" -}}
helm.sh/chart: {{ include "` + g.Chart.Name + `.chart" . }}
{{ include "` + g.Chart.Name + `.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "` + g.Chart.Name + `.selectorLabels" -}}
app.kubernetes.io/name: {{ include "` + g.Chart.Name + `.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "` + g.Chart.Name + `.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "` + g.Chart.Name + `.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Return the proper image name
*/}}
{{- define "` + g.Chart.Name + `.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}
`
}

func (g *HelmChartGenerator) generateServiceAccountTpl() string {
	return `{{- if .Values.serviceAccount.create -}}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "` + g.Chart.Name + `.serviceAccountName" . }}
  labels:
    {{- include "` + g.Chart.Name + `.labels" . | nindent 4 }}
  {{- with .Values.serviceAccount.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end }}
`
}

func (g *HelmChartGenerator) generateConfigMapTpl() string {
	return `{{- if .Values.config }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "` + g.Chart.Name + `.fullname" . }}-config
  labels:
    {{- include "` + g.Chart.Name + `.labels" . | nindent 4 }}
data:
  {{- range $key, $value := .Values.config }}
  {{ $key }}: |
    {{- $value | nindent 4 }}
  {{- end }}
{{- end }}
`
}

func (g *HelmChartGenerator) generateSecretTpl() string {
	return `{{- if .Values.secrets }}
apiVersion: v1
kind: Secret
metadata:
  name: {{ include "` + g.Chart.Name + `.fullname" . }}-secrets
  labels:
    {{- include "` + g.Chart.Name + `.labels" . | nindent 4 }}
type: Opaque
stringData:
  {{- range $key, $value := .Values.secrets }}
  {{ $key }}: {{ $value | quote }}
  {{- end }}
{{- end }}
`
}

func (g *HelmChartGenerator) generateDeploymentTpl() string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "` + g.Chart.Name + `.fullname" . }}
  labels:
    {{- include "` + g.Chart.Name + `.labels" . | nindent 4 }}
spec:
  {{- if not .Values.autoscaling.enabled }}
  replicas: {{ .Values.replicaCount }}
  {{- end }}
  selector:
    matchLabels:
      {{- include "` + g.Chart.Name + `.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      {{- with .Values.podAnnotations }}
      annotations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      labels:
        {{- include "` + g.Chart.Name + `.selectorLabels" . | nindent 8 }}
    spec:
      {{- with .Values.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      serviceAccountName: {{ include "` + g.Chart.Name + `.serviceAccountName" . }}
      securityContext:
        {{- toYaml .Values.podSecurityContext | nindent 8 }}
      containers:
        - name: {{ .Chart.Name }}
          securityContext:
            {{- toYaml .Values.securityContext | nindent 12 }}
          image: {{ include "` + g.Chart.Name + `.image" . }}
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          ports:
            - name: http
              containerPort: 8080
              protocol: TCP
            {{- if .Values.metrics.enabled }}
            - name: metrics
              containerPort: {{ .Values.metrics.port }}
              protocol: TCP
            {{- end }}
          {{- if .Values.probes.liveness.enabled }}
          livenessProbe:
            httpGet:
              path: {{ .Values.probes.liveness.path }}
              port: {{ .Values.probes.liveness.port }}
            initialDelaySeconds: {{ .Values.probes.liveness.initialDelaySeconds }}
            periodSeconds: {{ .Values.probes.liveness.periodSeconds }}
            timeoutSeconds: {{ .Values.probes.liveness.timeoutSeconds }}
            failureThreshold: {{ .Values.probes.liveness.failureThreshold }}
          {{- end }}
          {{- if .Values.probes.readiness.enabled }}
          readinessProbe:
            httpGet:
              path: {{ .Values.probes.readiness.path }}
              port: {{ .Values.probes.readiness.port }}
            initialDelaySeconds: {{ .Values.probes.readiness.initialDelaySeconds }}
            periodSeconds: {{ .Values.probes.readiness.periodSeconds }}
            timeoutSeconds: {{ .Values.probes.readiness.timeoutSeconds }}
            failureThreshold: {{ .Values.probes.readiness.failureThreshold }}
          {{- end }}
          {{- with .Values.env }}
          env:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          {{- with .Values.envFrom }}
          envFrom:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          resources:
            {{- toYaml .Values.resources | nindent 12 }}
          volumeMounts:
            - name: tmp
              mountPath: /tmp
            {{- if .Values.config }}
            - name: config
              mountPath: /etc/` + g.Chart.Name + `
              readOnly: true
            {{- end }}
            {{- if .Values.persistence.enabled }}
            - name: data
              mountPath: /data
            {{- end }}
      volumes:
        - name: tmp
          emptyDir: {}
        {{- if .Values.config }}
        - name: config
          configMap:
            name: {{ include "` + g.Chart.Name + `.fullname" . }}-config
        {{- end }}
        {{- if .Values.persistence.enabled }}
        - name: data
          persistentVolumeClaim:
            claimName: {{ include "` + g.Chart.Name + `.fullname" . }}-data
        {{- end }}
      {{- with .Values.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
`
}

func (g *HelmChartGenerator) generateServiceTpl() string {
	return `apiVersion: v1
kind: Service
metadata:
  name: {{ include "` + g.Chart.Name + `.fullname" . }}
  labels:
    {{- include "` + g.Chart.Name + `.labels" . | nindent 4 }}
spec:
  type: {{ .Values.service.type }}
  ports:
    - port: {{ .Values.service.port }}
      targetPort: http
      protocol: TCP
      name: http
    {{- if .Values.metrics.enabled }}
    - port: {{ .Values.metrics.port }}
      targetPort: metrics
      protocol: TCP
      name: metrics
    {{- end }}
  selector:
    {{- include "` + g.Chart.Name + `.selectorLabels" . | nindent 4 }}
`
}

func (g *HelmChartGenerator) generateIngressTpl() string {
	return `{{- if .Values.ingress.enabled -}}
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ include "` + g.Chart.Name + `.fullname" . }}
  labels:
    {{- include "` + g.Chart.Name + `.labels" . | nindent 4 }}
  {{- with .Values.ingress.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  {{- if .Values.ingress.className }}
  ingressClassName: {{ .Values.ingress.className }}
  {{- end }}
  {{- if .Values.ingress.tls }}
  tls:
    {{- range .Values.ingress.tls }}
    - hosts:
        {{- range .hosts }}
        - {{ . | quote }}
        {{- end }}
      secretName: {{ .secretName }}
    {{- end }}
  {{- end }}
  rules:
    {{- range .Values.ingress.hosts }}
    - host: {{ .host | quote }}
      http:
        paths:
          {{- range .paths }}
          - path: {{ .path }}
            pathType: {{ .pathType }}
            backend:
              service:
                name: {{ include "` + g.Chart.Name + `.fullname" $ }}
                port:
                  number: {{ $.Values.service.port }}
          {{- end }}
    {{- end }}
{{- end }}
`
}

func (g *HelmChartGenerator) generateHPATpl() string {
	return `{{- if .Values.autoscaling.enabled }}
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: {{ include "` + g.Chart.Name + `.fullname" . }}
  labels:
    {{- include "` + g.Chart.Name + `.labels" . | nindent 4 }}
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: {{ include "` + g.Chart.Name + `.fullname" . }}
  minReplicas: {{ .Values.autoscaling.minReplicas }}
  maxReplicas: {{ .Values.autoscaling.maxReplicas }}
  metrics:
    {{- if .Values.autoscaling.targetCPUUtilizationPercentage }}
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: {{ .Values.autoscaling.targetCPUUtilizationPercentage }}
    {{- end }}
    {{- if .Values.autoscaling.targetMemoryUtilizationPercentage }}
    - type: Resource
      resource:
        name: memory
        target:
          type: Utilization
          averageUtilization: {{ .Values.autoscaling.targetMemoryUtilizationPercentage }}
    {{- end }}
{{- end }}
`
}

func (g *HelmChartGenerator) generatePVCTpl() string {
	return `{{- if .Values.persistence.enabled }}
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: {{ include "` + g.Chart.Name + `.fullname" . }}-data
  labels:
    {{- include "` + g.Chart.Name + `.labels" . | nindent 4 }}
  {{- with .Values.persistence.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  accessModes:
    {{- range .Values.persistence.accessModes }}
    - {{ . | quote }}
    {{- end }}
  {{- if .Values.persistence.storageClass }}
  storageClassName: {{ .Values.persistence.storageClass | quote }}
  {{- end }}
  resources:
    requests:
      storage: {{ .Values.persistence.size | quote }}
{{- end }}
`
}

func (g *HelmChartGenerator) generateServiceMonitorTpl() string {
	return `{{- if and .Values.metrics.enabled .Values.metrics.serviceMonitor.enabled }}
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: {{ include "` + g.Chart.Name + `.fullname" . }}
  {{- if .Values.metrics.serviceMonitor.namespace }}
  namespace: {{ .Values.metrics.serviceMonitor.namespace }}
  {{- end }}
  labels:
    {{- include "` + g.Chart.Name + `.labels" . | nindent 4 }}
    {{- with .Values.metrics.serviceMonitor.labels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  selector:
    matchLabels:
      {{- include "` + g.Chart.Name + `.selectorLabels" . | nindent 6 }}
  endpoints:
    - port: metrics
      {{- if .Values.metrics.serviceMonitor.interval }}
      interval: {{ .Values.metrics.serviceMonitor.interval }}
      {{- end }}
{{- end }}
`
}

func (g *HelmChartGenerator) generateNotesTpl() string {
	return `1. Get the application URL by running these commands:
{{- if .Values.ingress.enabled }}
{{- range $host := .Values.ingress.hosts }}
  {{- range .paths }}
  http{{ if $.Values.ingress.tls }}s{{ end }}://{{ $host.host }}{{ .path }}
  {{- end }}
{{- end }}
{{- else if contains "NodePort" .Values.service.type }}
  export NODE_PORT=$(kubectl get --namespace {{ .Release.Namespace }} -o jsonpath="{.spec.ports[0].nodePort}" services {{ include "` + g.Chart.Name + `.fullname" . }})
  export NODE_IP=$(kubectl get nodes --namespace {{ .Release.Namespace }} -o jsonpath="{.items[0].status.addresses[0].address}")
  echo http://$NODE_IP:$NODE_PORT
{{- else if contains "LoadBalancer" .Values.service.type }}
     NOTE: It may take a few minutes for the LoadBalancer IP to be available.
           You can watch the status of by running 'kubectl get --namespace {{ .Release.Namespace }} svc -w {{ include "` + g.Chart.Name + `.fullname" . }}'
  export SERVICE_IP=$(kubectl get svc --namespace {{ .Release.Namespace }} {{ include "` + g.Chart.Name + `.fullname" . }} --template "{{"{{ range (index .status.loadBalancer.ingress 0) }}{{.}}{{ end }}"}}")
  echo http://$SERVICE_IP:{{ .Values.service.port }}
{{- else if contains "ClusterIP" .Values.service.type }}
  export POD_NAME=$(kubectl get pods --namespace {{ .Release.Namespace }} -l "app.kubernetes.io/name={{ include "` + g.Chart.Name + `.name" . }},app.kubernetes.io/instance={{ .Release.Name }}" -o jsonpath="{.items[0].metadata.name}")
  export CONTAINER_PORT=$(kubectl get pod --namespace {{ .Release.Namespace }} $POD_NAME -o jsonpath="{.spec.containers[0].ports[0].containerPort}")
  echo "Visit http://127.0.0.1:8080 to use your application"
  kubectl --namespace {{ .Release.Namespace }} port-forward $POD_NAME 8080:$CONTAINER_PORT
{{- end }}

2. ` + g.Chart.Name + ` is now deployed!

   Dashboard: kubectl port-forward svc/{{ include "` + g.Chart.Name + `.fullname" . }} 8080:{{ .Values.service.port }}
   Metrics:   kubectl port-forward svc/{{ include "` + g.Chart.Name + `.fullname" . }} {{ .Values.metrics.port }}:{{ .Values.metrics.port }}
`
}

// WriteToDirectory writes the complete Helm chart to a directory
func (g *HelmChartGenerator) WriteToDirectory(baseDir string) error {
	chartDir := filepath.Join(baseDir, g.Chart.Name)

	// Create directory structure
	dirs := []string{
		chartDir,
		filepath.Join(chartDir, "templates"),
		filepath.Join(chartDir, "templates", "tests"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Write Chart.yaml
	chartYAML, err := g.GenerateChartYAML()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(chartDir, "Chart.yaml"), []byte(chartYAML), 0644); err != nil {
		return fmt.Errorf("failed to write Chart.yaml: %w", err)
	}

	// Write values.yaml
	valuesYAML, err := g.GenerateValuesYAML()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(chartDir, "values.yaml"), []byte(valuesYAML), 0644); err != nil {
		return fmt.Errorf("failed to write values.yaml: %w", err)
	}

	// Write templates
	for _, tpl := range g.GenerateTemplates() {
		path := filepath.Join(chartDir, "templates", tpl.Name)
		if err := os.WriteFile(path, []byte(tpl.Content), 0644); err != nil {
			return fmt.Errorf("failed to write template %s: %w", tpl.Name, err)
		}
	}

	// Write .helmignore
	helmignore := `# Patterns to ignore when building packages.
.DS_Store
.git/
.gitignore
.bzr/
.bzrignore
.hg/
.hgignore
.svn/
*.swp
*.bak
*.tmp
*.orig
*~
.project
.idea/
*.tmproj
.vscode/
`
	if err := os.WriteFile(filepath.Join(chartDir, ".helmignore"), []byte(helmignore), 0644); err != nil {
		return fmt.Errorf("failed to write .helmignore: %w", err)
	}

	return nil
}

// ValidateChart performs basic validation on the chart
func (g *HelmChartGenerator) ValidateChart() []string {
	var errors []string

	if g.Chart.Name == "" {
		errors = append(errors, "chart name is required")
	}
	if g.Chart.Version == "" {
		errors = append(errors, "chart version is required")
	}
	if !isValidSemVer(g.Chart.Version) {
		errors = append(errors, "chart version must be valid semver")
	}
	if g.Values.ReplicaCount < 0 {
		errors = append(errors, "replicaCount must be non-negative")
	}
	if g.Values.Image.Repository == "" {
		errors = append(errors, "image repository is required")
	}

	return errors
}

// isValidSemVer checks if a string is valid semver
func isValidSemVer(v string) bool {
	if v == "" {
		return false
	}

	// Strip any prerelease suffix (everything after first -)
	mainVersion := strings.Split(v, "-")[0]

	parts := strings.Split(mainVersion, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// RenderTemplate renders a Helm template with values (for testing)
func RenderTemplate(templateContent string, values interface{}) (string, error) {
	tmpl, err := template.New("test").Funcs(helmFuncMap()).Parse(templateContent)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, values); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return buf.String(), nil
}

// helmFuncMap returns common Helm template functions
func helmFuncMap() template.FuncMap {
	return template.FuncMap{
		"quote": func(s string) string { return fmt.Sprintf("%q", s) },
		"default": func(d, v interface{}) interface{} {
			if v == nil || v == "" {
				return d
			}
			return v
		},
		"contains": func(substr, s string) bool { return strings.Contains(s, substr) },
		"replace":  strings.ReplaceAll,
		"trunc": func(n int, s string) string {
			if len(s) > n {
				return s[:n]
			}
			return s
		},
		"trimSuffix": func(suffix, s string) string { return strings.TrimSuffix(s, suffix) },
		"nindent": func(n int, s string) string {
			return "\n" + strings.Repeat(" ", n) + strings.ReplaceAll(s, "\n", "\n"+strings.Repeat(" ", n))
		},
		"toYaml":  func(v interface{}) string { b, _ := yaml.Marshal(v); return string(b) },
		"include": func(name string, data interface{}) string { return "" }, // Placeholder
		"printf":  fmt.Sprintf,
	}
}
