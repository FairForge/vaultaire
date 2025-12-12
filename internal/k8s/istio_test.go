// internal/k8s/istio_test.go
package k8s

import (
	"strings"
	"testing"
	"time"
)

func TestNewIstioManager(t *testing.T) {
	im := NewIstioManager("default")

	if im.namespace != "default" {
		t.Errorf("expected namespace 'default', got '%s'", im.namespace)
	}
	if im.labels["app.kubernetes.io/managed-by"] != "vaultaire" {
		t.Error("expected managed-by label")
	}
}

func TestGenerateVirtualService(t *testing.T) {
	im := NewIstioManager("default")

	vs := im.GenerateVirtualService(VirtualServiceConfig{
		Name:     "test-service",
		Hosts:    []string{"test.example.com"},
		Gateways: []string{"my-gateway"},
		HTTP: []HTTPRoute{
			{
				Name: "main",
				Match: []HTTPMatchRequest{
					{URI: &StringMatch{Prefix: "/api"}},
				},
				Route: []HTTPRouteDestination{
					{
						Destination: Destination{
							Host: "test-service",
							Port: &PortSelector{Number: 8080},
						},
						Weight: 100,
					},
				},
				Timeout: "30s",
			},
		},
	})

	if vs.Kind != KindVirtualService {
		t.Errorf("expected kind VirtualService, got %s", vs.Kind)
	}
	if vs.APIVersion != "networking.istio.io/v1beta1" {
		t.Errorf("expected apiVersion networking.istio.io/v1beta1, got %s", vs.APIVersion)
	}
	if vs.Metadata.Name != "test-service" {
		t.Errorf("expected name 'test-service', got '%s'", vs.Metadata.Name)
	}
	if vs.Metadata.Namespace != "default" {
		t.Errorf("expected namespace 'default', got '%s'", vs.Metadata.Namespace)
	}

	hosts, ok := vs.Spec["hosts"].([]string)
	if !ok || len(hosts) != 1 || hosts[0] != "test.example.com" {
		t.Error("expected hosts to be set correctly")
	}
}

func TestVirtualServiceWithRetries(t *testing.T) {
	im := NewIstioManager("default")

	vs := im.GenerateVirtualService(VirtualServiceConfig{
		Name:  "test-service",
		Hosts: []string{"test.example.com"},
		HTTP: []HTTPRoute{
			{
				Route: []HTTPRouteDestination{
					{Destination: Destination{Host: "test-service"}},
				},
				Retries: &HTTPRetry{
					Attempts:      3,
					PerTryTimeout: "2s",
					RetryOn:       "5xx",
				},
			},
		},
	})

	httpRoutes := vs.Spec["http"].([]map[string]interface{})
	if len(httpRoutes) != 1 {
		t.Fatal("expected 1 HTTP route")
	}

	retries := httpRoutes[0]["retries"].(*HTTPRetry)
	if retries.Attempts != 3 {
		t.Errorf("expected 3 retry attempts, got %d", retries.Attempts)
	}
}

func TestVirtualServiceWithFaultInjection(t *testing.T) {
	im := NewIstioManager("default")

	vs := im.GenerateVirtualService(VirtualServiceConfig{
		Name:  "test-service",
		Hosts: []string{"test.example.com"},
		HTTP: []HTTPRoute{
			{
				Route: []HTTPRouteDestination{
					{Destination: Destination{Host: "test-service"}},
				},
				Fault: &HTTPFaultInjection{
					Delay: &FaultDelay{
						FixedDelay: "5s",
						Percentage: &Percentage{Value: 10},
					},
					Abort: &FaultAbort{
						HTTPStatus: 503,
						Percentage: &Percentage{Value: 5},
					},
				},
			},
		},
	})

	httpRoutes := vs.Spec["http"].([]map[string]interface{})
	fault := httpRoutes[0]["fault"].(*HTTPFaultInjection)

	if fault.Delay.FixedDelay != "5s" {
		t.Error("expected delay to be 5s")
	}
	if fault.Abort.HTTPStatus != 503 {
		t.Error("expected abort status 503")
	}
}

func TestGenerateDestinationRule(t *testing.T) {
	im := NewIstioManager("default")

	dr := im.GenerateDestinationRule(DestinationRuleConfig{
		Name: "test-service",
		Host: "test-service.default.svc.cluster.local",
		TrafficPolicy: &TrafficPolicy{
			ConnectionPool: &ConnectionPoolSettings{
				TCP: &TCPSettings{
					MaxConnections: 100,
				},
				HTTP: &HTTPSettings{
					HTTP2MaxRequests: 1000,
				},
			},
			LoadBalancer: &LoadBalancerSettings{
				Simple: "ROUND_ROBIN",
			},
		},
		Subsets: []Subset{
			{
				Name:   "v1",
				Labels: map[string]string{"version": "v1"},
			},
			{
				Name:   "v2",
				Labels: map[string]string{"version": "v2"},
			},
		},
	})

	if dr.Kind != KindDestinationRule {
		t.Errorf("expected kind DestinationRule, got %s", dr.Kind)
	}

	host := dr.Spec["host"].(string)
	if host != "test-service.default.svc.cluster.local" {
		t.Errorf("expected host to be set, got '%s'", host)
	}

	subsets := dr.Spec["subsets"].([]Subset)
	if len(subsets) != 2 {
		t.Errorf("expected 2 subsets, got %d", len(subsets))
	}
}

func TestDestinationRuleWithOutlierDetection(t *testing.T) {
	im := NewIstioManager("default")

	dr := im.GenerateDestinationRule(DestinationRuleConfig{
		Name: "test-service",
		Host: "test-service",
		TrafficPolicy: &TrafficPolicy{
			OutlierDetection: &OutlierDetection{
				Consecutive5xxErrors: 5,
				Interval:             "30s",
				BaseEjectionTime:     "30s",
				MaxEjectionPercent:   50,
			},
		},
	})

	policy := dr.Spec["trafficPolicy"].(*TrafficPolicy)
	if policy.OutlierDetection.Consecutive5xxErrors != 5 {
		t.Error("expected consecutive5xxErrors to be 5")
	}
}

func TestGenerateGateway(t *testing.T) {
	im := NewIstioManager("default")

	gw := im.GenerateGateway(GatewayConfig{
		Name: "test-gateway",
		Servers: []GatewayServer{
			{
				Port: &GatewayPort{
					Number:   443,
					Name:     "https",
					Protocol: "HTTPS",
				},
				Hosts: []string{"*.example.com"},
				TLS: &GatewayTLS{
					Mode:           "SIMPLE",
					CredentialName: "my-cert",
				},
			},
			{
				Port: &GatewayPort{
					Number:   80,
					Name:     "http",
					Protocol: "HTTP",
				},
				Hosts: []string{"*.example.com"},
			},
		},
	})

	if gw.Kind != KindGateway {
		t.Errorf("expected kind Gateway, got %s", gw.Kind)
	}

	servers := gw.Spec["servers"].([]GatewayServer)
	if len(servers) != 2 {
		t.Errorf("expected 2 servers, got %d", len(servers))
	}

	if servers[0].TLS.CredentialName != "my-cert" {
		t.Error("expected TLS credential name")
	}
}

func TestGatewayDefaultSelector(t *testing.T) {
	im := NewIstioManager("default")

	gw := im.GenerateGateway(GatewayConfig{
		Name: "test-gateway",
		Servers: []GatewayServer{
			{
				Port:  &GatewayPort{Number: 80, Name: "http", Protocol: "HTTP"},
				Hosts: []string{"example.com"},
			},
		},
	})

	selector := gw.Spec["selector"].(map[string]string)
	if selector["istio"] != "ingressgateway" {
		t.Error("expected default selector istio=ingressgateway")
	}
}

func TestGenerateAuthorizationPolicy(t *testing.T) {
	im := NewIstioManager("default")

	authz := im.GenerateAuthorizationPolicy(AuthorizationPolicyConfig{
		Name: "test-authz",
		Selector: &WorkloadSelector{
			MatchLabels: map[string]string{"app": "test"},
		},
		Action: "ALLOW",
		Rules: []AuthorizationRule{
			{
				From: []RuleFrom{
					{
						Source: &Source{
							Principals: []string{"cluster.local/ns/default/sa/test"},
						},
					},
				},
				To: []RuleTo{
					{
						Operation: &Operation{
							Methods: []string{"GET", "POST"},
							Paths:   []string{"/api/*"},
						},
					},
				},
			},
		},
	})

	if authz.Kind != KindAuthorizationPolicy {
		t.Errorf("expected kind AuthorizationPolicy, got %s", authz.Kind)
	}
	if authz.APIVersion != "security.istio.io/v1beta1" {
		t.Errorf("expected security.istio.io/v1beta1, got %s", authz.APIVersion)
	}

	action := authz.Spec["action"].(string)
	if action != "ALLOW" {
		t.Errorf("expected action ALLOW, got %s", action)
	}
}

func TestGeneratePeerAuthentication(t *testing.T) {
	im := NewIstioManager("default")

	pa := im.GeneratePeerAuthentication(PeerAuthenticationConfig{
		Name: "test-mtls",
		Selector: &WorkloadSelector{
			MatchLabels: map[string]string{"app": "test"},
		},
		MTLS: &PeerAuthenticationMTLS{
			Mode: "STRICT",
		},
	})

	if pa.Kind != KindPeerAuthentication {
		t.Errorf("expected kind PeerAuthentication, got %s", pa.Kind)
	}

	mtls := pa.Spec["mtls"].(*PeerAuthenticationMTLS)
	if mtls.Mode != "STRICT" {
		t.Errorf("expected mTLS mode STRICT, got %s", mtls.Mode)
	}
}

func TestGenerateRequestAuthentication(t *testing.T) {
	im := NewIstioManager("default")

	ra := im.GenerateRequestAuthentication(RequestAuthenticationConfig{
		Name: "test-jwt",
		Selector: &WorkloadSelector{
			MatchLabels: map[string]string{"app": "test"},
		},
		JWTRules: []JWTRule{
			{
				Issuer:    "https://auth.example.com",
				Audiences: []string{"api.example.com"},
				JwksURI:   "https://auth.example.com/.well-known/jwks.json",
				FromHeaders: []JWTHeader{
					{Name: "Authorization", Prefix: "Bearer "},
				},
			},
		},
	})

	if ra.Kind != KindRequestAuthentication {
		t.Errorf("expected kind RequestAuthentication, got %s", ra.Kind)
	}

	rules := ra.Spec["jwtRules"].([]JWTRule)
	if len(rules) != 1 {
		t.Fatal("expected 1 JWT rule")
	}
	if rules[0].Issuer != "https://auth.example.com" {
		t.Error("expected issuer to be set")
	}
}

func TestGenerateServiceEntry(t *testing.T) {
	im := NewIstioManager("default")

	se := im.GenerateServiceEntry(ServiceEntryConfig{
		Name:       "external-api",
		Hosts:      []string{"api.external.com"},
		Addresses:  []string{"192.168.1.1"},
		Location:   "MESH_EXTERNAL",
		Resolution: "DNS",
		Ports: []IstioServicePort{
			{
				Number:   443,
				Protocol: "HTTPS",
				Name:     "https",
			},
		},
		Endpoints: []WorkloadEntry{
			{
				Address: "api.external.com",
				Ports:   map[string]int{"https": 443},
			},
		},
	})

	if se.Kind != KindServiceEntry {
		t.Errorf("expected kind ServiceEntry, got %s", se.Kind)
	}

	hosts := se.Spec["hosts"].([]string)
	if len(hosts) != 1 || hosts[0] != "api.external.com" {
		t.Error("expected hosts to be set")
	}

	location := se.Spec["location"].(string)
	if location != "MESH_EXTERNAL" {
		t.Errorf("expected location MESH_EXTERNAL, got %s", location)
	}
}

func TestGenerateSidecar(t *testing.T) {
	im := NewIstioManager("default")

	sc := im.GenerateSidecar(SidecarConfig{
		Name: "test-sidecar",
		WorkloadSelector: &WorkloadSelector{
			MatchLabels: map[string]string{"app": "test"},
		},
		Egress: []IstioEgressListener{
			{
				Hosts: []string{"./*", "istio-system/*"},
			},
		},
		OutboundTrafficPolicy: &OutboundTrafficPolicy{
			Mode: "REGISTRY_ONLY",
		},
	})

	if sc.Kind != KindSidecar {
		t.Errorf("expected kind Sidecar, got %s", sc.Kind)
	}

	egress := sc.Spec["egress"].([]IstioEgressListener)
	if len(egress) != 1 {
		t.Fatal("expected 1 egress listener")
	}

	policy := sc.Spec["outboundTrafficPolicy"].(*OutboundTrafficPolicy)
	if policy.Mode != "REGISTRY_ONLY" {
		t.Error("expected outbound mode REGISTRY_ONLY")
	}
}

func TestIstioResourceToYAML(t *testing.T) {
	im := NewIstioManager("default")

	vs := im.GenerateVirtualService(VirtualServiceConfig{
		Name:  "test",
		Hosts: []string{"test.example.com"},
		HTTP: []HTTPRoute{
			{
				Route: []HTTPRouteDestination{
					{Destination: Destination{Host: "test"}},
				},
			},
		},
	})

	yaml, err := vs.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "kind: VirtualService") {
		t.Error("expected YAML to contain kind")
	}
	if !strings.Contains(yaml, "networking.istio.io/v1beta1") {
		t.Error("expected YAML to contain apiVersion")
	}
}

func TestIstioResourceSetToYAML(t *testing.T) {
	im := NewIstioManager("default")

	set := &IstioResourceSet{}
	set.Add(im.GenerateVirtualService(VirtualServiceConfig{
		Name:  "vs1",
		Hosts: []string{"host1"},
	}))
	set.Add(im.GenerateDestinationRule(DestinationRuleConfig{
		Name: "dr1",
		Host: "host1",
	}))

	yaml, err := set.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	docs := strings.Split(yaml, "---")
	if len(docs) != 2 {
		t.Errorf("expected 2 YAML documents, got %d", len(docs))
	}
}

func TestGenerateVaultaireIstioConfig(t *testing.T) {
	set, err := GenerateVaultaireIstioConfig("vaultaire", "api.vaultaire.io")
	if err != nil {
		t.Fatalf("failed to generate Istio config: %v", err)
	}

	if len(set.Resources) < 4 {
		t.Errorf("expected at least 4 resources, got %d", len(set.Resources))
	}

	// Check for expected resource types
	kinds := make(map[IstioResourceKind]int)
	for _, r := range set.Resources {
		kinds[r.Kind]++
	}

	if kinds[KindGateway] != 1 {
		t.Error("expected 1 Gateway")
	}
	if kinds[KindVirtualService] != 1 {
		t.Error("expected 1 VirtualService")
	}
	if kinds[KindDestinationRule] != 1 {
		t.Error("expected 1 DestinationRule")
	}
	if kinds[KindPeerAuthentication] != 1 {
		t.Error("expected 1 PeerAuthentication")
	}
}

func TestGenerateCanaryVirtualService(t *testing.T) {
	im := NewIstioManager("default")

	vs := im.GenerateCanaryVirtualService(CanaryDeploymentConfig{
		ServiceName:   "myapp",
		StableVersion: "v1",
		CanaryVersion: "v2",
		CanaryWeight:  10,
		Headers: map[string]string{
			"x-canary": "true",
		},
	})

	if vs.Kind != KindVirtualService {
		t.Errorf("expected kind VirtualService, got %s", vs.Kind)
	}

	httpRoutes := vs.Spec["http"].([]map[string]interface{})

	// Should have 2 routes: header-based and weight-based
	if len(httpRoutes) != 2 {
		t.Errorf("expected 2 HTTP routes, got %d", len(httpRoutes))
	}
}

func TestGenerateCanaryWithoutHeaders(t *testing.T) {
	im := NewIstioManager("default")

	vs := im.GenerateCanaryVirtualService(CanaryDeploymentConfig{
		ServiceName:   "myapp",
		StableVersion: "v1",
		CanaryVersion: "v2",
		CanaryWeight:  20,
	})

	httpRoutes := vs.Spec["http"].([]map[string]interface{})

	// Should have only 1 route (weight-based) when no headers specified
	if len(httpRoutes) != 1 {
		t.Errorf("expected 1 HTTP route, got %d", len(httpRoutes))
	}
}

func TestGenerateCircuitBreaker(t *testing.T) {
	im := NewIstioManager("default")

	dr := im.GenerateCircuitBreaker(CircuitBreakerConfig{
		ServiceName:          "myapp",
		MaxConnections:       50,
		HTTP2MaxRequests:     500,
		Consecutive5xxErrors: 3,
		EjectionInterval:     20 * time.Second,
		BaseEjectionTime:     60 * time.Second,
		MaxEjectionPercent:   30,
	})

	if dr.Kind != KindDestinationRule {
		t.Errorf("expected kind DestinationRule, got %s", dr.Kind)
	}

	if !strings.Contains(dr.Metadata.Name, "circuit-breaker") {
		t.Error("expected name to contain 'circuit-breaker'")
	}

	policy := dr.Spec["trafficPolicy"].(*TrafficPolicy)

	if policy.ConnectionPool.TCP.MaxConnections != 50 {
		t.Errorf("expected maxConnections 50, got %d", policy.ConnectionPool.TCP.MaxConnections)
	}

	if policy.OutlierDetection.Consecutive5xxErrors != 3 {
		t.Errorf("expected consecutive5xxErrors 3, got %d", policy.OutlierDetection.Consecutive5xxErrors)
	}
}

func TestCircuitBreakerDefaults(t *testing.T) {
	im := NewIstioManager("default")

	dr := im.GenerateCircuitBreaker(CircuitBreakerConfig{
		ServiceName: "myapp",
	})

	policy := dr.Spec["trafficPolicy"].(*TrafficPolicy)

	// Check defaults are applied
	if policy.ConnectionPool.TCP.MaxConnections != 100 {
		t.Errorf("expected default maxConnections 100, got %d", policy.ConnectionPool.TCP.MaxConnections)
	}
	if policy.OutlierDetection.Consecutive5xxErrors != 5 {
		t.Errorf("expected default consecutive5xxErrors 5, got %d", policy.OutlierDetection.Consecutive5xxErrors)
	}
	if policy.OutlierDetection.MaxEjectionPercent != 50 {
		t.Errorf("expected default maxEjectionPercent 50, got %d", policy.OutlierDetection.MaxEjectionPercent)
	}
}

func TestVirtualServiceNamespaceDefault(t *testing.T) {
	im := NewIstioManager("production")

	vs := im.GenerateVirtualService(VirtualServiceConfig{
		Name:  "test",
		Hosts: []string{"test"},
	})

	if vs.Metadata.Namespace != "production" {
		t.Errorf("expected namespace 'production', got '%s'", vs.Metadata.Namespace)
	}
}

func TestVirtualServiceNamespaceOverride(t *testing.T) {
	im := NewIstioManager("production")

	vs := im.GenerateVirtualService(VirtualServiceConfig{
		Name:      "test",
		Namespace: "staging",
		Hosts:     []string{"test"},
	})

	if vs.Metadata.Namespace != "staging" {
		t.Errorf("expected namespace 'staging', got '%s'", vs.Metadata.Namespace)
	}
}

func TestLoadBalancerConsistentHash(t *testing.T) {
	im := NewIstioManager("default")

	dr := im.GenerateDestinationRule(DestinationRuleConfig{
		Name: "test",
		Host: "test",
		TrafficPolicy: &TrafficPolicy{
			LoadBalancer: &LoadBalancerSettings{
				ConsistentHash: &ConsistentHashLB{
					HTTPHeaderName: "x-user-id",
				},
			},
		},
	})

	policy := dr.Spec["trafficPolicy"].(*TrafficPolicy)
	if policy.LoadBalancer.ConsistentHash.HTTPHeaderName != "x-user-id" {
		t.Error("expected consistent hash by header")
	}
}

func TestLoadBalancerCookieHash(t *testing.T) {
	im := NewIstioManager("default")

	dr := im.GenerateDestinationRule(DestinationRuleConfig{
		Name: "test",
		Host: "test",
		TrafficPolicy: &TrafficPolicy{
			LoadBalancer: &LoadBalancerSettings{
				ConsistentHash: &ConsistentHashLB{
					HTTPCookie: &HTTPCookie{
						Name: "session",
						TTL:  "3600s",
					},
				},
			},
		},
	})

	policy := dr.Spec["trafficPolicy"].(*TrafficPolicy)
	if policy.LoadBalancer.ConsistentHash.HTTPCookie.Name != "session" {
		t.Error("expected consistent hash by cookie")
	}
}

func TestTLSSettings(t *testing.T) {
	im := NewIstioManager("default")

	dr := im.GenerateDestinationRule(DestinationRuleConfig{
		Name: "test",
		Host: "external-service",
		TrafficPolicy: &TrafficPolicy{
			TLS: &TLSSettings{
				Mode:              "MUTUAL",
				ClientCertificate: "/etc/certs/cert.pem",
				PrivateKey:        "/etc/certs/key.pem",
				CACertificates:    "/etc/certs/ca.pem",
			},
		},
	})

	policy := dr.Spec["trafficPolicy"].(*TrafficPolicy)
	if policy.TLS.Mode != "MUTUAL" {
		t.Errorf("expected TLS mode MUTUAL, got %s", policy.TLS.Mode)
	}
}

func TestCorsPolicy(t *testing.T) {
	im := NewIstioManager("default")

	vs := im.GenerateVirtualService(VirtualServiceConfig{
		Name:  "test",
		Hosts: []string{"test"},
		HTTP: []HTTPRoute{
			{
				Route: []HTTPRouteDestination{
					{Destination: Destination{Host: "test"}},
				},
				CorsPolicy: &CorsPolicy{
					AllowOrigins: []StringMatch{
						{Exact: "https://example.com"},
					},
					AllowMethods:     []string{"GET", "POST", "OPTIONS"},
					AllowHeaders:     []string{"Authorization", "Content-Type"},
					AllowCredentials: true,
					MaxAge:           "24h",
				},
			},
		},
	})

	httpRoutes := vs.Spec["http"].([]map[string]interface{})
	cors := httpRoutes[0]["corsPolicy"].(*CorsPolicy)

	if len(cors.AllowMethods) != 3 {
		t.Error("expected 3 allowed methods")
	}
	if !cors.AllowCredentials {
		t.Error("expected allowCredentials to be true")
	}
}

func TestHeaderManipulation(t *testing.T) {
	im := NewIstioManager("default")

	vs := im.GenerateVirtualService(VirtualServiceConfig{
		Name:  "test",
		Hosts: []string{"test"},
		HTTP: []HTTPRoute{
			{
				Route: []HTTPRouteDestination{
					{
						Destination: Destination{Host: "test"},
						Headers: &HeadersConfig{
							Request: &HeaderOperations{
								Set:    map[string]string{"x-custom": "value"},
								Remove: []string{"x-internal"},
							},
							Response: &HeaderOperations{
								Add: map[string]string{"x-served-by": "vaultaire"},
							},
						},
					},
				},
			},
		},
	})

	httpRoutes := vs.Spec["http"].([]map[string]interface{})
	if httpRoutes[0]["route"] == nil {
		t.Fatal("expected route to be set")
	}
}

func TestTCPRoute(t *testing.T) {
	im := NewIstioManager("default")

	vs := im.GenerateVirtualService(VirtualServiceConfig{
		Name:  "tcp-service",
		Hosts: []string{"tcp.example.com"},
		TCP: []TCPRoute{
			{
				Match: []TCPMatchRequest{
					{Port: 3306},
				},
				Route: []TCPRouteDestination{
					{
						Destination: Destination{
							Host: "mysql",
							Port: &PortSelector{Number: 3306},
						},
					},
				},
			},
		},
	})

	tcpRoutes := vs.Spec["tcp"].([]TCPRoute)
	if len(tcpRoutes) != 1 {
		t.Error("expected 1 TCP route")
	}
}

func TestTLSRoute(t *testing.T) {
	im := NewIstioManager("default")

	vs := im.GenerateVirtualService(VirtualServiceConfig{
		Name:  "tls-service",
		Hosts: []string{"*.example.com"},
		TLS: []TLSRoute{
			{
				Match: []TLSMatchAttributes{
					{SNIHosts: []string{"secure.example.com"}},
				},
				Route: []TCPRouteDestination{
					{
						Destination: Destination{Host: "secure-backend"},
					},
				},
			},
		},
	})

	tlsRoutes := vs.Spec["tls"].([]TLSRoute)
	if len(tlsRoutes) != 1 {
		t.Error("expected 1 TLS route")
	}
}
