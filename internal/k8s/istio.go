// internal/k8s/istio.go
package k8s

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// IstioResourceKind represents Istio CRD types
type IstioResourceKind string

const (
	KindVirtualService        IstioResourceKind = "VirtualService"
	KindDestinationRule       IstioResourceKind = "DestinationRule"
	KindGateway               IstioResourceKind = "Gateway"
	KindServiceEntry          IstioResourceKind = "ServiceEntry"
	KindSidecar               IstioResourceKind = "Sidecar"
	KindAuthorizationPolicy   IstioResourceKind = "AuthorizationPolicy"
	KindPeerAuthentication    IstioResourceKind = "PeerAuthentication"
	KindRequestAuthentication IstioResourceKind = "RequestAuthentication"
	KindEnvoyFilter           IstioResourceKind = "EnvoyFilter"
)

// IstioManager manages Istio resources
type IstioManager struct {
	namespace string
	labels    map[string]string
}

// NewIstioManager creates a new Istio manager
func NewIstioManager(namespace string) *IstioManager {
	return &IstioManager{
		namespace: namespace,
		labels: map[string]string{
			"app.kubernetes.io/managed-by": "vaultaire",
		},
	}
}

// IstioResource represents a generic Istio resource
type IstioResource struct {
	APIVersion string                 `yaml:"apiVersion"`
	Kind       IstioResourceKind      `yaml:"kind"`
	Metadata   ManifestMetadata       `yaml:"metadata"`
	Spec       map[string]interface{} `yaml:"spec"`
}

// ToYAML converts the resource to YAML
func (r *IstioResource) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(r); err != nil {
		return "", fmt.Errorf("failed to encode Istio resource: %w", err)
	}
	return buf.String(), nil
}

// VirtualServiceConfig configures a VirtualService
type VirtualServiceConfig struct {
	Name      string
	Namespace string
	Hosts     []string
	Gateways  []string
	HTTP      []HTTPRoute
	TCP       []TCPRoute
	TLS       []TLSRoute
}

// HTTPRoute defines HTTP routing rules
type HTTPRoute struct {
	Name             string
	Match            []HTTPMatchRequest
	Route            []HTTPRouteDestination
	Timeout          string
	Retries          *HTTPRetry
	Fault            *HTTPFaultInjection
	Headers          *HeadersConfig
	Mirror           *HTTPRouteDestination
	MirrorPercentage *Percentage
	CorsPolicy       *CorsPolicy
}

// HTTPMatchRequest defines HTTP match conditions
type HTTPMatchRequest struct {
	URI           *StringMatch
	Scheme        *StringMatch
	Method        *StringMatch
	Authority     *StringMatch
	Headers       map[string]*StringMatch
	QueryParams   map[string]*StringMatch
	SourceLabels  map[string]string
	Port          int
	IgnoreUriCase bool
}

// StringMatch defines string matching rules
type StringMatch struct {
	Exact  string `yaml:"exact,omitempty"`
	Prefix string `yaml:"prefix,omitempty"`
	Regex  string `yaml:"regex,omitempty"`
}

// HTTPRouteDestination defines a route destination
type HTTPRouteDestination struct {
	Destination Destination    `yaml:"destination"`
	Weight      int            `yaml:"weight,omitempty"`
	Headers     *HeadersConfig `yaml:"headers,omitempty"`
}

// Destination defines the destination service
type Destination struct {
	Host   string        `yaml:"host"`
	Subset string        `yaml:"subset,omitempty"`
	Port   *PortSelector `yaml:"port,omitempty"`
}

// PortSelector selects a port by number or name
type PortSelector struct {
	Number int `yaml:"number,omitempty"`
}

// HTTPRetry defines retry policy
type HTTPRetry struct {
	Attempts      int    `yaml:"attempts"`
	PerTryTimeout string `yaml:"perTryTimeout,omitempty"`
	RetryOn       string `yaml:"retryOn,omitempty"`
}

// HTTPFaultInjection defines fault injection
type HTTPFaultInjection struct {
	Delay *FaultDelay `yaml:"delay,omitempty"`
	Abort *FaultAbort `yaml:"abort,omitempty"`
}

// FaultDelay defines delay fault injection
type FaultDelay struct {
	Percentage *Percentage `yaml:"percentage,omitempty"`
	FixedDelay string      `yaml:"fixedDelay"`
}

// FaultAbort defines abort fault injection
type FaultAbort struct {
	Percentage *Percentage `yaml:"percentage,omitempty"`
	HTTPStatus int         `yaml:"httpStatus"`
}

// Percentage defines a percentage value
type Percentage struct {
	Value float64 `yaml:"value"`
}

// HeadersConfig defines header manipulation
type HeadersConfig struct {
	Request  *HeaderOperations `yaml:"request,omitempty"`
	Response *HeaderOperations `yaml:"response,omitempty"`
}

// HeaderOperations defines header operations
type HeaderOperations struct {
	Set    map[string]string `yaml:"set,omitempty"`
	Add    map[string]string `yaml:"add,omitempty"`
	Remove []string          `yaml:"remove,omitempty"`
}

// CorsPolicy defines CORS policy
type CorsPolicy struct {
	AllowOrigins     []StringMatch `yaml:"allowOrigins,omitempty"`
	AllowMethods     []string      `yaml:"allowMethods,omitempty"`
	AllowHeaders     []string      `yaml:"allowHeaders,omitempty"`
	ExposeHeaders    []string      `yaml:"exposeHeaders,omitempty"`
	MaxAge           string        `yaml:"maxAge,omitempty"`
	AllowCredentials bool          `yaml:"allowCredentials,omitempty"`
}

// TCPRoute defines TCP routing
type TCPRoute struct {
	Match []TCPMatchRequest     `yaml:"match,omitempty"`
	Route []TCPRouteDestination `yaml:"route"`
}

// TCPMatchRequest defines TCP match conditions
type TCPMatchRequest struct {
	DestinationSubnets []string          `yaml:"destinationSubnets,omitempty"`
	Port               int               `yaml:"port,omitempty"`
	SourceLabels       map[string]string `yaml:"sourceLabels,omitempty"`
	Gateways           []string          `yaml:"gateways,omitempty"`
}

// TCPRouteDestination defines TCP route destination
type TCPRouteDestination struct {
	Destination Destination `yaml:"destination"`
	Weight      int         `yaml:"weight,omitempty"`
}

// TLSRoute defines TLS routing
type TLSRoute struct {
	Match []TLSMatchAttributes  `yaml:"match"`
	Route []TCPRouteDestination `yaml:"route"`
}

// TLSMatchAttributes defines TLS match conditions
type TLSMatchAttributes struct {
	SNIHosts           []string          `yaml:"sniHosts"`
	DestinationSubnets []string          `yaml:"destinationSubnets,omitempty"`
	Port               int               `yaml:"port,omitempty"`
	SourceLabels       map[string]string `yaml:"sourceLabels,omitempty"`
	Gateways           []string          `yaml:"gateways,omitempty"`
}

// GenerateVirtualService creates a VirtualService resource
func (im *IstioManager) GenerateVirtualService(config VirtualServiceConfig) *IstioResource {
	if config.Namespace == "" {
		config.Namespace = im.namespace
	}

	spec := map[string]interface{}{
		"hosts": config.Hosts,
	}

	if len(config.Gateways) > 0 {
		spec["gateways"] = config.Gateways
	}

	if len(config.HTTP) > 0 {
		httpRoutes := make([]map[string]interface{}, 0, len(config.HTTP))
		for _, route := range config.HTTP {
			httpRoute := make(map[string]interface{})

			if route.Name != "" {
				httpRoute["name"] = route.Name
			}

			if len(route.Match) > 0 {
				httpRoute["match"] = convertHTTPMatches(route.Match)
			}

			if len(route.Route) > 0 {
				httpRoute["route"] = convertHTTPDestinations(route.Route)
			}

			if route.Timeout != "" {
				httpRoute["timeout"] = route.Timeout
			}

			if route.Retries != nil {
				httpRoute["retries"] = route.Retries
			}

			if route.Fault != nil {
				httpRoute["fault"] = route.Fault
			}

			if route.Headers != nil {
				httpRoute["headers"] = route.Headers
			}

			if route.Mirror != nil {
				httpRoute["mirror"] = route.Mirror
			}

			if route.CorsPolicy != nil {
				httpRoute["corsPolicy"] = route.CorsPolicy
			}

			httpRoutes = append(httpRoutes, httpRoute)
		}
		spec["http"] = httpRoutes
	}

	if len(config.TCP) > 0 {
		spec["tcp"] = config.TCP
	}

	if len(config.TLS) > 0 {
		spec["tls"] = config.TLS
	}

	labels := copyStringMap(im.labels)

	return &IstioResource{
		APIVersion: "networking.istio.io/v1beta1",
		Kind:       KindVirtualService,
		Metadata: ManifestMetadata{
			Name:      config.Name,
			Namespace: config.Namespace,
			Labels:    labels,
		},
		Spec: spec,
	}
}

func convertHTTPMatches(matches []HTTPMatchRequest) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(matches))
	for _, match := range matches {
		m := make(map[string]interface{})

		if match.URI != nil {
			m["uri"] = match.URI
		}
		if match.Scheme != nil {
			m["scheme"] = match.Scheme
		}
		if match.Method != nil {
			m["method"] = match.Method
		}
		if match.Authority != nil {
			m["authority"] = match.Authority
		}
		if len(match.Headers) > 0 {
			m["headers"] = match.Headers
		}
		if len(match.QueryParams) > 0 {
			m["queryParams"] = match.QueryParams
		}
		if len(match.SourceLabels) > 0 {
			m["sourceLabels"] = match.SourceLabels
		}
		if match.Port > 0 {
			m["port"] = match.Port
		}
		if match.IgnoreUriCase {
			m["ignoreUriCase"] = true
		}

		result = append(result, m)
	}
	return result
}

func convertHTTPDestinations(destinations []HTTPRouteDestination) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(destinations))
	for _, dest := range destinations {
		d := map[string]interface{}{
			"destination": dest.Destination,
		}
		if dest.Weight > 0 {
			d["weight"] = dest.Weight
		}
		if dest.Headers != nil {
			d["headers"] = dest.Headers
		}
		result = append(result, d)
	}
	return result
}

// DestinationRuleConfig configures a DestinationRule
type DestinationRuleConfig struct {
	Name          string
	Namespace     string
	Host          string
	TrafficPolicy *TrafficPolicy
	Subsets       []Subset
}

// TrafficPolicy defines traffic policy
type TrafficPolicy struct {
	ConnectionPool    *ConnectionPoolSettings `yaml:"connectionPool,omitempty"`
	LoadBalancer      *LoadBalancerSettings   `yaml:"loadBalancer,omitempty"`
	OutlierDetection  *OutlierDetection       `yaml:"outlierDetection,omitempty"`
	TLS               *TLSSettings            `yaml:"tls,omitempty"`
	PortLevelSettings []PortTrafficPolicy     `yaml:"portLevelSettings,omitempty"`
}

// ConnectionPoolSettings defines connection pool
type ConnectionPoolSettings struct {
	TCP  *TCPSettings  `yaml:"tcp,omitempty"`
	HTTP *HTTPSettings `yaml:"http,omitempty"`
}

// TCPSettings defines TCP connection settings
type TCPSettings struct {
	MaxConnections int           `yaml:"maxConnections,omitempty"`
	ConnectTimeout string        `yaml:"connectTimeout,omitempty"`
	TCPKeepalive   *TCPKeepalive `yaml:"tcpKeepalive,omitempty"`
}

// TCPKeepalive defines TCP keepalive settings
type TCPKeepalive struct {
	Probes   int    `yaml:"probes,omitempty"`
	Time     string `yaml:"time,omitempty"`
	Interval string `yaml:"interval,omitempty"`
}

// HTTPSettings defines HTTP connection settings
type HTTPSettings struct {
	H2UpgradePolicy          string `yaml:"h2UpgradePolicy,omitempty"`
	HTTP1MaxPendingRequests  int    `yaml:"http1MaxPendingRequests,omitempty"`
	HTTP2MaxRequests         int    `yaml:"http2MaxRequests,omitempty"`
	MaxRequestsPerConnection int    `yaml:"maxRequestsPerConnection,omitempty"`
	MaxRetries               int    `yaml:"maxRetries,omitempty"`
	IdleTimeout              string `yaml:"idleTimeout,omitempty"`
}

// LoadBalancerSettings defines load balancer settings
type LoadBalancerSettings struct {
	Simple             string             `yaml:"simple,omitempty"`
	ConsistentHash     *ConsistentHashLB  `yaml:"consistentHash,omitempty"`
	LocalityLbSetting  *LocalityLBSetting `yaml:"localityLbSetting,omitempty"`
	WarmupDurationSecs string             `yaml:"warmupDurationSecs,omitempty"`
}

// ConsistentHashLB defines consistent hash load balancing
type ConsistentHashLB struct {
	HTTPHeaderName         string      `yaml:"httpHeaderName,omitempty"`
	HTTPCookie             *HTTPCookie `yaml:"httpCookie,omitempty"`
	UseSourceIP            bool        `yaml:"useSourceIp,omitempty"`
	HTTPQueryParameterName string      `yaml:"httpQueryParameterName,omitempty"`
	MinimumRingSize        int         `yaml:"minimumRingSize,omitempty"`
}

// HTTPCookie defines cookie-based hashing
type HTTPCookie struct {
	Name string `yaml:"name"`
	Path string `yaml:"path,omitempty"`
	TTL  string `yaml:"ttl,omitempty"`
}

// LocalityLBSetting defines locality load balancing
type LocalityLBSetting struct {
	Distribute []LocalityDistribution `yaml:"distribute,omitempty"`
	Failover   []LocalityFailover     `yaml:"failover,omitempty"`
	Enabled    *bool                  `yaml:"enabled,omitempty"`
}

// LocalityDistribution defines traffic distribution
type LocalityDistribution struct {
	From string         `yaml:"from"`
	To   map[string]int `yaml:"to"`
}

// LocalityFailover defines failover policy
type LocalityFailover struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// OutlierDetection defines outlier detection
type OutlierDetection struct {
	Consecutive5xxErrors           int    `yaml:"consecutive5xxErrors,omitempty"`
	ConsecutiveGatewayErrors       int    `yaml:"consecutiveGatewayErrors,omitempty"`
	ConsecutiveLocalOriginFailures int    `yaml:"consecutiveLocalOriginFailures,omitempty"`
	Interval                       string `yaml:"interval,omitempty"`
	BaseEjectionTime               string `yaml:"baseEjectionTime,omitempty"`
	MaxEjectionPercent             int    `yaml:"maxEjectionPercent,omitempty"`
	MinHealthPercent               int    `yaml:"minHealthPercent,omitempty"`
	SplitExternalLocalOriginErrors bool   `yaml:"splitExternalLocalOriginErrors,omitempty"`
}

// TLSSettings defines TLS settings
type TLSSettings struct {
	Mode               string   `yaml:"mode"`
	ClientCertificate  string   `yaml:"clientCertificate,omitempty"`
	PrivateKey         string   `yaml:"privateKey,omitempty"`
	CACertificates     string   `yaml:"caCertificates,omitempty"`
	CredentialName     string   `yaml:"credentialName,omitempty"`
	SubjectAltNames    []string `yaml:"subjectAltNames,omitempty"`
	SNI                string   `yaml:"sni,omitempty"`
	InsecureSkipVerify bool     `yaml:"insecureSkipVerify,omitempty"`
}

// PortTrafficPolicy defines per-port traffic policy
type PortTrafficPolicy struct {
	Port             *PortSelector           `yaml:"port"`
	ConnectionPool   *ConnectionPoolSettings `yaml:"connectionPool,omitempty"`
	LoadBalancer     *LoadBalancerSettings   `yaml:"loadBalancer,omitempty"`
	OutlierDetection *OutlierDetection       `yaml:"outlierDetection,omitempty"`
	TLS              *TLSSettings            `yaml:"tls,omitempty"`
}

// Subset defines a service subset
type Subset struct {
	Name          string            `yaml:"name"`
	Labels        map[string]string `yaml:"labels"`
	TrafficPolicy *TrafficPolicy    `yaml:"trafficPolicy,omitempty"`
}

// GenerateDestinationRule creates a DestinationRule resource
func (im *IstioManager) GenerateDestinationRule(config DestinationRuleConfig) *IstioResource {
	if config.Namespace == "" {
		config.Namespace = im.namespace
	}

	spec := map[string]interface{}{
		"host": config.Host,
	}

	if config.TrafficPolicy != nil {
		spec["trafficPolicy"] = config.TrafficPolicy
	}

	if len(config.Subsets) > 0 {
		spec["subsets"] = config.Subsets
	}

	labels := copyStringMap(im.labels)

	return &IstioResource{
		APIVersion: "networking.istio.io/v1beta1",
		Kind:       KindDestinationRule,
		Metadata: ManifestMetadata{
			Name:      config.Name,
			Namespace: config.Namespace,
			Labels:    labels,
		},
		Spec: spec,
	}
}

// GatewayConfig configures an Istio Gateway
type GatewayConfig struct {
	Name      string
	Namespace string
	Selector  map[string]string
	Servers   []GatewayServer
}

// GatewayServer defines a gateway server
type GatewayServer struct {
	Port  *GatewayPort `yaml:"port"`
	Hosts []string     `yaml:"hosts"`
	TLS   *GatewayTLS  `yaml:"tls,omitempty"`
	Name  string       `yaml:"name,omitempty"`
}

// GatewayPort defines gateway port
type GatewayPort struct {
	Number   int    `yaml:"number"`
	Name     string `yaml:"name"`
	Protocol string `yaml:"protocol"`
}

// GatewayTLS defines gateway TLS settings
type GatewayTLS struct {
	Mode               string   `yaml:"mode"`
	CredentialName     string   `yaml:"credentialName,omitempty"`
	ServerCertificate  string   `yaml:"serverCertificate,omitempty"`
	PrivateKey         string   `yaml:"privateKey,omitempty"`
	CACertificates     string   `yaml:"caCertificates,omitempty"`
	SubjectAltNames    []string `yaml:"subjectAltNames,omitempty"`
	MinProtocolVersion string   `yaml:"minProtocolVersion,omitempty"`
	MaxProtocolVersion string   `yaml:"maxProtocolVersion,omitempty"`
	CipherSuites       []string `yaml:"cipherSuites,omitempty"`
}

// GenerateGateway creates a Gateway resource
func (im *IstioManager) GenerateGateway(config GatewayConfig) *IstioResource {
	if config.Namespace == "" {
		config.Namespace = im.namespace
	}

	if config.Selector == nil {
		config.Selector = map[string]string{
			"istio": "ingressgateway",
		}
	}

	spec := map[string]interface{}{
		"selector": config.Selector,
		"servers":  config.Servers,
	}

	labels := copyStringMap(im.labels)

	return &IstioResource{
		APIVersion: "networking.istio.io/v1beta1",
		Kind:       KindGateway,
		Metadata: ManifestMetadata{
			Name:      config.Name,
			Namespace: config.Namespace,
			Labels:    labels,
		},
		Spec: spec,
	}
}

// AuthorizationPolicyConfig configures an AuthorizationPolicy
type AuthorizationPolicyConfig struct {
	Name      string
	Namespace string
	Selector  *WorkloadSelector
	Action    string // ALLOW, DENY, CUSTOM, AUDIT
	Rules     []AuthorizationRule
	Provider  *AuthorizationProvider
}

// WorkloadSelector selects workloads
type WorkloadSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

// AuthorizationRule defines authorization rules
type AuthorizationRule struct {
	From []RuleFrom  `yaml:"from,omitempty"`
	To   []RuleTo    `yaml:"to,omitempty"`
	When []Condition `yaml:"when,omitempty"`
}

// RuleFrom defines source rules
type RuleFrom struct {
	Source *Source `yaml:"source"`
}

// Source defines traffic source
type Source struct {
	Principals           []string `yaml:"principals,omitempty"`
	NotPrincipals        []string `yaml:"notPrincipals,omitempty"`
	RequestPrincipals    []string `yaml:"requestPrincipals,omitempty"`
	NotRequestPrincipals []string `yaml:"notRequestPrincipals,omitempty"`
	Namespaces           []string `yaml:"namespaces,omitempty"`
	NotNamespaces        []string `yaml:"notNamespaces,omitempty"`
	IpBlocks             []string `yaml:"ipBlocks,omitempty"`
	NotIpBlocks          []string `yaml:"notIpBlocks,omitempty"`
	RemoteIpBlocks       []string `yaml:"remoteIpBlocks,omitempty"`
	NotRemoteIpBlocks    []string `yaml:"notRemoteIpBlocks,omitempty"`
}

// RuleTo defines target rules
type RuleTo struct {
	Operation *Operation `yaml:"operation"`
}

// Operation defines operation rules
type Operation struct {
	Hosts      []string `yaml:"hosts,omitempty"`
	NotHosts   []string `yaml:"notHosts,omitempty"`
	Ports      []string `yaml:"ports,omitempty"`
	NotPorts   []string `yaml:"notPorts,omitempty"`
	Methods    []string `yaml:"methods,omitempty"`
	NotMethods []string `yaml:"notMethods,omitempty"`
	Paths      []string `yaml:"paths,omitempty"`
	NotPaths   []string `yaml:"notPaths,omitempty"`
}

// Condition defines additional conditions
type Condition struct {
	Key       string   `yaml:"key"`
	Values    []string `yaml:"values,omitempty"`
	NotValues []string `yaml:"notValues,omitempty"`
}

// AuthorizationProvider defines external authorization provider
type AuthorizationProvider struct {
	Name string `yaml:"name"`
}

// GenerateAuthorizationPolicy creates an AuthorizationPolicy resource
func (im *IstioManager) GenerateAuthorizationPolicy(config AuthorizationPolicyConfig) *IstioResource {
	if config.Namespace == "" {
		config.Namespace = im.namespace
	}

	spec := make(map[string]interface{})

	if config.Selector != nil {
		spec["selector"] = config.Selector
	}

	if config.Action != "" {
		spec["action"] = config.Action
	}

	if len(config.Rules) > 0 {
		spec["rules"] = config.Rules
	}

	if config.Provider != nil {
		spec["provider"] = config.Provider
	}

	labels := copyStringMap(im.labels)

	return &IstioResource{
		APIVersion: "security.istio.io/v1beta1",
		Kind:       KindAuthorizationPolicy,
		Metadata: ManifestMetadata{
			Name:      config.Name,
			Namespace: config.Namespace,
			Labels:    labels,
		},
		Spec: spec,
	}
}

// PeerAuthenticationConfig configures PeerAuthentication
type PeerAuthenticationConfig struct {
	Name          string
	Namespace     string
	Selector      *WorkloadSelector
	MTLS          *PeerAuthenticationMTLS
	PortLevelMTLS map[int]*PeerAuthenticationMTLS
}

// PeerAuthenticationMTLS defines mTLS settings
type PeerAuthenticationMTLS struct {
	Mode string `yaml:"mode"` // STRICT, PERMISSIVE, DISABLE, UNSET
}

// GeneratePeerAuthentication creates a PeerAuthentication resource
func (im *IstioManager) GeneratePeerAuthentication(config PeerAuthenticationConfig) *IstioResource {
	if config.Namespace == "" {
		config.Namespace = im.namespace
	}

	spec := make(map[string]interface{})

	if config.Selector != nil {
		spec["selector"] = config.Selector
	}

	if config.MTLS != nil {
		spec["mtls"] = config.MTLS
	}

	if len(config.PortLevelMTLS) > 0 {
		spec["portLevelMtls"] = config.PortLevelMTLS
	}

	labels := copyStringMap(im.labels)

	return &IstioResource{
		APIVersion: "security.istio.io/v1beta1",
		Kind:       KindPeerAuthentication,
		Metadata: ManifestMetadata{
			Name:      config.Name,
			Namespace: config.Namespace,
			Labels:    labels,
		},
		Spec: spec,
	}
}

// RequestAuthenticationConfig configures RequestAuthentication
type RequestAuthenticationConfig struct {
	Name      string
	Namespace string
	Selector  *WorkloadSelector
	JWTRules  []JWTRule
}

// JWTRule defines JWT authentication rules
type JWTRule struct {
	Issuer                string          `yaml:"issuer"`
	Audiences             []string        `yaml:"audiences,omitempty"`
	JwksURI               string          `yaml:"jwksUri,omitempty"`
	Jwks                  string          `yaml:"jwks,omitempty"`
	FromHeaders           []JWTHeader     `yaml:"fromHeaders,omitempty"`
	FromParams            []string        `yaml:"fromParams,omitempty"`
	OutputPayloadToHeader string          `yaml:"outputPayloadToHeader,omitempty"`
	ForwardOriginalToken  bool            `yaml:"forwardOriginalToken,omitempty"`
	OutputClaimToHeaders  []ClaimToHeader `yaml:"outputClaimToHeaders,omitempty"`
}

// JWTHeader defines where to extract JWT
type JWTHeader struct {
	Name   string `yaml:"name"`
	Prefix string `yaml:"prefix,omitempty"`
}

// ClaimToHeader maps claims to headers
type ClaimToHeader struct {
	Header string `yaml:"header"`
	Claim  string `yaml:"claim"`
}

// GenerateRequestAuthentication creates a RequestAuthentication resource
func (im *IstioManager) GenerateRequestAuthentication(config RequestAuthenticationConfig) *IstioResource {
	if config.Namespace == "" {
		config.Namespace = im.namespace
	}

	spec := make(map[string]interface{})

	if config.Selector != nil {
		spec["selector"] = config.Selector
	}

	if len(config.JWTRules) > 0 {
		spec["jwtRules"] = config.JWTRules
	}

	labels := copyStringMap(im.labels)

	return &IstioResource{
		APIVersion: "security.istio.io/v1beta1",
		Kind:       KindRequestAuthentication,
		Metadata: ManifestMetadata{
			Name:      config.Name,
			Namespace: config.Namespace,
			Labels:    labels,
		},
		Spec: spec,
	}
}

// IstioServicePort defines service port for Istio ServiceEntry
type IstioServicePort struct {
	Number     int    `yaml:"number"`
	Protocol   string `yaml:"protocol"`
	Name       string `yaml:"name"`
	TargetPort int    `yaml:"targetPort,omitempty"`
}

// ServiceEntryConfig configures a ServiceEntry
type ServiceEntryConfig struct {
	Name       string
	Namespace  string
	Hosts      []string
	Addresses  []string
	Ports      []IstioServicePort // Changed from ServicePort
	Location   string             // MESH_EXTERNAL, MESH_INTERNAL
	Resolution string             // NONE, STATIC, DNS, DNS_ROUND_ROBIN
	Endpoints  []WorkloadEntry
}

// WorkloadEntry defines a workload endpoint
type WorkloadEntry struct {
	Address  string            `yaml:"address"`
	Ports    map[string]int    `yaml:"ports,omitempty"`
	Labels   map[string]string `yaml:"labels,omitempty"`
	Network  string            `yaml:"network,omitempty"`
	Locality string            `yaml:"locality,omitempty"`
	Weight   int               `yaml:"weight,omitempty"`
}

// GenerateServiceEntry creates a ServiceEntry resource
func (im *IstioManager) GenerateServiceEntry(config ServiceEntryConfig) *IstioResource {
	if config.Namespace == "" {
		config.Namespace = im.namespace
	}

	spec := map[string]interface{}{
		"hosts": config.Hosts,
		"ports": config.Ports,
	}

	if len(config.Addresses) > 0 {
		spec["addresses"] = config.Addresses
	}

	if config.Location != "" {
		spec["location"] = config.Location
	}

	if config.Resolution != "" {
		spec["resolution"] = config.Resolution
	}

	if len(config.Endpoints) > 0 {
		spec["endpoints"] = config.Endpoints
	}

	labels := copyStringMap(im.labels)

	return &IstioResource{
		APIVersion: "networking.istio.io/v1beta1",
		Kind:       KindServiceEntry,
		Metadata: ManifestMetadata{
			Name:      config.Name,
			Namespace: config.Namespace,
			Labels:    labels,
		},
		Spec: spec,
	}
}

// SidecarConfig configures a Sidecar resource
type SidecarConfig struct {
	Name                  string
	Namespace             string
	WorkloadSelector      *WorkloadSelector
	Ingress               []IstioIngressListener
	Egress                []IstioEgressListener
	OutboundTrafficPolicy *OutboundTrafficPolicy
}

// IstioIngressListener defines sidecar ingress
type IstioIngressListener struct {
	Port            *SidecarPort `yaml:"port"`
	Bind            string       `yaml:"bind,omitempty"`
	CaptureMode     string       `yaml:"captureMode,omitempty"`
	DefaultEndpoint string       `yaml:"defaultEndpoint,omitempty"`
}

// IstioEgressListener defines sidecar egress
type IstioEgressListener struct {
	Port        *SidecarPort `yaml:"port,omitempty"`
	Bind        string       `yaml:"bind,omitempty"`
	CaptureMode string       `yaml:"captureMode,omitempty"`
	Hosts       []string     `yaml:"hosts"`
}

// SidecarPort defines sidecar port
type SidecarPort struct {
	Number   int    `yaml:"number"`
	Protocol string `yaml:"protocol"`
	Name     string `yaml:"name,omitempty"`
}

// OutboundTrafficPolicy defines outbound traffic policy
type OutboundTrafficPolicy struct {
	Mode string `yaml:"mode"` // REGISTRY_ONLY, ALLOW_ANY
}

// GenerateSidecar creates a Sidecar resource
func (im *IstioManager) GenerateSidecar(config SidecarConfig) *IstioResource {
	if config.Namespace == "" {
		config.Namespace = im.namespace
	}

	spec := make(map[string]interface{})

	if config.WorkloadSelector != nil {
		spec["workloadSelector"] = config.WorkloadSelector
	}

	if len(config.Ingress) > 0 {
		spec["ingress"] = config.Ingress
	}

	if len(config.Egress) > 0 {
		spec["egress"] = config.Egress
	}

	if config.OutboundTrafficPolicy != nil {
		spec["outboundTrafficPolicy"] = config.OutboundTrafficPolicy
	}

	labels := copyStringMap(im.labels)

	return &IstioResource{
		APIVersion: "networking.istio.io/v1beta1",
		Kind:       KindSidecar,
		Metadata: ManifestMetadata{
			Name:      config.Name,
			Namespace: config.Namespace,
			Labels:    labels,
		},
		Spec: spec,
	}
}

// IstioResourceSet represents a collection of Istio resources
type IstioResourceSet struct {
	Resources []*IstioResource
}

// Add adds an Istio resource to the set
func (s *IstioResourceSet) Add(r *IstioResource) {
	s.Resources = append(s.Resources, r)
}

// ToYAML converts all resources to a multi-document YAML
func (s *IstioResourceSet) ToYAML() (string, error) {
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

// GenerateVaultaireIstioConfig generates a complete Istio configuration for Vaultaire
func GenerateVaultaireIstioConfig(namespace, host string) (*IstioResourceSet, error) {
	im := NewIstioManager(namespace)
	set := &IstioResourceSet{}

	// Gateway
	gateway := im.GenerateGateway(GatewayConfig{
		Name: "vaultaire-gateway",
		Servers: []GatewayServer{
			{
				Port: &GatewayPort{
					Number:   443,
					Name:     "https",
					Protocol: "HTTPS",
				},
				Hosts: []string{host},
				TLS: &GatewayTLS{
					Mode:           "SIMPLE",
					CredentialName: "vaultaire-tls",
				},
			},
			{
				Port: &GatewayPort{
					Number:   80,
					Name:     "http",
					Protocol: "HTTP",
				},
				Hosts: []string{host},
			},
		},
	})
	set.Add(gateway)

	// VirtualService
	vs := im.GenerateVirtualService(VirtualServiceConfig{
		Name:     "vaultaire",
		Hosts:    []string{host},
		Gateways: []string{"vaultaire-gateway"},
		HTTP: []HTTPRoute{
			{
				Name: "main",
				Match: []HTTPMatchRequest{
					{URI: &StringMatch{Prefix: "/"}},
				},
				Route: []HTTPRouteDestination{
					{
						Destination: Destination{
							Host: "vaultaire",
							Port: &PortSelector{Number: 80},
						},
						Weight: 100,
					},
				},
				Retries: &HTTPRetry{
					Attempts:      3,
					PerTryTimeout: "2s",
					RetryOn:       "5xx,reset,connect-failure",
				},
				Timeout: "30s",
			},
		},
	})
	set.Add(vs)

	// DestinationRule
	dr := im.GenerateDestinationRule(DestinationRuleConfig{
		Name: "vaultaire",
		Host: "vaultaire",
		TrafficPolicy: &TrafficPolicy{
			ConnectionPool: &ConnectionPoolSettings{
				TCP: &TCPSettings{
					MaxConnections: 100,
					ConnectTimeout: "10s",
				},
				HTTP: &HTTPSettings{
					H2UpgradePolicy:          "UPGRADE",
					HTTP2MaxRequests:         1000,
					MaxRequestsPerConnection: 100,
				},
			},
			OutlierDetection: &OutlierDetection{
				Consecutive5xxErrors: 5,
				Interval:             "30s",
				BaseEjectionTime:     "30s",
				MaxEjectionPercent:   50,
			},
		},
		Subsets: []Subset{
			{
				Name:   "v1",
				Labels: map[string]string{"version": "v1"},
			},
		},
	})
	set.Add(dr)

	// PeerAuthentication (mTLS)
	pa := im.GeneratePeerAuthentication(PeerAuthenticationConfig{
		Name: "vaultaire-mtls",
		Selector: &WorkloadSelector{
			MatchLabels: map[string]string{"app": "vaultaire"},
		},
		MTLS: &PeerAuthenticationMTLS{
			Mode: "STRICT",
		},
	})
	set.Add(pa)

	// AuthorizationPolicy
	authz := im.GenerateAuthorizationPolicy(AuthorizationPolicyConfig{
		Name: "vaultaire-authz",
		Selector: &WorkloadSelector{
			MatchLabels: map[string]string{"app": "vaultaire"},
		},
		Action: "ALLOW",
		Rules: []AuthorizationRule{
			{
				To: []RuleTo{
					{
						Operation: &Operation{
							Methods: []string{"GET", "POST", "PUT", "DELETE", "HEAD"},
						},
					},
				},
			},
		},
	})
	set.Add(authz)

	return set, nil
}

// CanaryDeploymentConfig defines canary deployment settings
type CanaryDeploymentConfig struct {
	ServiceName   string
	Namespace     string
	StableVersion string
	CanaryVersion string
	CanaryWeight  int
	Headers       map[string]string // Header-based routing to canary
}

// GenerateCanaryVirtualService creates VirtualService for canary deployment
func (im *IstioManager) GenerateCanaryVirtualService(config CanaryDeploymentConfig) *IstioResource {
	if config.Namespace == "" {
		config.Namespace = im.namespace
	}

	routes := []HTTPRoute{}

	// Header-based routing to canary (for testing)
	if len(config.Headers) > 0 {
		headerMatch := make(map[string]*StringMatch)
		for k, v := range config.Headers {
			headerMatch[k] = &StringMatch{Exact: v}
		}

		routes = append(routes, HTTPRoute{
			Name: "canary-header",
			Match: []HTTPMatchRequest{
				{Headers: headerMatch},
			},
			Route: []HTTPRouteDestination{
				{
					Destination: Destination{
						Host:   config.ServiceName,
						Subset: config.CanaryVersion,
					},
					Weight: 100,
				},
			},
		})
	}

	// Weight-based routing
	routes = append(routes, HTTPRoute{
		Name: "primary",
		Route: []HTTPRouteDestination{
			{
				Destination: Destination{
					Host:   config.ServiceName,
					Subset: config.StableVersion,
				},
				Weight: 100 - config.CanaryWeight,
			},
			{
				Destination: Destination{
					Host:   config.ServiceName,
					Subset: config.CanaryVersion,
				},
				Weight: config.CanaryWeight,
			},
		},
	})

	return im.GenerateVirtualService(VirtualServiceConfig{
		Name:  config.ServiceName + "-canary",
		Hosts: []string{config.ServiceName},
		HTTP:  routes,
	})
}

// CircuitBreakerConfig defines circuit breaker settings
type CircuitBreakerConfig struct {
	ServiceName          string
	Namespace            string
	MaxConnections       int
	HTTP2MaxRequests     int
	MaxRequestsPerConn   int
	Consecutive5xxErrors int
	EjectionInterval     time.Duration
	BaseEjectionTime     time.Duration
	MaxEjectionPercent   int
}

// GenerateCircuitBreaker creates a DestinationRule with circuit breaker
func (im *IstioManager) GenerateCircuitBreaker(config CircuitBreakerConfig) *IstioResource {
	if config.Namespace == "" {
		config.Namespace = im.namespace
	}

	// Set defaults
	if config.MaxConnections == 0 {
		config.MaxConnections = 100
	}
	if config.HTTP2MaxRequests == 0 {
		config.HTTP2MaxRequests = 1000
	}
	if config.Consecutive5xxErrors == 0 {
		config.Consecutive5xxErrors = 5
	}
	if config.EjectionInterval == 0 {
		config.EjectionInterval = 10 * time.Second
	}
	if config.BaseEjectionTime == 0 {
		config.BaseEjectionTime = 30 * time.Second
	}
	if config.MaxEjectionPercent == 0 {
		config.MaxEjectionPercent = 50
	}

	return im.GenerateDestinationRule(DestinationRuleConfig{
		Name: config.ServiceName + "-circuit-breaker",
		Host: config.ServiceName,
		TrafficPolicy: &TrafficPolicy{
			ConnectionPool: &ConnectionPoolSettings{
				TCP: &TCPSettings{
					MaxConnections: config.MaxConnections,
				},
				HTTP: &HTTPSettings{
					HTTP2MaxRequests:         config.HTTP2MaxRequests,
					MaxRequestsPerConnection: config.MaxRequestsPerConn,
				},
			},
			OutlierDetection: &OutlierDetection{
				Consecutive5xxErrors: config.Consecutive5xxErrors,
				Interval:             config.EjectionInterval.String(),
				BaseEjectionTime:     config.BaseEjectionTime.String(),
				MaxEjectionPercent:   config.MaxEjectionPercent,
			},
		},
	})
}
