// internal/k8s/netpol_test.go
package k8s

import (
	"strings"
	"testing"
)

func TestNewNetworkPolicyManager(t *testing.T) {
	npm := NewNetworkPolicyManager("default")

	if npm.namespace != "default" {
		t.Errorf("expected namespace 'default', got '%s'", npm.namespace)
	}
}

func TestGenerateNetworkPolicy(t *testing.T) {
	npm := NewNetworkPolicyManager("default")

	np := npm.GenerateNetworkPolicy(NetworkPolicyConfig{
		Name:        "test-policy",
		PodSelector: map[string]string{"app": "test"},
		PolicyTypes: []PolicyType{PolicyTypeIngress},
		Ingress: []NetworkPolicyIngressRule{
			{
				From: []NetworkPolicyPeer{
					{PodSelector: map[string]string{"role": "frontend"}},
				},
				Ports: []NetworkPolicyPort{
					{Protocol: "TCP", Port: "8080"},
				},
			},
		},
	})

	if np.Kind != "NetworkPolicy" {
		t.Errorf("expected kind NetworkPolicy, got %s", np.Kind)
	}
	if np.APIVersion != "networking.k8s.io/v1" {
		t.Errorf("expected apiVersion networking.k8s.io/v1, got %s", np.APIVersion)
	}
	if np.Spec.PodSelector.MatchLabels["app"] != "test" {
		t.Error("expected pod selector app=test")
	}
}

func TestGenerateNetworkPolicyWithEgress(t *testing.T) {
	npm := NewNetworkPolicyManager("default")

	np := npm.GenerateNetworkPolicy(NetworkPolicyConfig{
		Name:        "test-egress",
		PodSelector: map[string]string{"app": "test"},
		PolicyTypes: []PolicyType{PolicyTypeEgress},
		Egress: []NetworkPolicyEgressRule{
			{
				To: []NetworkPolicyPeer{
					{IPBlock: &IPBlock{CIDR: "10.0.0.0/8"}},
				},
				Ports: []NetworkPolicyPort{
					{Protocol: "TCP", Port: "443"},
				},
			},
		},
	})

	if len(np.Spec.Egress) != 1 {
		t.Fatalf("expected 1 egress rule, got %d", len(np.Spec.Egress))
	}
	if np.Spec.Egress[0].To[0].IPBlock.CIDR != "10.0.0.0/8" {
		t.Error("expected CIDR 10.0.0.0/8")
	}
}

func TestNetworkPolicyWithIPBlockExcept(t *testing.T) {
	npm := NewNetworkPolicyManager("default")

	np := npm.GenerateNetworkPolicy(NetworkPolicyConfig{
		Name:        "test-ipblock",
		PodSelector: map[string]string{},
		PolicyTypes: []PolicyType{PolicyTypeEgress},
		Egress: []NetworkPolicyEgressRule{
			{
				To: []NetworkPolicyPeer{
					{IPBlock: &IPBlock{
						CIDR:   "0.0.0.0/0",
						Except: []string{"10.0.0.0/8", "192.168.0.0/16"},
					}},
				},
			},
		},
	})

	if len(np.Spec.Egress[0].To[0].IPBlock.Except) != 2 {
		t.Errorf("expected 2 except blocks, got %d", len(np.Spec.Egress[0].To[0].IPBlock.Except))
	}
}

func TestNetworkPolicyToYAML(t *testing.T) {
	npm := NewNetworkPolicyManager("default")

	np := npm.GenerateNetworkPolicy(NetworkPolicyConfig{
		Name:        "test-policy",
		PodSelector: map[string]string{"app": "test"},
		PolicyTypes: []PolicyType{PolicyTypeIngress},
	})

	yaml, err := np.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "kind: NetworkPolicy") {
		t.Error("expected YAML to contain kind")
	}
	if !strings.Contains(yaml, "networking.k8s.io/v1") {
		t.Error("expected YAML to contain apiVersion")
	}
}

func TestNetworkPolicyBuilder(t *testing.T) {
	np := NewNetworkPolicyBuilder("test", "default").
		ForPods(map[string]string{"app": "web"}).
		WithPolicyTypes(PolicyTypeIngress, PolicyTypeEgress).
		AllowIngressFromPods(map[string]string{"role": "frontend"}, TCP("8080")).
		AllowEgressToPods(map[string]string{"role": "backend"}, TCP("5432")).
		Build()

	if np.Metadata.Name != "test" {
		t.Errorf("expected name 'test', got '%s'", np.Metadata.Name)
	}
	if len(np.Spec.PolicyTypes) != 2 {
		t.Error("expected 2 policy types")
	}
	if len(np.Spec.Ingress) != 1 {
		t.Error("expected 1 ingress rule")
	}
	if len(np.Spec.Egress) != 1 {
		t.Error("expected 1 egress rule")
	}
}

func TestNetworkPolicyBuilderAllowDNS(t *testing.T) {
	np := NewNetworkPolicyBuilder("test", "default").
		ForPods(map[string]string{"app": "web"}).
		WithPolicyTypes(PolicyTypeEgress).
		AllowDNS().
		Build()

	if len(np.Spec.Egress) != 1 {
		t.Fatalf("expected 1 egress rule, got %d", len(np.Spec.Egress))
	}
	if len(np.Spec.Egress[0].Ports) != 2 {
		t.Error("expected 2 DNS ports (UDP and TCP)")
	}
}

func TestNetworkPolicyBuilderFromNamespace(t *testing.T) {
	np := NewNetworkPolicyBuilder("test", "default").
		ForPods(map[string]string{"app": "web"}).
		WithPolicyTypes(PolicyTypeIngress).
		AllowIngressFromNamespace(map[string]string{"name": "frontend"}, TCP("8080")).
		Build()

	if np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["name"] != "frontend" {
		t.Error("expected namespace selector")
	}
}

func TestNetworkPolicyBuilderFromCIDR(t *testing.T) {
	np := NewNetworkPolicyBuilder("test", "default").
		ForPods(map[string]string{"app": "web"}).
		WithPolicyTypes(PolicyTypeIngress).
		AllowIngressFromCIDR("10.0.0.0/8", nil, TCP("8080")).
		Build()

	if np.Spec.Ingress[0].From[0].IPBlock.CIDR != "10.0.0.0/8" {
		t.Error("expected CIDR block")
	}
}

func TestNetworkPolicyBuilderAllowAll(t *testing.T) {
	np := NewNetworkPolicyBuilder("test", "default").
		ForPods(map[string]string{"app": "web"}).
		WithPolicyTypes(PolicyTypeIngress, PolicyTypeEgress).
		AllowAllIngress().
		AllowAllEgress().
		Build()

	if len(np.Spec.Ingress) != 1 {
		t.Error("expected 1 ingress rule (allow all)")
	}
	if len(np.Spec.Egress) != 1 {
		t.Error("expected 1 egress rule (allow all)")
	}
}

func TestNetworkPolicyBuilderDenyAll(t *testing.T) {
	np := NewNetworkPolicyBuilder("test", "default").
		ForPods(map[string]string{}).
		DenyAllIngress().
		DenyAllEgress().
		Build()

	if len(np.Spec.PolicyTypes) != 2 {
		t.Error("expected 2 policy types")
	}
	if len(np.Spec.Ingress) != 0 {
		t.Error("expected no ingress rules (deny all)")
	}
	if len(np.Spec.Egress) != 0 {
		t.Error("expected no egress rules (deny all)")
	}
}

func TestTCPPort(t *testing.T) {
	port := TCP("8080")

	if port.Protocol != "TCP" {
		t.Errorf("expected TCP protocol, got %s", port.Protocol)
	}
	if port.Port != "8080" {
		t.Errorf("expected port 8080, got %s", port.Port)
	}
}

func TestUDPPort(t *testing.T) {
	port := UDP("53")

	if port.Protocol != "UDP" {
		t.Errorf("expected UDP protocol, got %s", port.Protocol)
	}
}

func TestPortRange(t *testing.T) {
	port := PortRange("TCP", "8000", 9000)

	if port.Protocol != "TCP" {
		t.Error("expected TCP protocol")
	}
	if port.Port != "8000" {
		t.Error("expected start port 8000")
	}
	if port.EndPort != 9000 {
		t.Error("expected end port 9000")
	}
}

func TestGenerateDefaultDenyIngress(t *testing.T) {
	np := GenerateDefaultDenyIngress("default")

	if np.Metadata.Name != "default-deny-ingress" {
		t.Errorf("expected name 'default-deny-ingress', got '%s'", np.Metadata.Name)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != "Ingress" {
		t.Error("expected Ingress policy type")
	}
	if len(np.Spec.Ingress) != 0 {
		t.Error("expected no ingress rules")
	}
}

func TestGenerateDefaultDenyEgress(t *testing.T) {
	np := GenerateDefaultDenyEgress("default")

	if np.Metadata.Name != "default-deny-egress" {
		t.Errorf("expected name 'default-deny-egress', got '%s'", np.Metadata.Name)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != "Egress" {
		t.Error("expected Egress policy type")
	}
}

func TestGenerateDefaultDenyAll(t *testing.T) {
	np := GenerateDefaultDenyAll("default")

	if np.Metadata.Name != "default-deny-all" {
		t.Errorf("expected name 'default-deny-all', got '%s'", np.Metadata.Name)
	}
	if len(np.Spec.PolicyTypes) != 2 {
		t.Error("expected 2 policy types")
	}
}

func TestGenerateVaultaireNetworkPolicies(t *testing.T) {
	policies := GenerateVaultaireNetworkPolicies("vaultaire")

	if len(policies) < 4 {
		t.Errorf("expected at least 4 policies, got %d", len(policies))
	}

	names := make(map[string]bool)
	for _, p := range policies {
		names[p.Metadata.Name] = true
	}

	expectedPolicies := []string{
		"default-deny-ingress",
		"allow-api-ingress",
		"allow-worker-egress",
		"allow-metrics-scrape",
	}

	for _, name := range expectedPolicies {
		if !names[name] {
			t.Errorf("expected policy '%s'", name)
		}
	}
}

func TestNetworkPolicySet(t *testing.T) {
	set := &NetworkPolicySet{}
	set.Add(GenerateDefaultDenyIngress("default"))
	set.Add(GenerateDefaultDenyEgress("default"))

	yaml, err := set.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	docs := strings.Split(yaml, "---")
	if len(docs) != 2 {
		t.Errorf("expected 2 YAML documents, got %d", len(docs))
	}
}

func TestNetworkPolicyNamespaceDefault(t *testing.T) {
	npm := NewNetworkPolicyManager("production")

	np := npm.GenerateNetworkPolicy(NetworkPolicyConfig{
		Name:        "test-policy",
		PodSelector: map[string]string{},
	})

	if np.Metadata.Namespace != "production" {
		t.Errorf("expected namespace 'production', got '%s'", np.Metadata.Namespace)
	}
}

func TestNetworkPolicyNamespaceOverride(t *testing.T) {
	npm := NewNetworkPolicyManager("production")

	np := npm.GenerateNetworkPolicy(NetworkPolicyConfig{
		Name:        "test-policy",
		Namespace:   "staging",
		PodSelector: map[string]string{},
	})

	if np.Metadata.Namespace != "staging" {
		t.Errorf("expected namespace 'staging', got '%s'", np.Metadata.Namespace)
	}
}

func TestNetworkPolicyWithNamedPort(t *testing.T) {
	npm := NewNetworkPolicyManager("default")

	np := npm.GenerateNetworkPolicy(NetworkPolicyConfig{
		Name:        "test-policy",
		PodSelector: map[string]string{},
		PolicyTypes: []PolicyType{PolicyTypeIngress},
		Ingress: []NetworkPolicyIngressRule{
			{
				Ports: []NetworkPolicyPort{
					{Protocol: "TCP", Port: "http"},
				},
			},
		},
	})

	// Named port should be preserved as string
	if np.Spec.Ingress[0].Ports[0].Port != "http" {
		t.Errorf("expected named port 'http', got %v", np.Spec.Ingress[0].Ports[0].Port)
	}
}

func TestNetworkPolicyWithPortRange(t *testing.T) {
	npm := NewNetworkPolicyManager("default")

	np := npm.GenerateNetworkPolicy(NetworkPolicyConfig{
		Name:        "test-policy",
		PodSelector: map[string]string{},
		PolicyTypes: []PolicyType{PolicyTypeIngress},
		Ingress: []NetworkPolicyIngressRule{
			{
				Ports: []NetworkPolicyPort{
					{Protocol: "TCP", Port: "8000", EndPort: 9000},
				},
			},
		},
	})

	if np.Spec.Ingress[0].Ports[0].EndPort == nil {
		t.Fatal("expected end port")
	}
	if *np.Spec.Ingress[0].Ports[0].EndPort != 9000 {
		t.Errorf("expected end port 9000, got %d", *np.Spec.Ingress[0].Ports[0].EndPort)
	}
}

func TestNetworkPolicyBuilderWithLabels(t *testing.T) {
	np := NewNetworkPolicyBuilder("test", "default").
		ForPods(map[string]string{"app": "web"}).
		WithLabel("env", "prod").
		WithAnnotation("description", "test policy").
		Build()

	if np.Metadata.Labels["env"] != "prod" {
		t.Error("expected env label")
	}
	if np.Metadata.Annotations["description"] != "test policy" {
		t.Error("expected description annotation")
	}
}

func TestConvertPeerPodSelector(t *testing.T) {
	peer := NetworkPolicyPeer{
		PodSelector: map[string]string{"app": "frontend"},
	}

	spec := convertPeer(peer)

	if spec.PodSelector == nil {
		t.Fatal("expected pod selector")
	}
	if spec.PodSelector.MatchLabels["app"] != "frontend" {
		t.Error("expected app=frontend")
	}
}

func TestConvertPeerNamespaceSelector(t *testing.T) {
	peer := NetworkPolicyPeer{
		NamespaceSelector: map[string]string{"name": "monitoring"},
	}

	spec := convertPeer(peer)

	if spec.NamespaceSelector == nil {
		t.Fatal("expected namespace selector")
	}
	if spec.NamespaceSelector.MatchLabels["name"] != "monitoring" {
		t.Error("expected name=monitoring")
	}
}

func TestConvertPortNumeric(t *testing.T) {
	port := NetworkPolicyPort{Protocol: "TCP", Port: "8080"}
	spec := convertPort(port)

	// Numeric port should be converted to int
	if spec.Port != 8080 {
		t.Errorf("expected port 8080, got %v", spec.Port)
	}
}
