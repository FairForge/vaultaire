// internal/k8s/ingress_test.go
package k8s

import (
	"strings"
	"testing"
)

func TestNewIngressManager(t *testing.T) {
	im := NewIngressManager("default")

	if im.namespace != "default" {
		t.Errorf("expected namespace 'default', got '%s'", im.namespace)
	}
	if im.labels["app.kubernetes.io/managed-by"] != "vaultaire" {
		t.Error("expected managed-by label")
	}
}

func TestGenerateIngress(t *testing.T) {
	im := NewIngressManager("default")

	ingress := im.GenerateIngress(K8sIngressConfig{
		Name:             "test-ingress",
		IngressClassName: "nginx",
		TLS: []K8sIngressTLS{
			{
				Hosts:      []string{"example.com"},
				SecretName: "example-tls",
			},
		},
		Rules: []IngressRule{
			{
				Host: "example.com",
				HTTP: &HTTPIngressRuleValue{
					Paths: []HTTPIngressPath{
						{
							Path:     "/",
							PathType: "Prefix",
							Backend: &IngressBackend{
								Service: &IngressServiceBackend{
									Name: "example-service",
									Port: K8sIngressSvcPort{Number: 80},
								},
							},
						},
					},
				},
			},
		},
	})

	if ingress.Kind != "Ingress" {
		t.Errorf("expected kind Ingress, got %s", ingress.Kind)
	}
	if ingress.APIVersion != "networking.k8s.io/v1" {
		t.Errorf("expected apiVersion networking.k8s.io/v1, got %s", ingress.APIVersion)
	}
	if ingress.Metadata.Name != "test-ingress" {
		t.Errorf("expected name 'test-ingress', got '%s'", ingress.Metadata.Name)
	}
	if ingress.Spec.IngressClassName != "nginx" {
		t.Errorf("expected ingressClassName 'nginx', got '%s'", ingress.Spec.IngressClassName)
	}
}

func TestIngressDefaultNamespace(t *testing.T) {
	im := NewIngressManager("production")

	ingress := im.GenerateIngress(K8sIngressConfig{
		Name: "test-ingress",
	})

	if ingress.Metadata.Namespace != "production" {
		t.Errorf("expected namespace 'production', got '%s'", ingress.Metadata.Namespace)
	}
}

func TestIngressNamespaceOverride(t *testing.T) {
	im := NewIngressManager("production")

	ingress := im.GenerateIngress(K8sIngressConfig{
		Name:      "test-ingress",
		Namespace: "staging",
	})

	if ingress.Metadata.Namespace != "staging" {
		t.Errorf("expected namespace 'staging', got '%s'", ingress.Metadata.Namespace)
	}
}

func TestIngressToYAML(t *testing.T) {
	im := NewIngressManager("default")

	ingress := im.GenerateIngress(K8sIngressConfig{
		Name:             "test-ingress",
		IngressClassName: "nginx",
		Rules: []IngressRule{
			{
				Host: "example.com",
				HTTP: &HTTPIngressRuleValue{
					Paths: []HTTPIngressPath{
						PathPrefix("/", "example-service", 80),
					},
				},
			},
		},
	})

	yaml, err := ingress.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "kind: Ingress") {
		t.Error("expected YAML to contain kind")
	}
	if !strings.Contains(yaml, "networking.k8s.io/v1") {
		t.Error("expected YAML to contain apiVersion")
	}
}

func TestNginxIngressConfigBasic(t *testing.T) {
	config := &NginxIngressConfig{
		ProxyBodySize:       "100m",
		ProxyConnectTimeout: 60,
		ProxySendTimeout:    60,
		ProxyReadTimeout:    60,
	}

	annotations := config.ToAnnotations()

	if annotations["nginx.ingress.kubernetes.io/proxy-body-size"] != "100m" {
		t.Error("expected proxy-body-size annotation")
	}
	if annotations["nginx.ingress.kubernetes.io/proxy-connect-timeout"] != "60" {
		t.Error("expected proxy-connect-timeout annotation")
	}
}

func TestNginxIngressConfigSSL(t *testing.T) {
	config := &NginxIngressConfig{
		SSLRedirect:      true,
		ForceSSLRedirect: true,
		SSLCiphers:       "ECDHE-RSA-AES128-GCM-SHA256",
		SSLProtocols:     "TLSv1.2 TLSv1.3",
	}

	annotations := config.ToAnnotations()

	if annotations["nginx.ingress.kubernetes.io/ssl-redirect"] != "true" {
		t.Error("expected ssl-redirect annotation")
	}
	if annotations["nginx.ingress.kubernetes.io/force-ssl-redirect"] != "true" {
		t.Error("expected force-ssl-redirect annotation")
	}
}

func TestNginxIngressConfigCORS(t *testing.T) {
	config := &NginxIngressConfig{
		CORSEnabled:          true,
		CORSAllowOrigin:      "*",
		CORSAllowMethods:     "GET, POST",
		CORSAllowHeaders:     "Authorization",
		CORSAllowCredentials: true,
		CORSMaxAge:           3600,
	}

	annotations := config.ToAnnotations()

	if annotations["nginx.ingress.kubernetes.io/enable-cors"] != "true" {
		t.Error("expected enable-cors annotation")
	}
	if annotations["nginx.ingress.kubernetes.io/cors-allow-origin"] != "*" {
		t.Error("expected cors-allow-origin annotation")
	}
	if annotations["nginx.ingress.kubernetes.io/cors-allow-credentials"] != "true" {
		t.Error("expected cors-allow-credentials annotation")
	}
}

func TestNginxIngressConfigRateLimit(t *testing.T) {
	config := &NginxIngressConfig{
		RateLimitConnections: 10,
		RateLimitRPS:         100,
		RateLimitRPM:         1000,
	}

	annotations := config.ToAnnotations()

	if annotations["nginx.ingress.kubernetes.io/limit-connections"] != "10" {
		t.Error("expected limit-connections annotation")
	}
	if annotations["nginx.ingress.kubernetes.io/limit-rps"] != "100" {
		t.Error("expected limit-rps annotation")
	}
}

func TestNginxIngressConfigCanary(t *testing.T) {
	config := &NginxIngressConfig{
		CanaryEnabled:     true,
		CanaryWeight:      20,
		CanaryHeader:      "x-canary",
		CanaryHeaderValue: "true",
	}

	annotations := config.ToAnnotations()

	if annotations["nginx.ingress.kubernetes.io/canary"] != "true" {
		t.Error("expected canary annotation")
	}
	if annotations["nginx.ingress.kubernetes.io/canary-weight"] != "20" {
		t.Error("expected canary-weight annotation")
	}
	if annotations["nginx.ingress.kubernetes.io/canary-by-header"] != "x-canary" {
		t.Error("expected canary-by-header annotation")
	}
}

func TestNginxIngressConfigAuth(t *testing.T) {
	config := &NginxIngressConfig{
		AuthType:   "basic",
		AuthSecret: "auth-secret",
		AuthRealm:  "Authentication Required",
	}

	annotations := config.ToAnnotations()

	if annotations["nginx.ingress.kubernetes.io/auth-type"] != "basic" {
		t.Error("expected auth-type annotation")
	}
	if annotations["nginx.ingress.kubernetes.io/auth-secret"] != "auth-secret" {
		t.Error("expected auth-secret annotation")
	}
}

func TestNginxIngressConfigRewrite(t *testing.T) {
	config := &NginxIngressConfig{
		RewriteTarget: "/$1",
		AppRoot:       "/app",
	}

	annotations := config.ToAnnotations()

	if annotations["nginx.ingress.kubernetes.io/rewrite-target"] != "/$1" {
		t.Error("expected rewrite-target annotation")
	}
	if annotations["nginx.ingress.kubernetes.io/app-root"] != "/app" {
		t.Error("expected app-root annotation")
	}
}

func TestTraefikIngressConfig(t *testing.T) {
	config := &TraefikIngressConfig{
		EntryPoints:      []string{"websecure"},
		Priority:         100,
		TLSCertResolver:  "letsencrypt",
		Middlewares:      []string{"default-headers@kubernetescrd"},
		Sticky:           true,
		StickyCookieName: "server",
	}

	annotations := config.ToAnnotations()

	if annotations["traefik.ingress.kubernetes.io/router.entrypoints"] != "websecure" {
		t.Error("expected entrypoints annotation")
	}
	if annotations["traefik.ingress.kubernetes.io/router.priority"] != "100" {
		t.Error("expected priority annotation")
	}
	if annotations["traefik.ingress.kubernetes.io/service.sticky.cookie"] != "true" {
		t.Error("expected sticky cookie annotation")
	}
}

func TestAWSALBIngressConfig(t *testing.T) {
	config := &AWSALBIngressConfig{
		Scheme:          "internet-facing",
		IPAddressType:   "ipv4",
		TargetType:      "ip",
		SecurityGroups:  []string{"sg-12345"},
		Subnets:         []string{"subnet-1", "subnet-2"},
		CertificateARN:  "arn:aws:acm:us-east-1:123456789:certificate/abc",
		SSLPolicy:       "ELBSecurityPolicy-TLS-1-2-2017-01",
		HealthCheckPath: "/health",
		Tags:            map[string]string{"env": "prod"},
	}

	annotations := config.ToAnnotations()

	if annotations["kubernetes.io/ingress.class"] != "alb" {
		t.Error("expected alb ingress class")
	}
	if annotations["alb.ingress.kubernetes.io/scheme"] != "internet-facing" {
		t.Error("expected scheme annotation")
	}
	if annotations["alb.ingress.kubernetes.io/target-type"] != "ip" {
		t.Error("expected target-type annotation")
	}
	if !strings.Contains(annotations["alb.ingress.kubernetes.io/tags"], "env=prod") {
		t.Error("expected tags annotation")
	}
}

func TestAWSALBCognitoConfig(t *testing.T) {
	config := &AWSALBIngressConfig{
		CognitoUserPoolARN:      "arn:aws:cognito-idp:us-east-1:123456789:userpool/abc",
		CognitoUserPoolClientID: "client-id",
		CognitoUserPoolDomain:   "auth.example.com",
		AuthOnUnauthenticated:   "authenticate",
	}

	annotations := config.ToAnnotations()

	if annotations["alb.ingress.kubernetes.io/auth-type"] != "cognito" {
		t.Error("expected auth-type cognito")
	}
	if !strings.Contains(annotations["alb.ingress.kubernetes.io/auth-idp-cognito"], "UserPoolArn") {
		t.Error("expected cognito config annotation")
	}
}

func TestGCPIngressConfig(t *testing.T) {
	config := &GCPIngressConfig{
		GlobalStaticIPName:  "my-static-ip",
		BackendConfigName:   "my-backend-config",
		ManagedCertificates: []string{"cert-1", "cert-2"},
	}

	annotations := config.ToAnnotations()

	if annotations["kubernetes.io/ingress.global-static-ip-name"] != "my-static-ip" {
		t.Error("expected global-static-ip-name annotation")
	}
	if !strings.Contains(annotations["cloud.google.com/backend-config"], "my-backend-config") {
		t.Error("expected backend-config annotation")
	}
	if annotations["networking.gke.io/managed-certificates"] != "cert-1,cert-2" {
		t.Error("expected managed-certificates annotation")
	}
}

func TestIngressBuilder(t *testing.T) {
	ingress := NewIngressBuilder("test", "default").
		WithIngressClass("nginx").
		WithTLS([]string{"example.com"}, "tls-secret").
		WithRule("example.com",
			PathPrefix("/api", "api-service", 8080),
			PathPrefix("/", "web-service", 80),
		).
		WithAnnotation("custom", "value").
		WithLabel("env", "prod").
		Build()

	if ingress.Metadata.Name != "test" {
		t.Errorf("expected name 'test', got '%s'", ingress.Metadata.Name)
	}
	if ingress.Spec.IngressClassName != "nginx" {
		t.Error("expected nginx ingress class")
	}
	if len(ingress.Spec.TLS) != 1 {
		t.Error("expected 1 TLS config")
	}
	if len(ingress.Spec.Rules) != 1 {
		t.Error("expected 1 rule")
	}
	if ingress.Metadata.Annotations["custom"] != "value" {
		t.Error("expected custom annotation")
	}
	if ingress.Metadata.Labels["env"] != "prod" {
		t.Error("expected env label")
	}
}

func TestIngressBuilderWithNginx(t *testing.T) {
	ingress := NewIngressBuilder("test", "default").
		WithNginx(&NginxIngressConfig{
			SSLRedirect:   true,
			ProxyBodySize: "50m",
		}).
		Build()

	if ingress.Metadata.Annotations["nginx.ingress.kubernetes.io/ssl-redirect"] != "true" {
		t.Error("expected nginx ssl-redirect annotation")
	}
}

func TestIngressBuilderWithDefaultBackend(t *testing.T) {
	ingress := NewIngressBuilder("test", "default").
		WithDefaultBackend("default-service", 80).
		Build()

	if ingress.Spec.DefaultBackend == nil {
		t.Fatal("expected default backend")
	}
	if ingress.Spec.DefaultBackend.Service.Name != "default-service" {
		t.Error("expected default-service name")
	}
}

func TestPathPrefix(t *testing.T) {
	path := PathPrefix("/api", "api-service", 8080)

	if path.Path != "/api" {
		t.Errorf("expected path '/api', got '%s'", path.Path)
	}
	if path.PathType != "Prefix" {
		t.Errorf("expected pathType 'Prefix', got '%s'", path.PathType)
	}
	if path.Backend.Service.Name != "api-service" {
		t.Error("expected api-service")
	}
	if path.Backend.Service.Port.Number != 8080 {
		t.Errorf("expected port 8080, got %d", path.Backend.Service.Port.Number)
	}
}

func TestPathExact(t *testing.T) {
	path := PathExact("/health", "health-service", 8080)

	if path.PathType != "Exact" {
		t.Errorf("expected pathType 'Exact', got '%s'", path.PathType)
	}
}

func TestPathImplementationSpecific(t *testing.T) {
	path := PathImplementationSpecific("/legacy/*", "legacy-service", 80)

	if path.PathType != "ImplementationSpecific" {
		t.Errorf("expected pathType 'ImplementationSpecific', got '%s'", path.PathType)
	}
}

func TestGenerateVaultaireIngress(t *testing.T) {
	ingress := GenerateVaultaireIngress("vaultaire", "api.vaultaire.io", "vaultaire-tls", "nginx")

	if ingress.Metadata.Name != "vaultaire" {
		t.Errorf("expected name 'vaultaire', got '%s'", ingress.Metadata.Name)
	}
	if ingress.Metadata.Namespace != "vaultaire" {
		t.Errorf("expected namespace 'vaultaire', got '%s'", ingress.Metadata.Namespace)
	}

	// Check TLS
	if len(ingress.Spec.TLS) != 1 {
		t.Fatal("expected 1 TLS config")
	}
	if ingress.Spec.TLS[0].SecretName != "vaultaire-tls" {
		t.Error("expected vaultaire-tls secret")
	}

	// Check rules
	if len(ingress.Spec.Rules) != 1 {
		t.Fatal("expected 1 rule")
	}
	if ingress.Spec.Rules[0].Host != "api.vaultaire.io" {
		t.Error("expected api.vaultaire.io host")
	}

	// Check paths
	paths := ingress.Spec.Rules[0].HTTP.Paths
	if len(paths) != 4 {
		t.Errorf("expected 4 paths, got %d", len(paths))
	}

	// Check nginx annotations
	if ingress.Metadata.Annotations["nginx.ingress.kubernetes.io/ssl-redirect"] != "true" {
		t.Error("expected ssl-redirect annotation")
	}
	if ingress.Metadata.Annotations["nginx.ingress.kubernetes.io/enable-cors"] != "true" {
		t.Error("expected enable-cors annotation")
	}
}

func TestIngressSet(t *testing.T) {
	im := NewIngressManager("default")

	set := &IngressSet{}
	set.Add(im.GenerateIngress(K8sIngressConfig{Name: "ingress-1"}))
	set.Add(im.GenerateIngress(K8sIngressConfig{Name: "ingress-2"}))

	yaml, err := set.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	docs := strings.Split(yaml, "---")
	if len(docs) != 2 {
		t.Errorf("expected 2 YAML documents, got %d", len(docs))
	}
}

func TestIngressWithMultipleTLS(t *testing.T) {
	ingress := NewIngressBuilder("test", "default").
		WithTLS([]string{"app1.example.com"}, "tls-app1").
		WithTLS([]string{"app2.example.com"}, "tls-app2").
		Build()

	if len(ingress.Spec.TLS) != 2 {
		t.Errorf("expected 2 TLS configs, got %d", len(ingress.Spec.TLS))
	}
}

func TestIngressWithMultipleRules(t *testing.T) {
	ingress := NewIngressBuilder("test", "default").
		WithRule("app1.example.com", PathPrefix("/", "app1", 80)).
		WithRule("app2.example.com", PathPrefix("/", "app2", 80)).
		Build()

	if len(ingress.Spec.Rules) != 2 {
		t.Errorf("expected 2 rules, got %d", len(ingress.Spec.Rules))
	}
}

func TestNginxSnippets(t *testing.T) {
	config := &NginxIngressConfig{
		ServerSnippet:        "add_header X-Frame-Options DENY;",
		ConfigurationSnippet: "proxy_set_header X-Real-IP $remote_addr;",
	}

	annotations := config.ToAnnotations()

	if annotations["nginx.ingress.kubernetes.io/server-snippet"] != "add_header X-Frame-Options DENY;" {
		t.Error("expected server-snippet annotation")
	}
	if annotations["nginx.ingress.kubernetes.io/configuration-snippet"] != "proxy_set_header X-Real-IP $remote_addr;" {
		t.Error("expected configuration-snippet annotation")
	}
}

func TestNginxWhitelist(t *testing.T) {
	config := &NginxIngressConfig{
		WhitelistSourceRange: "10.0.0.0/8,192.168.0.0/16",
	}

	annotations := config.ToAnnotations()

	if annotations["nginx.ingress.kubernetes.io/whitelist-source-range"] != "10.0.0.0/8,192.168.0.0/16" {
		t.Error("expected whitelist-source-range annotation")
	}
}

func TestNginxModSecurity(t *testing.T) {
	config := &NginxIngressConfig{
		EnableModsecurity:  true,
		ModsecuritySnippet: "SecRuleEngine On",
	}

	annotations := config.ToAnnotations()

	if annotations["nginx.ingress.kubernetes.io/enable-modsecurity"] != "true" {
		t.Error("expected enable-modsecurity annotation")
	}
	if annotations["nginx.ingress.kubernetes.io/modsecurity-snippet"] != "SecRuleEngine On" {
		t.Error("expected modsecurity-snippet annotation")
	}
}

func TestIngressBuilderMultipleProviders(t *testing.T) {
	ingress := NewIngressBuilder("test", "default").
		WithNginx(&NginxIngressConfig{SSLRedirect: true}).
		WithAnnotation("custom", "value").
		Build()

	if ingress.Metadata.Annotations["nginx.ingress.kubernetes.io/ssl-redirect"] != "true" {
		t.Error("expected nginx annotation")
	}
	if ingress.Metadata.Annotations["custom"] != "value" {
		t.Error("expected custom annotation")
	}
}
