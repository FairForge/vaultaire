// internal/k8s/netpol.go
package k8s

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// NetworkPolicyManager manages Kubernetes NetworkPolicy resources
type NetworkPolicyManager struct {
	namespace string
	labels    map[string]string
}

// NewNetworkPolicyManager creates a new NetworkPolicy manager
func NewNetworkPolicyManager(namespace string) *NetworkPolicyManager {
	return &NetworkPolicyManager{
		namespace: namespace,
		labels: map[string]string{
			"app.kubernetes.io/managed-by": "vaultaire",
		},
	}
}

// NetworkPolicyConfig configures a NetworkPolicy
type NetworkPolicyConfig struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string

	// Pod selector - which pods this policy applies to
	PodSelector map[string]string

	// Policy types
	PolicyTypes []PolicyType

	// Ingress rules
	Ingress []NetworkPolicyIngressRule

	// Egress rules
	Egress []NetworkPolicyEgressRule
}

// PolicyType defines the type of network policy
type PolicyType string

const (
	PolicyTypeIngress PolicyType = "Ingress"
	PolicyTypeEgress  PolicyType = "Egress"
)

// NetworkPolicyIngressRule defines an ingress rule
type NetworkPolicyIngressRule struct {
	From  []NetworkPolicyPeer
	Ports []NetworkPolicyPort
}

// NetworkPolicyEgressRule defines an egress rule
type NetworkPolicyEgressRule struct {
	To    []NetworkPolicyPeer
	Ports []NetworkPolicyPort
}

// NetworkPolicyPeer defines a peer for network policy
type NetworkPolicyPeer struct {
	PodSelector       map[string]string
	NamespaceSelector map[string]string
	IPBlock           *IPBlock
}

// IPBlock defines an IP block
type IPBlock struct {
	CIDR   string
	Except []string
}

// NetworkPolicyPort defines a port for network policy
type NetworkPolicyPort struct {
	Protocol string // TCP, UDP, SCTP
	Port     string // Can be number or named port
	EndPort  int32  // For port ranges
}

// NetworkPolicyResource represents a NetworkPolicy
type NetworkPolicyResource struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   ManifestMetadata  `yaml:"metadata"`
	Spec       NetworkPolicySpec `yaml:"spec"`
}

// NetworkPolicySpec defines NetworkPolicy specification
type NetworkPolicySpec struct {
	PodSelector NetworkPolicyLabelSelector `yaml:"podSelector"`
	PolicyTypes []string                   `yaml:"policyTypes,omitempty"`
	Ingress     []NetworkPolicyIngressSpec `yaml:"ingress,omitempty"`
	Egress      []NetworkPolicyEgressSpec  `yaml:"egress,omitempty"`
}

// NetworkPolicyLabelSelector for pod selection
type NetworkPolicyLabelSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels,omitempty"`
}

// NetworkPolicyIngressSpec for spec
type NetworkPolicyIngressSpec struct {
	From  []NetworkPolicyPeerSpec `yaml:"from,omitempty"`
	Ports []NetworkPolicyPortSpec `yaml:"ports,omitempty"`
}

// NetworkPolicyEgressSpec for spec
type NetworkPolicyEgressSpec struct {
	To    []NetworkPolicyPeerSpec `yaml:"to,omitempty"`
	Ports []NetworkPolicyPortSpec `yaml:"ports,omitempty"`
}

// NetworkPolicyPeerSpec for spec
type NetworkPolicyPeerSpec struct {
	PodSelector       *NetworkPolicyLabelSelector `yaml:"podSelector,omitempty"`
	NamespaceSelector *NetworkPolicyLabelSelector `yaml:"namespaceSelector,omitempty"`
	IPBlock           *IPBlockSpec                `yaml:"ipBlock,omitempty"`
}

// IPBlockSpec for spec
type IPBlockSpec struct {
	CIDR   string   `yaml:"cidr"`
	Except []string `yaml:"except,omitempty"`
}

// NetworkPolicyPortSpec for spec
type NetworkPolicyPortSpec struct {
	Protocol string `yaml:"protocol,omitempty"`
	Port     any    `yaml:"port,omitempty"`
	EndPort  *int32 `yaml:"endPort,omitempty"`
}

// GenerateNetworkPolicy creates a NetworkPolicy resource
func (npm *NetworkPolicyManager) GenerateNetworkPolicy(config NetworkPolicyConfig) *NetworkPolicyResource {
	if config.Namespace == "" {
		config.Namespace = npm.namespace
	}

	labels := copyStringMap(npm.labels)
	for k, v := range config.Labels {
		labels[k] = v
	}

	np := &NetworkPolicyResource{
		APIVersion: "networking.k8s.io/v1",
		Kind:       "NetworkPolicy",
		Metadata: ManifestMetadata{
			Name:        config.Name,
			Namespace:   config.Namespace,
			Labels:      labels,
			Annotations: config.Annotations,
		},
		Spec: NetworkPolicySpec{
			PodSelector: NetworkPolicyLabelSelector{
				MatchLabels: config.PodSelector,
			},
		},
	}

	// Policy types
	for _, pt := range config.PolicyTypes {
		np.Spec.PolicyTypes = append(np.Spec.PolicyTypes, string(pt))
	}

	// Ingress rules
	for _, rule := range config.Ingress {
		ingressSpec := NetworkPolicyIngressSpec{}

		for _, from := range rule.From {
			peer := convertPeer(from)
			ingressSpec.From = append(ingressSpec.From, peer)
		}

		for _, port := range rule.Ports {
			portSpec := convertPort(port)
			ingressSpec.Ports = append(ingressSpec.Ports, portSpec)
		}

		np.Spec.Ingress = append(np.Spec.Ingress, ingressSpec)
	}

	// Egress rules
	for _, rule := range config.Egress {
		egressSpec := NetworkPolicyEgressSpec{}

		for _, to := range rule.To {
			peer := convertPeer(to)
			egressSpec.To = append(egressSpec.To, peer)
		}

		for _, port := range rule.Ports {
			portSpec := convertPort(port)
			egressSpec.Ports = append(egressSpec.Ports, portSpec)
		}

		np.Spec.Egress = append(np.Spec.Egress, egressSpec)
	}

	return np
}

func convertPeer(peer NetworkPolicyPeer) NetworkPolicyPeerSpec {
	spec := NetworkPolicyPeerSpec{}

	if len(peer.PodSelector) > 0 {
		spec.PodSelector = &NetworkPolicyLabelSelector{
			MatchLabels: peer.PodSelector,
		}
	}

	if len(peer.NamespaceSelector) > 0 {
		spec.NamespaceSelector = &NetworkPolicyLabelSelector{
			MatchLabels: peer.NamespaceSelector,
		}
	}

	if peer.IPBlock != nil {
		spec.IPBlock = &IPBlockSpec{
			CIDR:   peer.IPBlock.CIDR,
			Except: peer.IPBlock.Except,
		}
	}

	return spec
}

func convertPort(port NetworkPolicyPort) NetworkPolicyPortSpec {
	spec := NetworkPolicyPortSpec{
		Protocol: port.Protocol,
	}

	// Try to parse as int, otherwise use string (named port)
	if port.Port != "" {
		var portNum int
		if _, err := fmt.Sscanf(port.Port, "%d", &portNum); err == nil {
			spec.Port = portNum
		} else {
			spec.Port = port.Port
		}
	}

	if port.EndPort > 0 {
		spec.EndPort = &port.EndPort
	}

	return spec
}

// ToYAML converts NetworkPolicy to YAML
func (np *NetworkPolicyResource) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(np); err != nil {
		return "", fmt.Errorf("failed to encode NetworkPolicy: %w", err)
	}
	return buf.String(), nil
}

// NetworkPolicyBuilder provides a fluent interface for building NetworkPolicy
type NetworkPolicyBuilder struct {
	config NetworkPolicyConfig
}

// NewNetworkPolicyBuilder creates a new NetworkPolicy builder
func NewNetworkPolicyBuilder(name, namespace string) *NetworkPolicyBuilder {
	return &NetworkPolicyBuilder{
		config: NetworkPolicyConfig{
			Name:      name,
			Namespace: namespace,
		},
	}
}

// ForPods sets the pod selector
func (b *NetworkPolicyBuilder) ForPods(selector map[string]string) *NetworkPolicyBuilder {
	b.config.PodSelector = selector
	return b
}

// WithPolicyTypes sets policy types
func (b *NetworkPolicyBuilder) WithPolicyTypes(types ...PolicyType) *NetworkPolicyBuilder {
	b.config.PolicyTypes = types
	return b
}

// AllowIngressFromPods allows ingress from pods matching selector
func (b *NetworkPolicyBuilder) AllowIngressFromPods(selector map[string]string, ports ...NetworkPolicyPort) *NetworkPolicyBuilder {
	rule := NetworkPolicyIngressRule{
		From:  []NetworkPolicyPeer{{PodSelector: selector}},
		Ports: ports,
	}
	b.config.Ingress = append(b.config.Ingress, rule)
	return b
}

// AllowIngressFromNamespace allows ingress from a namespace
func (b *NetworkPolicyBuilder) AllowIngressFromNamespace(nsSelector map[string]string, ports ...NetworkPolicyPort) *NetworkPolicyBuilder {
	rule := NetworkPolicyIngressRule{
		From:  []NetworkPolicyPeer{{NamespaceSelector: nsSelector}},
		Ports: ports,
	}
	b.config.Ingress = append(b.config.Ingress, rule)
	return b
}

// AllowIngressFromCIDR allows ingress from a CIDR block
func (b *NetworkPolicyBuilder) AllowIngressFromCIDR(cidr string, except []string, ports ...NetworkPolicyPort) *NetworkPolicyBuilder {
	rule := NetworkPolicyIngressRule{
		From:  []NetworkPolicyPeer{{IPBlock: &IPBlock{CIDR: cidr, Except: except}}},
		Ports: ports,
	}
	b.config.Ingress = append(b.config.Ingress, rule)
	return b
}

// AllowAllIngress allows all ingress traffic
func (b *NetworkPolicyBuilder) AllowAllIngress() *NetworkPolicyBuilder {
	b.config.Ingress = append(b.config.Ingress, NetworkPolicyIngressRule{})
	return b
}

// AllowEgressToPods allows egress to pods matching selector
func (b *NetworkPolicyBuilder) AllowEgressToPods(selector map[string]string, ports ...NetworkPolicyPort) *NetworkPolicyBuilder {
	rule := NetworkPolicyEgressRule{
		To:    []NetworkPolicyPeer{{PodSelector: selector}},
		Ports: ports,
	}
	b.config.Egress = append(b.config.Egress, rule)
	return b
}

// AllowEgressToNamespace allows egress to a namespace
func (b *NetworkPolicyBuilder) AllowEgressToNamespace(nsSelector map[string]string, ports ...NetworkPolicyPort) *NetworkPolicyBuilder {
	rule := NetworkPolicyEgressRule{
		To:    []NetworkPolicyPeer{{NamespaceSelector: nsSelector}},
		Ports: ports,
	}
	b.config.Egress = append(b.config.Egress, rule)
	return b
}

// AllowEgressToCIDR allows egress to a CIDR block
func (b *NetworkPolicyBuilder) AllowEgressToCIDR(cidr string, except []string, ports ...NetworkPolicyPort) *NetworkPolicyBuilder {
	rule := NetworkPolicyEgressRule{
		To:    []NetworkPolicyPeer{{IPBlock: &IPBlock{CIDR: cidr, Except: except}}},
		Ports: ports,
	}
	b.config.Egress = append(b.config.Egress, rule)
	return b
}

// AllowDNS allows egress DNS (UDP/TCP 53)
func (b *NetworkPolicyBuilder) AllowDNS() *NetworkPolicyBuilder {
	rule := NetworkPolicyEgressRule{
		Ports: []NetworkPolicyPort{
			{Protocol: "UDP", Port: "53"},
			{Protocol: "TCP", Port: "53"},
		},
	}
	b.config.Egress = append(b.config.Egress, rule)
	return b
}

// AllowAllEgress allows all egress traffic
func (b *NetworkPolicyBuilder) AllowAllEgress() *NetworkPolicyBuilder {
	b.config.Egress = append(b.config.Egress, NetworkPolicyEgressRule{})
	return b
}

// DenyAllIngress denies all ingress (empty ingress with Ingress policy type)
func (b *NetworkPolicyBuilder) DenyAllIngress() *NetworkPolicyBuilder {
	b.config.PolicyTypes = append(b.config.PolicyTypes, PolicyTypeIngress)
	// No ingress rules = deny all
	return b
}

// DenyAllEgress denies all egress (empty egress with Egress policy type)
func (b *NetworkPolicyBuilder) DenyAllEgress() *NetworkPolicyBuilder {
	b.config.PolicyTypes = append(b.config.PolicyTypes, PolicyTypeEgress)
	// No egress rules = deny all
	return b
}

// WithAnnotation adds an annotation
func (b *NetworkPolicyBuilder) WithAnnotation(key, value string) *NetworkPolicyBuilder {
	if b.config.Annotations == nil {
		b.config.Annotations = make(map[string]string)
	}
	b.config.Annotations[key] = value
	return b
}

// WithLabel adds a label
func (b *NetworkPolicyBuilder) WithLabel(key, value string) *NetworkPolicyBuilder {
	if b.config.Labels == nil {
		b.config.Labels = make(map[string]string)
	}
	b.config.Labels[key] = value
	return b
}

// Build creates the NetworkPolicy resource
func (b *NetworkPolicyBuilder) Build() *NetworkPolicyResource {
	npm := NewNetworkPolicyManager(b.config.Namespace)
	return npm.GenerateNetworkPolicy(b.config)
}

// TCP creates a TCP port
func TCP(port string) NetworkPolicyPort {
	return NetworkPolicyPort{Protocol: "TCP", Port: port}
}

// UDP creates a UDP port
func UDP(port string) NetworkPolicyPort {
	return NetworkPolicyPort{Protocol: "UDP", Port: port}
}

// PortRange creates a port range
func PortRange(protocol, startPort string, endPort int32) NetworkPolicyPort {
	return NetworkPolicyPort{Protocol: protocol, Port: startPort, EndPort: endPort}
}

// GenerateDefaultDenyIngress creates a default deny ingress policy
func GenerateDefaultDenyIngress(namespace string) *NetworkPolicyResource {
	return NewNetworkPolicyBuilder("default-deny-ingress", namespace).
		ForPods(map[string]string{}).
		WithPolicyTypes(PolicyTypeIngress).
		Build()
}

// GenerateDefaultDenyEgress creates a default deny egress policy
func GenerateDefaultDenyEgress(namespace string) *NetworkPolicyResource {
	return NewNetworkPolicyBuilder("default-deny-egress", namespace).
		ForPods(map[string]string{}).
		WithPolicyTypes(PolicyTypeEgress).
		Build()
}

// GenerateDefaultDenyAll creates a default deny all policy
func GenerateDefaultDenyAll(namespace string) *NetworkPolicyResource {
	return NewNetworkPolicyBuilder("default-deny-all", namespace).
		ForPods(map[string]string{}).
		WithPolicyTypes(PolicyTypeIngress, PolicyTypeEgress).
		Build()
}

// GenerateVaultaireNetworkPolicies creates NetworkPolicies for Vaultaire
func GenerateVaultaireNetworkPolicies(namespace string) []*NetworkPolicyResource {
	policies := []*NetworkPolicyResource{}

	// Default deny all ingress
	policies = append(policies, GenerateDefaultDenyIngress(namespace))

	// Allow API server to receive traffic
	apiPolicy := NewNetworkPolicyBuilder("allow-api-ingress", namespace).
		ForPods(map[string]string{"app": "vaultaire-api"}).
		WithPolicyTypes(PolicyTypeIngress).
		AllowIngressFromNamespace(
			map[string]string{"kubernetes.io/metadata.name": "ingress-nginx"},
			TCP("8080"),
		).
		AllowIngressFromPods(
			map[string]string{"app": "vaultaire-worker"},
			TCP("8080"),
		).
		Build()
	policies = append(policies, apiPolicy)

	// Allow workers to communicate with API and external services
	workerPolicy := NewNetworkPolicyBuilder("allow-worker-egress", namespace).
		ForPods(map[string]string{"app": "vaultaire-worker"}).
		WithPolicyTypes(PolicyTypeEgress).
		AllowDNS().
		AllowEgressToPods(
			map[string]string{"app": "vaultaire-api"},
			TCP("8080"),
		).
		AllowEgressToCIDR("0.0.0.0/0", []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"},
			TCP("443"),
		).
		Build()
	policies = append(policies, workerPolicy)

	// Allow metrics scraping from monitoring namespace
	metricsPolicy := NewNetworkPolicyBuilder("allow-metrics-scrape", namespace).
		ForPods(map[string]string{}).
		WithPolicyTypes(PolicyTypeIngress).
		AllowIngressFromNamespace(
			map[string]string{"kubernetes.io/metadata.name": "monitoring"},
			TCP("9090"),
		).
		Build()
	policies = append(policies, metricsPolicy)

	// Allow internal pod-to-pod communication within namespace
	internalPolicy := NewNetworkPolicyBuilder("allow-internal", namespace).
		ForPods(map[string]string{}).
		WithPolicyTypes(PolicyTypeIngress).
		AllowIngressFromPods(map[string]string{}).
		Build()
	policies = append(policies, internalPolicy)

	return policies
}

// NetworkPolicySet represents a collection of NetworkPolicy resources
type NetworkPolicySet struct {
	Policies []*NetworkPolicyResource
}

// Add adds a NetworkPolicy to the set
func (s *NetworkPolicySet) Add(np *NetworkPolicyResource) {
	s.Policies = append(s.Policies, np)
}

// ToYAML converts all NetworkPolicies to multi-document YAML
func (s *NetworkPolicySet) ToYAML() (string, error) {
	var parts []string
	for _, np := range s.Policies {
		yaml, err := np.ToYAML()
		if err != nil {
			return "", err
		}
		parts = append(parts, yaml)
	}
	return strings.Join(parts, "---\n"), nil
}
