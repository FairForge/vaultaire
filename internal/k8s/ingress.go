// internal/k8s/ingress.go
package k8s

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// IngressManager manages Kubernetes Ingress resources
type IngressManager struct {
	namespace string
	labels    map[string]string
}

// NewIngressManager creates a new Ingress manager
func NewIngressManager(namespace string) *IngressManager {
	return &IngressManager{
		namespace: namespace,
		labels: map[string]string{
			"app.kubernetes.io/managed-by": "vaultaire",
		},
	}
}

// K8sIngressConfig configures an Ingress resource
type K8sIngressConfig struct {
	Name             string
	Namespace        string
	Labels           map[string]string
	Annotations      map[string]string
	IngressClassName string
	TLS              []K8sIngressTLS
	Rules            []IngressRule
	DefaultBackend   *IngressBackend
}

// K8sIngressTLS configures TLS for an Ingress
type K8sIngressTLS struct {
	Hosts      []string `yaml:"hosts,omitempty"`
	SecretName string   `yaml:"secretName,omitempty"`
}

// IngressRule defines an Ingress rule
type IngressRule struct {
	Host string
	HTTP *HTTPIngressRuleValue
}

// HTTPIngressRuleValue defines HTTP rules
type HTTPIngressRuleValue struct {
	Paths []HTTPIngressPath `yaml:"paths"`
}

// HTTPIngressPath defines a path-based routing rule
type HTTPIngressPath struct {
	Path     string          `yaml:"path"`
	PathType string          `yaml:"pathType"` // Exact, Prefix, ImplementationSpecific
	Backend  *IngressBackend `yaml:"backend"`
}

// IngressBackend defines the backend service
type IngressBackend struct {
	Service  *IngressServiceBackend  `yaml:"service,omitempty"`
	Resource *IngressResourceBackend `yaml:"resource,omitempty"`
}

// IngressServiceBackend references a Service
type IngressServiceBackend struct {
	Name string            `yaml:"name"`
	Port K8sIngressSvcPort `yaml:"port"`
}

// K8sIngressSvcPort identifies a service port
type K8sIngressSvcPort struct {
	Name   string `yaml:"name,omitempty"`
	Number int    `yaml:"number,omitempty"`
}

// IngressResourceBackend references a resource
type IngressResourceBackend struct {
	APIGroup string `yaml:"apiGroup"`
	Kind     string `yaml:"kind"`
	Name     string `yaml:"name"`
}

// IngressResource represents a Kubernetes Ingress
type IngressResource struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   ManifestMetadata `yaml:"metadata"`
	Spec       IngressSpec      `yaml:"spec"`
}

// IngressSpec defines the Ingress specification
type IngressSpec struct {
	IngressClassName string          `yaml:"ingressClassName,omitempty"`
	TLS              []K8sIngressTLS `yaml:"tls,omitempty"`
	Rules            []IngressRule   `yaml:"rules,omitempty"` // Changed from IngressRuleSpec
	DefaultBackend   *IngressBackend `yaml:"defaultBackend,omitempty"`
}

// IngressRuleSpec defines a rule in the spec
type IngressRuleSpec struct {
	Host string                `yaml:"host,omitempty"`
	HTTP *HTTPIngressRuleValue `yaml:"http,omitempty"`
}

// GenerateIngress creates an Ingress resource
func (im *IngressManager) GenerateIngress(config K8sIngressConfig) *IngressResource {
	if config.Namespace == "" {
		config.Namespace = im.namespace
	}

	labels := copyStringMap(im.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	annotations := copyStringMap(config.Annotations)

	return &IngressResource{
		APIVersion: "networking.k8s.io/v1",
		Kind:       "Ingress",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Namespace:   config.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: IngressSpec{
			IngressClassName: config.IngressClassName,
			TLS:              config.TLS,
			Rules:            config.Rules, // Direct assignment now works
			DefaultBackend:   config.DefaultBackend,
		},
	}
}

// ToYAML converts the Ingress to YAML
func (i *IngressResource) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(i); err != nil {
		return "", fmt.Errorf("failed to encode ingress: %w", err)
	}
	return buf.String(), nil
}

// NginxIngressConfig provides nginx-specific configuration
type NginxIngressConfig struct {
	// Basic settings
	ProxyBodySize       string
	ProxyConnectTimeout int
	ProxySendTimeout    int
	ProxyReadTimeout    int

	// SSL settings
	SSLRedirect      bool
	ForceSSLRedirect bool
	SSLCiphers       string
	SSLProtocols     string

	// Backend settings
	BackendProtocol string // HTTP, HTTPS, GRPC, GRPCS
	UpstreamHashBy  string

	// Rate limiting
	RateLimitConnections int
	RateLimitRPS         int
	RateLimitRPM         int

	// CORS
	CORSEnabled          bool
	CORSAllowOrigin      string
	CORSAllowMethods     string
	CORSAllowHeaders     string
	CORSExposeHeaders    string
	CORSAllowCredentials bool
	CORSMaxAge           int

	// Authentication
	AuthType   string // basic, digest, external
	AuthSecret string
	AuthRealm  string
	AuthURL    string

	// Rewrites
	RewriteTarget string
	AppRoot       string

	// Whitelist/Blacklist
	WhitelistSourceRange string
	DenylistSourceRange  string

	// Custom snippets
	ServerSnippet        string
	ConfigurationSnippet string
	StreamSnippet        string

	// Canary settings
	CanaryEnabled     bool
	CanaryWeight      int
	CanaryHeader      string
	CanaryHeaderValue string
	CanaryCookie      string

	// Misc
	ClientMaxBodySize    string
	ProxyBuffering       string
	ProxyBufferSize      string
	ProxyMaxTempFileSize string
	UseRegex             bool
	EnableModsecurity    bool
	ModsecuritySnippet   string
}

// ToAnnotations converts nginx config to annotations
func (n *NginxIngressConfig) ToAnnotations() map[string]string {
	annotations := make(map[string]string)

	// Basic settings
	if n.ProxyBodySize != "" {
		annotations["nginx.ingress.kubernetes.io/proxy-body-size"] = n.ProxyBodySize
	}
	if n.ProxyConnectTimeout > 0 {
		annotations["nginx.ingress.kubernetes.io/proxy-connect-timeout"] = fmt.Sprintf("%d", n.ProxyConnectTimeout)
	}
	if n.ProxySendTimeout > 0 {
		annotations["nginx.ingress.kubernetes.io/proxy-send-timeout"] = fmt.Sprintf("%d", n.ProxySendTimeout)
	}
	if n.ProxyReadTimeout > 0 {
		annotations["nginx.ingress.kubernetes.io/proxy-read-timeout"] = fmt.Sprintf("%d", n.ProxyReadTimeout)
	}

	// SSL settings
	if n.SSLRedirect {
		annotations["nginx.ingress.kubernetes.io/ssl-redirect"] = "true"
	}
	if n.ForceSSLRedirect {
		annotations["nginx.ingress.kubernetes.io/force-ssl-redirect"] = "true"
	}
	if n.SSLCiphers != "" {
		annotations["nginx.ingress.kubernetes.io/ssl-ciphers"] = n.SSLCiphers
	}
	if n.SSLProtocols != "" {
		annotations["nginx.ingress.kubernetes.io/ssl-protocols"] = n.SSLProtocols
	}

	// Backend settings
	if n.BackendProtocol != "" {
		annotations["nginx.ingress.kubernetes.io/backend-protocol"] = n.BackendProtocol
	}
	if n.UpstreamHashBy != "" {
		annotations["nginx.ingress.kubernetes.io/upstream-hash-by"] = n.UpstreamHashBy
	}

	// Rate limiting
	if n.RateLimitConnections > 0 {
		annotations["nginx.ingress.kubernetes.io/limit-connections"] = fmt.Sprintf("%d", n.RateLimitConnections)
	}
	if n.RateLimitRPS > 0 {
		annotations["nginx.ingress.kubernetes.io/limit-rps"] = fmt.Sprintf("%d", n.RateLimitRPS)
	}
	if n.RateLimitRPM > 0 {
		annotations["nginx.ingress.kubernetes.io/limit-rpm"] = fmt.Sprintf("%d", n.RateLimitRPM)
	}

	// CORS
	if n.CORSEnabled {
		annotations["nginx.ingress.kubernetes.io/enable-cors"] = "true"
		if n.CORSAllowOrigin != "" {
			annotations["nginx.ingress.kubernetes.io/cors-allow-origin"] = n.CORSAllowOrigin
		}
		if n.CORSAllowMethods != "" {
			annotations["nginx.ingress.kubernetes.io/cors-allow-methods"] = n.CORSAllowMethods
		}
		if n.CORSAllowHeaders != "" {
			annotations["nginx.ingress.kubernetes.io/cors-allow-headers"] = n.CORSAllowHeaders
		}
		if n.CORSExposeHeaders != "" {
			annotations["nginx.ingress.kubernetes.io/cors-expose-headers"] = n.CORSExposeHeaders
		}
		if n.CORSAllowCredentials {
			annotations["nginx.ingress.kubernetes.io/cors-allow-credentials"] = "true"
		}
		if n.CORSMaxAge > 0 {
			annotations["nginx.ingress.kubernetes.io/cors-max-age"] = fmt.Sprintf("%d", n.CORSMaxAge)
		}
	}

	// Authentication
	if n.AuthType != "" {
		annotations["nginx.ingress.kubernetes.io/auth-type"] = n.AuthType
	}
	if n.AuthSecret != "" {
		annotations["nginx.ingress.kubernetes.io/auth-secret"] = n.AuthSecret
	}
	if n.AuthRealm != "" {
		annotations["nginx.ingress.kubernetes.io/auth-realm"] = n.AuthRealm
	}
	if n.AuthURL != "" {
		annotations["nginx.ingress.kubernetes.io/auth-url"] = n.AuthURL
	}

	// Rewrites
	if n.RewriteTarget != "" {
		annotations["nginx.ingress.kubernetes.io/rewrite-target"] = n.RewriteTarget
	}
	if n.AppRoot != "" {
		annotations["nginx.ingress.kubernetes.io/app-root"] = n.AppRoot
	}

	// Whitelist/Blacklist
	if n.WhitelistSourceRange != "" {
		annotations["nginx.ingress.kubernetes.io/whitelist-source-range"] = n.WhitelistSourceRange
	}
	if n.DenylistSourceRange != "" {
		annotations["nginx.ingress.kubernetes.io/denylist-source-range"] = n.DenylistSourceRange
	}

	// Custom snippets
	if n.ServerSnippet != "" {
		annotations["nginx.ingress.kubernetes.io/server-snippet"] = n.ServerSnippet
	}
	if n.ConfigurationSnippet != "" {
		annotations["nginx.ingress.kubernetes.io/configuration-snippet"] = n.ConfigurationSnippet
	}
	if n.StreamSnippet != "" {
		annotations["nginx.ingress.kubernetes.io/stream-snippet"] = n.StreamSnippet
	}

	// Canary settings
	if n.CanaryEnabled {
		annotations["nginx.ingress.kubernetes.io/canary"] = "true"
		if n.CanaryWeight > 0 {
			annotations["nginx.ingress.kubernetes.io/canary-weight"] = fmt.Sprintf("%d", n.CanaryWeight)
		}
		if n.CanaryHeader != "" {
			annotations["nginx.ingress.kubernetes.io/canary-by-header"] = n.CanaryHeader
			if n.CanaryHeaderValue != "" {
				annotations["nginx.ingress.kubernetes.io/canary-by-header-value"] = n.CanaryHeaderValue
			}
		}
		if n.CanaryCookie != "" {
			annotations["nginx.ingress.kubernetes.io/canary-by-cookie"] = n.CanaryCookie
		}
	}

	// Misc
	if n.ClientMaxBodySize != "" {
		annotations["nginx.ingress.kubernetes.io/client-max-body-size"] = n.ClientMaxBodySize
	}
	if n.ProxyBuffering != "" {
		annotations["nginx.ingress.kubernetes.io/proxy-buffering"] = n.ProxyBuffering
	}
	if n.ProxyBufferSize != "" {
		annotations["nginx.ingress.kubernetes.io/proxy-buffer-size"] = n.ProxyBufferSize
	}
	if n.ProxyMaxTempFileSize != "" {
		annotations["nginx.ingress.kubernetes.io/proxy-max-temp-file-size"] = n.ProxyMaxTempFileSize
	}
	if n.UseRegex {
		annotations["nginx.ingress.kubernetes.io/use-regex"] = "true"
	}
	if n.EnableModsecurity {
		annotations["nginx.ingress.kubernetes.io/enable-modsecurity"] = "true"
		if n.ModsecuritySnippet != "" {
			annotations["nginx.ingress.kubernetes.io/modsecurity-snippet"] = n.ModsecuritySnippet
		}
	}

	return annotations
}

// TraefikIngressConfig provides Traefik-specific configuration
type TraefikIngressConfig struct {
	EntryPoints      []string
	Priority         int
	TLSOptions       string
	TLSCertResolver  string
	Middlewares      []string
	Sticky           bool
	StickyCookieName string
	PassHostHeader   bool
	ServersTransport string
}

// ToAnnotations converts Traefik config to annotations
func (t *TraefikIngressConfig) ToAnnotations() map[string]string {
	annotations := make(map[string]string)

	if len(t.EntryPoints) > 0 {
		annotations["traefik.ingress.kubernetes.io/router.entrypoints"] = strings.Join(t.EntryPoints, ",")
	}
	if t.Priority > 0 {
		annotations["traefik.ingress.kubernetes.io/router.priority"] = fmt.Sprintf("%d", t.Priority)
	}
	if t.TLSOptions != "" {
		annotations["traefik.ingress.kubernetes.io/router.tls.options"] = t.TLSOptions
	}
	if t.TLSCertResolver != "" {
		annotations["traefik.ingress.kubernetes.io/router.tls.certresolver"] = t.TLSCertResolver
	}
	if len(t.Middlewares) > 0 {
		annotations["traefik.ingress.kubernetes.io/router.middlewares"] = strings.Join(t.Middlewares, ",")
	}
	if t.Sticky {
		annotations["traefik.ingress.kubernetes.io/service.sticky.cookie"] = "true"
		if t.StickyCookieName != "" {
			annotations["traefik.ingress.kubernetes.io/service.sticky.cookie.name"] = t.StickyCookieName
		}
	}
	if !t.PassHostHeader {
		annotations["traefik.ingress.kubernetes.io/service.passhostheader"] = "false"
	}
	if t.ServersTransport != "" {
		annotations["traefik.ingress.kubernetes.io/service.serverstransport"] = t.ServersTransport
	}

	return annotations
}

// AWSALBIngressConfig provides AWS ALB-specific configuration
type AWSALBIngressConfig struct {
	Scheme                  string
	IPAddressType           string
	LoadBalancerName        string
	SecurityGroups          []string
	Subnets                 []string
	Tags                    map[string]string
	ListenPorts             string
	CertificateARN          string
	SSLPolicy               string
	SSLRedirect             string
	TargetType              string
	BackendProtocol         string
	BackendProtocolVersion  string
	HealthCheckPath         string
	HealthCheckPort         string
	HealthCheckInterval     int
	HealthCheckTimeout      int
	HealthyThreshold        int
	UnhealthyThreshold      int
	SuccessCodes            string
	WAFv2ACLArn             string
	CognitoUserPoolARN      string
	CognitoUserPoolClientID string
	CognitoUserPoolDomain   string
	AuthOnUnauthenticated   string
	Actions                 string
}

// ToAnnotations converts AWS ALB config to annotations
func (a *AWSALBIngressConfig) ToAnnotations() map[string]string {
	annotations := map[string]string{
		"kubernetes.io/ingress.class": "alb",
	}

	if a.Scheme != "" {
		annotations["alb.ingress.kubernetes.io/scheme"] = a.Scheme
	}
	if a.IPAddressType != "" {
		annotations["alb.ingress.kubernetes.io/ip-address-type"] = a.IPAddressType
	}
	if a.LoadBalancerName != "" {
		annotations["alb.ingress.kubernetes.io/load-balancer-name"] = a.LoadBalancerName
	}
	if len(a.SecurityGroups) > 0 {
		annotations["alb.ingress.kubernetes.io/security-groups"] = strings.Join(a.SecurityGroups, ",")
	}
	if len(a.Subnets) > 0 {
		annotations["alb.ingress.kubernetes.io/subnets"] = strings.Join(a.Subnets, ",")
	}
	if len(a.Tags) > 0 {
		var tags []string
		for k, v := range a.Tags {
			tags = append(tags, fmt.Sprintf("%s=%s", k, v))
		}
		annotations["alb.ingress.kubernetes.io/tags"] = strings.Join(tags, ",")
	}
	if a.ListenPorts != "" {
		annotations["alb.ingress.kubernetes.io/listen-ports"] = a.ListenPorts
	}
	if a.CertificateARN != "" {
		annotations["alb.ingress.kubernetes.io/certificate-arn"] = a.CertificateARN
	}
	if a.SSLPolicy != "" {
		annotations["alb.ingress.kubernetes.io/ssl-policy"] = a.SSLPolicy
	}
	if a.SSLRedirect != "" {
		annotations["alb.ingress.kubernetes.io/ssl-redirect"] = a.SSLRedirect
	}
	if a.TargetType != "" {
		annotations["alb.ingress.kubernetes.io/target-type"] = a.TargetType
	}
	if a.BackendProtocol != "" {
		annotations["alb.ingress.kubernetes.io/backend-protocol"] = a.BackendProtocol
	}
	if a.BackendProtocolVersion != "" {
		annotations["alb.ingress.kubernetes.io/backend-protocol-version"] = a.BackendProtocolVersion
	}
	if a.HealthCheckPath != "" {
		annotations["alb.ingress.kubernetes.io/healthcheck-path"] = a.HealthCheckPath
	}
	if a.HealthCheckPort != "" {
		annotations["alb.ingress.kubernetes.io/healthcheck-port"] = a.HealthCheckPort
	}
	if a.HealthCheckInterval > 0 {
		annotations["alb.ingress.kubernetes.io/healthcheck-interval-seconds"] = fmt.Sprintf("%d", a.HealthCheckInterval)
	}
	if a.HealthCheckTimeout > 0 {
		annotations["alb.ingress.kubernetes.io/healthcheck-timeout-seconds"] = fmt.Sprintf("%d", a.HealthCheckTimeout)
	}
	if a.HealthyThreshold > 0 {
		annotations["alb.ingress.kubernetes.io/healthy-threshold-count"] = fmt.Sprintf("%d", a.HealthyThreshold)
	}
	if a.UnhealthyThreshold > 0 {
		annotations["alb.ingress.kubernetes.io/unhealthy-threshold-count"] = fmt.Sprintf("%d", a.UnhealthyThreshold)
	}
	if a.SuccessCodes != "" {
		annotations["alb.ingress.kubernetes.io/success-codes"] = a.SuccessCodes
	}
	if a.WAFv2ACLArn != "" {
		annotations["alb.ingress.kubernetes.io/wafv2-acl-arn"] = a.WAFv2ACLArn
	}
	if a.CognitoUserPoolARN != "" {
		annotations["alb.ingress.kubernetes.io/auth-type"] = "cognito"
		annotations["alb.ingress.kubernetes.io/auth-idp-cognito"] = fmt.Sprintf(`{"UserPoolArn":"%s","UserPoolClientId":"%s","UserPoolDomain":"%s"}`,
			a.CognitoUserPoolARN, a.CognitoUserPoolClientID, a.CognitoUserPoolDomain)
		if a.AuthOnUnauthenticated != "" {
			annotations["alb.ingress.kubernetes.io/auth-on-unauthenticated-request"] = a.AuthOnUnauthenticated
		}
	}
	if a.Actions != "" {
		annotations["alb.ingress.kubernetes.io/actions"] = a.Actions
	}

	return annotations
}

// GCPIngressConfig provides GCP-specific configuration
type GCPIngressConfig struct {
	GlobalStaticIPName   string
	RegionalStaticIPName string
	BackendConfigName    string
	ManagedCertificates  []string
	PreSharedCerts       []string
	HealthCheckPath      string
	CDNEnabled           bool
	IAPEnabled           bool
	IAPClientID          string
	IAPClientSecret      string
}

// ToAnnotations converts GCP config to annotations
func (g *GCPIngressConfig) ToAnnotations() map[string]string {
	annotations := make(map[string]string)

	if g.GlobalStaticIPName != "" {
		annotations["kubernetes.io/ingress.global-static-ip-name"] = g.GlobalStaticIPName
	}
	if g.RegionalStaticIPName != "" {
		annotations["kubernetes.io/ingress.regional-static-ip-name"] = g.RegionalStaticIPName
	}
	if g.BackendConfigName != "" {
		annotations["cloud.google.com/backend-config"] = fmt.Sprintf(`{"default": "%s"}`, g.BackendConfigName)
	}
	if len(g.ManagedCertificates) > 0 {
		annotations["networking.gke.io/managed-certificates"] = strings.Join(g.ManagedCertificates, ",")
	}
	if len(g.PreSharedCerts) > 0 {
		annotations["ingress.gcp.kubernetes.io/pre-shared-cert"] = strings.Join(g.PreSharedCerts, ",")
	}

	return annotations
}

// IngressBuilder provides a fluent interface for building Ingress resources
type IngressBuilder struct {
	config        K8sIngressConfig
	nginxConfig   *NginxIngressConfig
	traefikConfig *TraefikIngressConfig
	albConfig     *AWSALBIngressConfig
	gcpConfig     *GCPIngressConfig
}

// NewIngressBuilder creates a new Ingress builder
func NewIngressBuilder(name, namespace string) *IngressBuilder {
	return &IngressBuilder{
		config: K8sIngressConfig{
			Name:        name,
			Namespace:   namespace,
			Annotations: make(map[string]string),
			Labels:      make(map[string]string),
		},
	}
}

// WithIngressClass sets the ingress class
func (b *IngressBuilder) WithIngressClass(className string) *IngressBuilder {
	b.config.IngressClassName = className
	return b
}

// WithTLS adds TLS configuration
func (b *IngressBuilder) WithTLS(hosts []string, secretName string) *IngressBuilder {
	b.config.TLS = append(b.config.TLS, K8sIngressTLS{
		Hosts:      hosts,
		SecretName: secretName,
	})
	return b
}

// WithRule adds a routing rule
func (b *IngressBuilder) WithRule(host string, paths ...HTTPIngressPath) *IngressBuilder {
	b.config.Rules = append(b.config.Rules, IngressRule{
		Host: host,
		HTTP: &HTTPIngressRuleValue{Paths: paths},
	})
	return b
}

// WithDefaultBackend sets the default backend
func (b *IngressBuilder) WithDefaultBackend(serviceName string, port int) *IngressBuilder {
	b.config.DefaultBackend = &IngressBackend{
		Service: &IngressServiceBackend{
			Name: serviceName,
			Port: K8sIngressSvcPort{Number: port},
		},
	}
	return b
}

// WithNginx adds nginx-specific configuration
func (b *IngressBuilder) WithNginx(config *NginxIngressConfig) *IngressBuilder {
	b.nginxConfig = config
	return b
}

// WithTraefik adds Traefik-specific configuration
func (b *IngressBuilder) WithTraefik(config *TraefikIngressConfig) *IngressBuilder {
	b.traefikConfig = config
	return b
}

// WithAWSALB adds AWS ALB-specific configuration
func (b *IngressBuilder) WithAWSALB(config *AWSALBIngressConfig) *IngressBuilder {
	b.albConfig = config
	return b
}

// WithGCP adds GCP-specific configuration
func (b *IngressBuilder) WithGCP(config *GCPIngressConfig) *IngressBuilder {
	b.gcpConfig = config
	return b
}

// WithAnnotation adds a custom annotation
func (b *IngressBuilder) WithAnnotation(key, value string) *IngressBuilder {
	b.config.Annotations[key] = value
	return b
}

// WithLabel adds a custom label
func (b *IngressBuilder) WithLabel(key, value string) *IngressBuilder {
	b.config.Labels[key] = value
	return b
}

// Build creates the Ingress resource
func (b *IngressBuilder) Build() *IngressResource {
	im := NewIngressManager(b.config.Namespace)

	// Merge provider-specific annotations
	if b.nginxConfig != nil {
		for k, v := range b.nginxConfig.ToAnnotations() {
			b.config.Annotations[k] = v
		}
	}
	if b.traefikConfig != nil {
		for k, v := range b.traefikConfig.ToAnnotations() {
			b.config.Annotations[k] = v
		}
	}
	if b.albConfig != nil {
		for k, v := range b.albConfig.ToAnnotations() {
			b.config.Annotations[k] = v
		}
	}
	if b.gcpConfig != nil {
		for k, v := range b.gcpConfig.ToAnnotations() {
			b.config.Annotations[k] = v
		}
	}

	return im.GenerateIngress(b.config)
}

// PathPrefix creates an HTTPIngressPath with Prefix path type
func PathPrefix(path, serviceName string, port int) HTTPIngressPath {
	return HTTPIngressPath{
		Path:     path,
		PathType: "Prefix",
		Backend: &IngressBackend{
			Service: &IngressServiceBackend{
				Name: serviceName,
				Port: K8sIngressSvcPort{Number: port},
			},
		},
	}
}

// PathExact creates an HTTPIngressPath with Exact path type
func PathExact(path, serviceName string, port int) HTTPIngressPath {
	return HTTPIngressPath{
		Path:     path,
		PathType: "Exact",
		Backend: &IngressBackend{
			Service: &IngressServiceBackend{
				Name: serviceName,
				Port: K8sIngressSvcPort{Number: port},
			},
		},
	}
}

// PathImplementationSpecific creates an HTTPIngressPath with ImplementationSpecific path type
func PathImplementationSpecific(path, serviceName string, port int) HTTPIngressPath {
	return HTTPIngressPath{
		Path:     path,
		PathType: "ImplementationSpecific",
		Backend: &IngressBackend{
			Service: &IngressServiceBackend{
				Name: serviceName,
				Port: K8sIngressSvcPort{Number: port},
			},
		},
	}
}

// GenerateVaultaireIngress creates a complete Ingress configuration for Vaultaire
func GenerateVaultaireIngress(namespace, host, tlsSecretName, ingressClass string) *IngressResource {
	builder := NewIngressBuilder("vaultaire", namespace).
		WithIngressClass(ingressClass).
		WithTLS([]string{host}, tlsSecretName).
		WithRule(host,
			PathPrefix("/api", "vaultaire-api", 8080),
			PathPrefix("/health", "vaultaire-api", 8080),
			PathPrefix("/metrics", "vaultaire-api", 9090),
			PathPrefix("/", "vaultaire-web", 80),
		).
		WithNginx(&NginxIngressConfig{
			SSLRedirect:         true,
			ProxyBodySize:       "100m",
			ProxyReadTimeout:    300,
			ProxySendTimeout:    300,
			ProxyConnectTimeout: 60,
			CORSEnabled:         true,
			CORSAllowOrigin:     "*",
			CORSAllowMethods:    "GET, POST, PUT, DELETE, OPTIONS",
			CORSAllowHeaders:    "DNT,X-CustomHeader,Keep-Alive,User-Agent,X-Requested-With,If-Modified-Since,Cache-Control,Content-Type,Authorization",
		})

	return builder.Build()
}

// IngressSet represents a collection of Ingress resources
type IngressSet struct {
	Resources []*IngressResource
}

// Add adds an Ingress resource to the set
func (s *IngressSet) Add(r *IngressResource) {
	s.Resources = append(s.Resources, r)
}

// ToYAML converts all Ingress resources to multi-document YAML
func (s *IngressSet) ToYAML() (string, error) {
	var parts []string
	for _, r := range s.Resources {
		yaml, err := r.ToYAML()
		if err != nil {
			return "", err
		}
		parts = append(parts, yaml)
	}
	return strings.Join(parts, "---\n"), nil
}
