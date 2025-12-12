// internal/k8s/observability.go
package k8s

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigMapManifest represents a ConfigMap for dashboards
type ConfigMapManifest struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   ManifestMetadata  `yaml:"metadata"`
	Data       map[string]string `yaml:"data,omitempty"`
}

// ToYAML converts ConfigMap to YAML
func (cm *ConfigMapManifest) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(cm); err != nil {
		return "", fmt.Errorf("failed to encode ConfigMap: %w", err)
	}
	return buf.String(), nil
}

// ObservabilityManager manages observability resources
type ObservabilityManager struct {
	namespace string
	labels    map[string]string
}

// NewObservabilityManager creates a new observability manager
func NewObservabilityManager(namespace string) *ObservabilityManager {
	return &ObservabilityManager{
		namespace: namespace,
		labels: map[string]string{
			"app.kubernetes.io/managed-by": "vaultaire",
		},
	}
}

// ServiceMonitorCfg configures a Prometheus ServiceMonitor (renamed to avoid conflict)
type ServiceMonitorCfg struct {
	Name              string
	Namespace         string
	Labels            map[string]string
	Annotations       map[string]string
	Selector          map[string]string
	NamespaceSelector *NamespaceSelector
	Endpoints         []MonitorEndpoint
	JobLabel          string
	TargetLabels      []string
	PodTargetLabels   []string
	SampleLimit       int
}

// NamespaceSelector defines namespace selection
type NamespaceSelector struct {
	Any        bool
	MatchNames []string
}

// MonitorEndpoint defines a scrape endpoint (renamed to avoid conflict)
type MonitorEndpoint struct {
	Port              string
	Path              string
	Interval          string
	ScrapeTimeout     string
	Scheme            string
	TLSConfig         *MonitorTLSConfig
	BearerTokenFile   string
	BasicAuth         *MonitorBasicAuth
	MetricRelabelings []RelabelConfig
	Relabelings       []RelabelConfig
	HonorLabels       bool
	HonorTimestamps   *bool
}

// MonitorTLSConfig defines TLS settings
type MonitorTLSConfig struct {
	CAFile             string
	CertFile           string
	KeyFile            string
	ServerName         string
	InsecureSkipVerify bool
}

// MonitorBasicAuth defines basic auth settings
type MonitorBasicAuth struct {
	Username SecretKeySelector
	Password SecretKeySelector
}

// SecretKeySelector references a secret key
type SecretKeySelector struct {
	Name string
	Key  string
}

// RelabelConfig defines metric relabeling
type RelabelConfig struct {
	SourceLabels []string
	Separator    string
	TargetLabel  string
	Regex        string
	Modulus      int
	Replacement  string
	Action       string // replace, keep, drop, hashmod, labelmap, labeldrop, labelkeep
}

// ServiceMonitorResource represents a ServiceMonitor
type ServiceMonitorResource struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   ManifestMetadata   `yaml:"metadata"`
	Spec       ServiceMonitorSpec `yaml:"spec"`
}

// ServiceMonitorSpec defines ServiceMonitor specification
type ServiceMonitorSpec struct {
	Selector          LabelSelectorSpec      `yaml:"selector"`
	NamespaceSelector *NamespaceSelectorSpec `yaml:"namespaceSelector,omitempty"`
	Endpoints         []EndpointSpec         `yaml:"endpoints"`
	JobLabel          string                 `yaml:"jobLabel,omitempty"`
	TargetLabels      []string               `yaml:"targetLabels,omitempty"`
	PodTargetLabels   []string               `yaml:"podTargetLabels,omitempty"`
	SampleLimit       int                    `yaml:"sampleLimit,omitempty"`
}

// LabelSelectorSpec for ServiceMonitor
type LabelSelectorSpec struct {
	MatchLabels map[string]string `yaml:"matchLabels,omitempty"`
}

// NamespaceSelectorSpec for ServiceMonitor
type NamespaceSelectorSpec struct {
	Any        bool     `yaml:"any,omitempty"`
	MatchNames []string `yaml:"matchNames,omitempty"`
}

// EndpointSpec for ServiceMonitor
type EndpointSpec struct {
	Port              string              `yaml:"port,omitempty"`
	Path              string              `yaml:"path,omitempty"`
	Interval          string              `yaml:"interval,omitempty"`
	ScrapeTimeout     string              `yaml:"scrapeTimeout,omitempty"`
	Scheme            string              `yaml:"scheme,omitempty"`
	TLSConfig         *TLSConfigSpec      `yaml:"tlsConfig,omitempty"`
	BearerTokenFile   string              `yaml:"bearerTokenFile,omitempty"`
	BasicAuth         *BasicAuthSpec      `yaml:"basicAuth,omitempty"`
	MetricRelabelings []RelabelConfigSpec `yaml:"metricRelabelings,omitempty"`
	Relabelings       []RelabelConfigSpec `yaml:"relabelings,omitempty"`
	HonorLabels       bool                `yaml:"honorLabels,omitempty"`
	HonorTimestamps   *bool               `yaml:"honorTimestamps,omitempty"`
}

// TLSConfigSpec for ServiceMonitor
type TLSConfigSpec struct {
	CAFile             string `yaml:"caFile,omitempty"`
	CertFile           string `yaml:"certFile,omitempty"`
	KeyFile            string `yaml:"keyFile,omitempty"`
	ServerName         string `yaml:"serverName,omitempty"`
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify,omitempty"`
}

// BasicAuthSpec for ServiceMonitor
type BasicAuthSpec struct {
	Username SecretKeySelectorSpec `yaml:"username"`
	Password SecretKeySelectorSpec `yaml:"password"`
}

// SecretKeySelectorSpec for ServiceMonitor
type SecretKeySelectorSpec struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

// RelabelConfigSpec for ServiceMonitor
type RelabelConfigSpec struct {
	SourceLabels []string `yaml:"sourceLabels,omitempty"`
	Separator    string   `yaml:"separator,omitempty"`
	TargetLabel  string   `yaml:"targetLabel,omitempty"`
	Regex        string   `yaml:"regex,omitempty"`
	Modulus      int      `yaml:"modulus,omitempty"`
	Replacement  string   `yaml:"replacement,omitempty"`
	Action       string   `yaml:"action,omitempty"`
}

// GenerateServiceMonitor creates a ServiceMonitor resource
func (om *ObservabilityManager) GenerateServiceMonitor(config ServiceMonitorCfg) *ServiceMonitorResource {
	if config.Namespace == "" {
		config.Namespace = om.namespace
	}

	labels := copyStringMap(om.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	sm := &ServiceMonitorResource{
		APIVersion: "monitoring.coreos.com/v1",
		Kind:       "ServiceMonitor",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Namespace:   config.Namespace,
			Labels:      labels,
			Annotations: config.Annotations,
		},
		Spec: ServiceMonitorSpec{
			Selector: LabelSelectorSpec{
				MatchLabels: config.Selector,
			},
			JobLabel:        config.JobLabel,
			TargetLabels:    config.TargetLabels,
			PodTargetLabels: config.PodTargetLabels,
			SampleLimit:     config.SampleLimit,
		},
	}

	if config.NamespaceSelector != nil {
		sm.Spec.NamespaceSelector = &NamespaceSelectorSpec{
			Any:        config.NamespaceSelector.Any,
			MatchNames: config.NamespaceSelector.MatchNames,
		}
	}

	for _, ep := range config.Endpoints {
		epSpec := EndpointSpec{
			Port:            ep.Port,
			Path:            ep.Path,
			Interval:        ep.Interval,
			ScrapeTimeout:   ep.ScrapeTimeout,
			Scheme:          ep.Scheme,
			BearerTokenFile: ep.BearerTokenFile,
			HonorLabels:     ep.HonorLabels,
			HonorTimestamps: ep.HonorTimestamps,
		}

		if ep.TLSConfig != nil {
			epSpec.TLSConfig = &TLSConfigSpec{
				CAFile:             ep.TLSConfig.CAFile,
				CertFile:           ep.TLSConfig.CertFile,
				KeyFile:            ep.TLSConfig.KeyFile,
				ServerName:         ep.TLSConfig.ServerName,
				InsecureSkipVerify: ep.TLSConfig.InsecureSkipVerify,
			}
		}

		if ep.BasicAuth != nil {
			epSpec.BasicAuth = &BasicAuthSpec{
				Username: SecretKeySelectorSpec{Name: ep.BasicAuth.Username.Name, Key: ep.BasicAuth.Username.Key},
				Password: SecretKeySelectorSpec{Name: ep.BasicAuth.Password.Name, Key: ep.BasicAuth.Password.Key},
			}
		}

		for _, r := range ep.MetricRelabelings {
			epSpec.MetricRelabelings = append(epSpec.MetricRelabelings, RelabelConfigSpec(r))
		}

		for _, r := range ep.Relabelings {
			epSpec.Relabelings = append(epSpec.Relabelings, RelabelConfigSpec(r))
		}

		sm.Spec.Endpoints = append(sm.Spec.Endpoints, epSpec)
	}

	return sm
}

// ToYAML converts ServiceMonitor to YAML
func (sm *ServiceMonitorResource) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(sm); err != nil {
		return "", fmt.Errorf("failed to encode ServiceMonitor: %w", err)
	}
	return buf.String(), nil
}

// PodMonitorCfg configures a Prometheus PodMonitor
type PodMonitorCfg struct {
	Name                string
	Namespace           string
	Labels              map[string]string
	Annotations         map[string]string
	Selector            map[string]string
	NamespaceSelector   *NamespaceSelector
	PodMetricsEndpoints []PodMetricsEndpoint
	JobLabel            string
	PodTargetLabels     []string
	SampleLimit         int
}

// PodMetricsEndpoint defines a pod metrics endpoint
type PodMetricsEndpoint struct {
	Port            string
	Path            string
	Interval        string
	ScrapeTimeout   string
	Scheme          string
	HonorLabels     bool
	HonorTimestamps *bool
}

// PodMonitorResource represents a PodMonitor
type PodMonitorResource struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   ManifestMetadata `yaml:"metadata"`
	Spec       PodMonitorSpec   `yaml:"spec"`
}

// PodMonitorSpec defines PodMonitor specification
type PodMonitorSpec struct {
	Selector            LabelSelectorSpec        `yaml:"selector"`
	NamespaceSelector   *NamespaceSelectorSpec   `yaml:"namespaceSelector,omitempty"`
	PodMetricsEndpoints []PodMetricsEndpointSpec `yaml:"podMetricsEndpoints"`
	JobLabel            string                   `yaml:"jobLabel,omitempty"`
	PodTargetLabels     []string                 `yaml:"podTargetLabels,omitempty"`
	SampleLimit         int                      `yaml:"sampleLimit,omitempty"`
}

// PodMetricsEndpointSpec for PodMonitor
type PodMetricsEndpointSpec struct {
	Port            string `yaml:"port,omitempty"`
	Path            string `yaml:"path,omitempty"`
	Interval        string `yaml:"interval,omitempty"`
	ScrapeTimeout   string `yaml:"scrapeTimeout,omitempty"`
	Scheme          string `yaml:"scheme,omitempty"`
	HonorLabels     bool   `yaml:"honorLabels,omitempty"`
	HonorTimestamps *bool  `yaml:"honorTimestamps,omitempty"`
}

// GeneratePodMonitor creates a PodMonitor resource
func (om *ObservabilityManager) GeneratePodMonitor(config PodMonitorCfg) *PodMonitorResource {
	if config.Namespace == "" {
		config.Namespace = om.namespace
	}

	labels := copyStringMap(om.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	pm := &PodMonitorResource{
		APIVersion: "monitoring.coreos.com/v1",
		Kind:       "PodMonitor",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Namespace:   config.Namespace,
			Labels:      labels,
			Annotations: config.Annotations,
		},
		Spec: PodMonitorSpec{
			Selector: LabelSelectorSpec{
				MatchLabels: config.Selector,
			},
			JobLabel:        config.JobLabel,
			PodTargetLabels: config.PodTargetLabels,
			SampleLimit:     config.SampleLimit,
		},
	}

	if config.NamespaceSelector != nil {
		pm.Spec.NamespaceSelector = &NamespaceSelectorSpec{
			Any:        config.NamespaceSelector.Any,
			MatchNames: config.NamespaceSelector.MatchNames,
		}
	}

	for _, ep := range config.PodMetricsEndpoints {
		pm.Spec.PodMetricsEndpoints = append(pm.Spec.PodMetricsEndpoints, PodMetricsEndpointSpec(ep))
	}

	return pm
}

// ToYAML converts PodMonitor to YAML
func (pm *PodMonitorResource) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(pm); err != nil {
		return "", fmt.Errorf("failed to encode PodMonitor: %w", err)
	}
	return buf.String(), nil
}

// PrometheusRuleCfg configures a PrometheusRule
type PrometheusRuleCfg struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
	Groups      []RuleGroup
}

// RuleGroup defines a group of rules
type RuleGroup struct {
	Name     string
	Interval string
	Rules    []Rule
}

// Rule defines an alerting or recording rule
type Rule struct {
	Record      string // Recording rule name
	Alert       string // Alert name
	Expr        string // PromQL expression
	For         string // Alert duration
	Labels      map[string]string
	Annotations map[string]string
}

// PrometheusRuleResource represents a PrometheusRule
type PrometheusRuleResource struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   ManifestMetadata   `yaml:"metadata"`
	Spec       PrometheusRuleSpec `yaml:"spec"`
}

// PrometheusRuleSpec defines PrometheusRule specification
type PrometheusRuleSpec struct {
	Groups []RuleGroupSpec `yaml:"groups"`
}

// RuleGroupSpec for PrometheusRule
type RuleGroupSpec struct {
	Name     string     `yaml:"name"`
	Interval string     `yaml:"interval,omitempty"`
	Rules    []RuleSpec `yaml:"rules"`
}

// RuleSpec for PrometheusRule
type RuleSpec struct {
	Record      string            `yaml:"record,omitempty"`
	Alert       string            `yaml:"alert,omitempty"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// GeneratePrometheusRule creates a PrometheusRule resource
func (om *ObservabilityManager) GeneratePrometheusRule(config PrometheusRuleCfg) *PrometheusRuleResource {
	if config.Namespace == "" {
		config.Namespace = om.namespace
	}

	labels := copyStringMap(om.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	pr := &PrometheusRuleResource{
		APIVersion: "monitoring.coreos.com/v1",
		Kind:       "PrometheusRule",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Namespace:   config.Namespace,
			Labels:      labels,
			Annotations: config.Annotations,
		},
		Spec: PrometheusRuleSpec{},
	}

	for _, g := range config.Groups {
		groupSpec := RuleGroupSpec{
			Name:     g.Name,
			Interval: g.Interval,
		}
		for _, r := range g.Rules {
			groupSpec.Rules = append(groupSpec.Rules, RuleSpec(r))
		}
		pr.Spec.Groups = append(pr.Spec.Groups, groupSpec)
	}

	return pr
}

// ToYAML converts PrometheusRule to YAML
func (pr *PrometheusRuleResource) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(pr); err != nil {
		return "", fmt.Errorf("failed to encode PrometheusRule: %w", err)
	}
	return buf.String(), nil
}

// GrafanaDashboardCfg configures a Grafana Dashboard ConfigMap
type GrafanaDashboardCfg struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
	Folder      string
	JSON        string
}

// GenerateGrafanaDashboard creates a ConfigMap for Grafana dashboard
func (om *ObservabilityManager) GenerateGrafanaDashboard(config GrafanaDashboardCfg) *ConfigMapManifest {
	if config.Namespace == "" {
		config.Namespace = om.namespace
	}

	labels := copyStringMap(om.labels)
	labels["grafana_dashboard"] = "1"
	for k, v := range config.Labels {
		labels[k] = v
	}

	annotations := make(map[string]string)
	for k, v := range config.Annotations {
		annotations[k] = v
	}
	if config.Folder != "" {
		annotations["grafana_folder"] = config.Folder
	}

	return &ConfigMapManifest{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Namespace:   config.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Data: map[string]string{
			config.Name + ".json": config.JSON,
		},
	}
}

// GenerateVaultaireObservability creates observability resources for Vaultaire
func GenerateVaultaireObservability(namespace string) (*ServiceMonitorResource, *PrometheusRuleResource) {
	om := NewObservabilityManager(namespace)

	// ServiceMonitor for API server
	sm := om.GenerateServiceMonitor(ServiceMonitorCfg{
		Name:     "vaultaire-api",
		Selector: map[string]string{"app": "vaultaire-api"},
		Endpoints: []MonitorEndpoint{
			{
				Port:     "metrics",
				Path:     "/metrics",
				Interval: "30s",
			},
		},
		TargetLabels: []string{"app", "version"},
	})

	// Alerting rules
	pr := om.GeneratePrometheusRule(PrometheusRuleCfg{
		Name: "vaultaire-alerts",
		Labels: map[string]string{
			"prometheus": "vaultaire",
			"role":       "alert-rules",
		},
		Groups: []RuleGroup{
			{
				Name: "vaultaire.rules",
				Rules: []Rule{
					{
						Alert: "VaultaireAPIHighErrorRate",
						Expr:  `sum(rate(http_requests_total{job="vaultaire-api",status=~"5.."}[5m])) / sum(rate(http_requests_total{job="vaultaire-api"}[5m])) > 0.05`,
						For:   "5m",
						Labels: map[string]string{
							"severity": "warning",
						},
						Annotations: map[string]string{
							"summary":     "High error rate on Vaultaire API",
							"description": "Error rate is {{ $value | humanizePercentage }} over the last 5 minutes",
						},
					},
					{
						Alert: "VaultaireAPIPodDown",
						Expr:  `up{job="vaultaire-api"} == 0`,
						For:   "1m",
						Labels: map[string]string{
							"severity": "critical",
						},
						Annotations: map[string]string{
							"summary":     "Vaultaire API pod is down",
							"description": "Pod {{ $labels.pod }} has been down for more than 1 minute",
						},
					},
					{
						Alert: "VaultaireHighMemoryUsage",
						Expr:  `container_memory_usage_bytes{container="vaultaire-api"} / container_spec_memory_limit_bytes{container="vaultaire-api"} > 0.9`,
						For:   "5m",
						Labels: map[string]string{
							"severity": "warning",
						},
						Annotations: map[string]string{
							"summary":     "High memory usage on Vaultaire API",
							"description": "Memory usage is above 90% for pod {{ $labels.pod }}",
						},
					},
					{
						Alert: "VaultaireHighCPUUsage",
						Expr:  `rate(container_cpu_usage_seconds_total{container="vaultaire-api"}[5m]) > 0.9`,
						For:   "5m",
						Labels: map[string]string{
							"severity": "warning",
						},
						Annotations: map[string]string{
							"summary":     "High CPU usage on Vaultaire API",
							"description": "CPU usage is above 90% for pod {{ $labels.pod }}",
						},
					},
					{
						Alert: "VaultaireStorageQuotaExceeded",
						Expr:  `vaultaire_storage_used_bytes / vaultaire_storage_quota_bytes > 0.95`,
						For:   "10m",
						Labels: map[string]string{
							"severity": "critical",
						},
						Annotations: map[string]string{
							"summary":     "Storage quota nearly exceeded",
							"description": "Storage usage is {{ $value | humanizePercentage }} of quota",
						},
					},
				},
			},
			{
				Name: "vaultaire.slo",
				Rules: []Rule{
					{
						Record: "vaultaire:http_request_duration_seconds:p99",
						Expr:   `histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket{job="vaultaire-api"}[5m])) by (le))`,
					},
					{
						Record: "vaultaire:http_request_duration_seconds:p95",
						Expr:   `histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket{job="vaultaire-api"}[5m])) by (le))`,
					},
					{
						Record: "vaultaire:http_request_duration_seconds:p50",
						Expr:   `histogram_quantile(0.50, sum(rate(http_request_duration_seconds_bucket{job="vaultaire-api"}[5m])) by (le))`,
					},
					{
						Record: "vaultaire:availability:ratio_rate5m",
						Expr:   `sum(rate(http_requests_total{job="vaultaire-api",status!~"5.."}[5m])) / sum(rate(http_requests_total{job="vaultaire-api"}[5m]))`,
					},
				},
			},
		},
	})

	return sm, pr
}

// ObservabilitySet represents a collection of observability resources
type ObservabilitySet struct {
	ServiceMonitors []*ServiceMonitorResource
	PodMonitors     []*PodMonitorResource
	PrometheusRules []*PrometheusRuleResource
	Dashboards      []*ConfigMapManifest
}

// ToYAML converts all resources to multi-document YAML
func (s *ObservabilitySet) ToYAML() (string, error) {
	var parts []string

	for _, sm := range s.ServiceMonitors {
		yaml, err := sm.ToYAML()
		if err != nil {
			return "", err
		}
		parts = append(parts, yaml)
	}

	for _, pm := range s.PodMonitors {
		yaml, err := pm.ToYAML()
		if err != nil {
			return "", err
		}
		parts = append(parts, yaml)
	}

	for _, pr := range s.PrometheusRules {
		yaml, err := pr.ToYAML()
		if err != nil {
			return "", err
		}
		parts = append(parts, yaml)
	}

	for _, d := range s.Dashboards {
		yaml, err := d.ToYAML()
		if err != nil {
			return "", err
		}
		parts = append(parts, yaml)
	}

	return strings.Join(parts, "---\n"), nil
}
