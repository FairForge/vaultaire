// internal/k8s/manifests_test.go
package k8s

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNewManifestGenerator(t *testing.T) {
	gen := NewManifestGenerator("myapp", "production")

	if gen.AppName != "myapp" {
		t.Errorf("expected AppName 'myapp', got '%s'", gen.AppName)
	}
	if gen.Namespace != "production" {
		t.Errorf("expected Namespace 'production', got '%s'", gen.Namespace)
	}
	if gen.Labels["app.kubernetes.io/name"] != "myapp" {
		t.Error("expected app.kubernetes.io/name label to be set")
	}
}

func TestGenerateDeployment(t *testing.T) {
	gen := NewManifestGenerator("vaultaire", "default")

	deployment, err := gen.GenerateDeployment(DeploymentConfig{
		Name:          "api",
		Image:         "vaultaire:latest",
		Replicas:      3,
		Port:          8080,
		CPURequest:    "100m",
		CPULimit:      "500m",
		MemoryRequest: "128Mi",
		MemoryLimit:   "512Mi",
		HealthPath:    "/health",
	})
	if err != nil {
		t.Fatalf("failed to generate deployment: %v", err)
	}

	if deployment.Kind != KindDeployment {
		t.Errorf("expected kind Deployment, got %s", deployment.Kind)
	}
	if deployment.APIVersion != "apps/v1" {
		t.Errorf("expected apiVersion apps/v1, got %s", deployment.APIVersion)
	}
	if deployment.Metadata.Name != "api" {
		t.Errorf("expected name 'api', got '%s'", deployment.Metadata.Name)
	}
	if deployment.Metadata.Namespace != "default" {
		t.Errorf("expected namespace 'default', got '%s'", deployment.Metadata.Namespace)
	}

	spec, ok := deployment.Spec.(DeploymentSpec)
	if !ok {
		t.Fatal("spec is not DeploymentSpec")
	}
	if spec.Replicas != 3 {
		t.Errorf("expected 3 replicas, got %d", spec.Replicas)
	}
}

func TestGenerateDeploymentWithEnvVars(t *testing.T) {
	gen := NewManifestGenerator("vaultaire", "default")

	deployment, err := gen.GenerateDeployment(DeploymentConfig{
		Name:     "api",
		Image:    "vaultaire:latest",
		Replicas: 1,
		Port:     8080,
		EnvVars: map[string]string{
			"LOG_LEVEL": "debug",
			"ENV":       "test",
		},
		SecretEnvVars: map[string]string{
			"DB_PASSWORD": "db-secrets:password",
		},
		ConfigMapEnvVars: map[string]string{
			"CONFIG_PATH": "app-config:path",
		},
	})
	if err != nil {
		t.Fatalf("failed to generate deployment: %v", err)
	}

	spec := deployment.Spec.(DeploymentSpec)
	container := spec.Template.Spec.Containers[0]

	// Check env vars are present
	foundEnvVars := make(map[string]bool)
	for _, env := range container.Env {
		foundEnvVars[env.Name] = true
	}

	if !foundEnvVars["LOG_LEVEL"] {
		t.Error("expected LOG_LEVEL env var")
	}
	if !foundEnvVars["DB_PASSWORD"] {
		t.Error("expected DB_PASSWORD env var from secret")
	}
	if !foundEnvVars["CONFIG_PATH"] {
		t.Error("expected CONFIG_PATH env var from configmap")
	}
}

func TestGenerateDeploymentWithVolumes(t *testing.T) {
	gen := NewManifestGenerator("vaultaire", "default")

	deployment, err := gen.GenerateDeployment(DeploymentConfig{
		Name:     "api",
		Image:    "vaultaire:latest",
		Replicas: 1,
		Port:     8080,
		Volumes: []VolumeConfig{
			{Name: "config", MountPath: "/etc/config", ConfigMap: "app-config", ReadOnly: true},
			{Name: "secrets", MountPath: "/etc/secrets", Secret: "app-secrets", ReadOnly: true},
			{Name: "data", MountPath: "/data", PVC: "app-data"},
			{Name: "tmp", MountPath: "/tmp", EmptyDir: true},
		},
	})
	if err != nil {
		t.Fatalf("failed to generate deployment: %v", err)
	}

	spec := deployment.Spec.(DeploymentSpec)
	container := spec.Template.Spec.Containers[0]

	if len(container.VolumeMounts) != 4 {
		t.Errorf("expected 4 volume mounts, got %d", len(container.VolumeMounts))
	}

	volumes := spec.Template.Spec.Volumes
	if len(volumes) != 4 {
		t.Errorf("expected 4 volumes, got %d", len(volumes))
	}

	// Verify volume types
	volumeTypes := make(map[string]string)
	for _, v := range volumes {
		switch {
		case v.ConfigMap != nil:
			volumeTypes[v.Name] = "configMap"
		case v.Secret != nil:
			volumeTypes[v.Name] = "secret"
		case v.PersistentVolumeClaim != nil:
			volumeTypes[v.Name] = "pvc"
		case v.EmptyDir != nil:
			volumeTypes[v.Name] = "emptyDir"
		}
	}

	if volumeTypes["config"] != "configMap" {
		t.Error("expected config volume to be configMap")
	}
	if volumeTypes["secrets"] != "secret" {
		t.Error("expected secrets volume to be secret")
	}
	if volumeTypes["data"] != "pvc" {
		t.Error("expected data volume to be pvc")
	}
	if volumeTypes["tmp"] != "emptyDir" {
		t.Error("expected tmp volume to be emptyDir")
	}
}

func TestGenerateService(t *testing.T) {
	gen := NewManifestGenerator("vaultaire", "default")

	svc := gen.GenerateService("api", 80, 8080, "ClusterIP")

	if svc.Kind != KindService {
		t.Errorf("expected kind Service, got %s", svc.Kind)
	}
	if svc.APIVersion != "v1" {
		t.Errorf("expected apiVersion v1, got %s", svc.APIVersion)
	}

	spec, ok := svc.Spec.(ServiceSpec)
	if !ok {
		t.Fatal("spec is not ServiceSpec")
	}
	if spec.Type != "ClusterIP" {
		t.Errorf("expected type ClusterIP, got %s", spec.Type)
	}
	if len(spec.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(spec.Ports))
	}
	if spec.Ports[0].Port != 80 {
		t.Errorf("expected port 80, got %d", spec.Ports[0].Port)
	}
	if spec.Ports[0].TargetPort != 8080 {
		t.Errorf("expected targetPort 8080, got %d", spec.Ports[0].TargetPort)
	}
}

func TestGenerateServiceLoadBalancer(t *testing.T) {
	gen := NewManifestGenerator("vaultaire", "default")

	svc := gen.GenerateService("api", 443, 8443, "LoadBalancer")

	spec := svc.Spec.(ServiceSpec)
	if spec.Type != "LoadBalancer" {
		t.Errorf("expected type LoadBalancer, got %s", spec.Type)
	}
}

func TestGenerateConfigMap(t *testing.T) {
	gen := NewManifestGenerator("vaultaire", "default")

	cm := gen.GenerateConfigMap("app-config", map[string]string{
		"config.yaml": "key: value",
		"settings":    "debug=true",
	})

	if cm.Kind != KindConfigMap {
		t.Errorf("expected kind ConfigMap, got %s", cm.Kind)
	}
	if cm.APIVersion != "v1" {
		t.Errorf("expected apiVersion v1, got %s", cm.APIVersion)
	}
	if cm.Data["config.yaml"] != "key: value" {
		t.Error("expected config.yaml data to be set")
	}
	if cm.Data["settings"] != "debug=true" {
		t.Error("expected settings data to be set")
	}
}

func TestGenerateSecret(t *testing.T) {
	gen := NewManifestGenerator("vaultaire", "default")

	secret := gen.GenerateSecret("db-secrets", map[string]string{
		"username": "admin",
		"password": "secret123",
	}, "Opaque")

	if secret.Kind != KindSecret {
		t.Errorf("expected kind Secret, got %s", secret.Kind)
	}
	if secret.Type != "Opaque" {
		t.Errorf("expected type Opaque, got %s", secret.Type)
	}
	if secret.StringData["username"] != "admin" {
		t.Error("expected username to be set")
	}
}

func TestGenerateServiceAccount(t *testing.T) {
	gen := NewManifestGenerator("vaultaire", "default")

	sa := gen.GenerateServiceAccount("vaultaire-sa")

	if sa.Kind != KindServiceAccount {
		t.Errorf("expected kind ServiceAccount, got %s", sa.Kind)
	}
	if sa.Metadata.Name != "vaultaire-sa" {
		t.Errorf("expected name 'vaultaire-sa', got '%s'", sa.Metadata.Name)
	}
}

func TestManifestToYAML(t *testing.T) {
	gen := NewManifestGenerator("vaultaire", "default")
	svc := gen.GenerateService("api", 80, 8080, "ClusterIP")

	yaml, err := svc.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "apiVersion: v1") {
		t.Error("expected YAML to contain apiVersion")
	}
	if !strings.Contains(yaml, "kind: Service") {
		t.Error("expected YAML to contain kind")
	}
	if !strings.Contains(yaml, "name: api") {
		t.Error("expected YAML to contain name")
	}
}

func TestManifestSetToYAML(t *testing.T) {
	gen := NewManifestGenerator("vaultaire", "default")

	ms := &ManifestSet{}
	ms.Add(gen.GenerateServiceAccount("vaultaire"))
	ms.Add(gen.GenerateConfigMap("config", map[string]string{"key": "value"}))
	ms.Add(gen.GenerateService("api", 80, 8080, "ClusterIP"))

	yaml, err := ms.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert manifest set to YAML: %v", err)
	}

	// Count document separators
	docs := strings.Split(yaml, "---")
	if len(docs) != 3 {
		t.Errorf("expected 3 YAML documents, got %d", len(docs))
	}
}

func TestGenerateVaultaireManifests(t *testing.T) {
	ms, err := GenerateVaultaireManifests("vaultaire", "vaultaire:v1.0.0", 3)
	if err != nil {
		t.Fatalf("failed to generate manifests: %v", err)
	}

	if len(ms.Manifests) < 4 {
		t.Errorf("expected at least 4 manifests, got %d", len(ms.Manifests))
	}

	// Verify each manifest type exists
	kinds := make(map[ManifestKind]int)
	for _, m := range ms.Manifests {
		kinds[m.Kind]++
	}

	if kinds[KindServiceAccount] != 1 {
		t.Error("expected 1 ServiceAccount")
	}
	if kinds[KindConfigMap] != 1 {
		t.Error("expected 1 ConfigMap")
	}
	if kinds[KindSecret] != 1 {
		t.Error("expected 1 Secret")
	}
	if kinds[KindDeployment] != 1 {
		t.Error("expected 1 Deployment")
	}
	if kinds[KindService] != 1 {
		t.Error("expected 1 Service")
	}
}

func TestDeploymentSecurityContext(t *testing.T) {
	gen := NewManifestGenerator("vaultaire", "default")

	deployment, err := gen.GenerateDeployment(DeploymentConfig{
		Name:     "api",
		Image:    "vaultaire:latest",
		Replicas: 1,
		Port:     8080,
	})
	if err != nil {
		t.Fatalf("failed to generate deployment: %v", err)
	}

	spec := deployment.Spec.(DeploymentSpec)
	container := spec.Template.Spec.Containers[0]

	// Verify container security context
	if container.SecurityContext == nil {
		t.Fatal("expected container security context")
	}
	if container.SecurityContext.RunAsNonRoot == nil || !*container.SecurityContext.RunAsNonRoot {
		t.Error("expected runAsNonRoot to be true")
	}
	if container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("expected readOnlyRootFilesystem to be true")
	}
	if container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Error("expected allowPrivilegeEscalation to be false")
	}
	if container.SecurityContext.Capabilities == nil || len(container.SecurityContext.Capabilities.Drop) == 0 {
		t.Error("expected capabilities to drop ALL")
	}

	// Verify pod security context
	podSecurity := spec.Template.Spec.SecurityContext
	if podSecurity == nil {
		t.Fatal("expected pod security context")
	}
	if podSecurity.RunAsNonRoot == nil || !*podSecurity.RunAsNonRoot {
		t.Error("expected pod runAsNonRoot to be true")
	}
}

func TestDeploymentProbes(t *testing.T) {
	gen := NewManifestGenerator("vaultaire", "default")

	deployment, err := gen.GenerateDeployment(DeploymentConfig{
		Name:       "api",
		Image:      "vaultaire:latest",
		Replicas:   1,
		Port:       8080,
		HealthPath: "/healthz",
		HealthPort: 8081,
	})
	if err != nil {
		t.Fatalf("failed to generate deployment: %v", err)
	}

	spec := deployment.Spec.(DeploymentSpec)
	container := spec.Template.Spec.Containers[0]

	if container.LivenessProbe == nil {
		t.Fatal("expected liveness probe")
	}
	if container.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("expected liveness path '/healthz', got '%s'", container.LivenessProbe.HTTPGet.Path)
	}
	if container.LivenessProbe.HTTPGet.Port != 8081 {
		t.Errorf("expected liveness port 8081, got %d", container.LivenessProbe.HTTPGet.Port)
	}

	if container.ReadinessProbe == nil {
		t.Fatal("expected readiness probe")
	}
	if container.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("expected readiness path '/healthz', got '%s'", container.ReadinessProbe.HTTPGet.Path)
	}
}

func TestManifestLabelsAndAnnotations(t *testing.T) {
	gen := NewManifestGenerator("vaultaire", "default")
	gen.WithLabels(map[string]string{
		"environment": "production",
		"team":        "platform",
	})
	gen.WithAnnotations(map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   "8080",
	})

	svc := gen.GenerateService("api", 80, 8080, "ClusterIP")

	if svc.Metadata.Labels["environment"] != "production" {
		t.Error("expected environment label")
	}
	if svc.Metadata.Labels["team"] != "platform" {
		t.Error("expected team label")
	}
	if svc.Metadata.Annotations["prometheus.io/scrape"] != "true" {
		t.Error("expected prometheus scrape annotation")
	}
}

func TestManifestYAMLValidity(t *testing.T) {
	ms, err := GenerateVaultaireManifests("vaultaire", "vaultaire:latest", 1)
	if err != nil {
		t.Fatalf("failed to generate manifests: %v", err)
	}

	yamlContent, err := ms.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	// Try to parse each document
	docs := strings.Split(yamlContent, "---")
	for i, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}

		var parsed map[string]interface{}
		if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
			t.Errorf("document %d is not valid YAML: %v", i, err)
		}

		// Verify required fields
		if _, ok := parsed["apiVersion"]; !ok {
			t.Errorf("document %d missing apiVersion", i)
		}
		if _, ok := parsed["kind"]; !ok {
			t.Errorf("document %d missing kind", i)
		}
		if _, ok := parsed["metadata"]; !ok {
			t.Errorf("document %d missing metadata", i)
		}
	}
}

func TestWithLabels(t *testing.T) {
	gen := NewManifestGenerator("app", "ns")
	gen.WithLabels(map[string]string{"env": "prod"})

	if gen.Labels["env"] != "prod" {
		t.Error("expected env label to be set")
	}
	// Original labels should still exist
	if gen.Labels["app.kubernetes.io/name"] != "app" {
		t.Error("expected original label to persist")
	}
}

func TestWithAnnotations(t *testing.T) {
	gen := NewManifestGenerator("app", "ns")
	gen.WithAnnotations(map[string]string{"note": "test"})

	if gen.Annotations["note"] != "test" {
		t.Error("expected annotation to be set")
	}
}

func TestManifestTemplate(t *testing.T) {
	tmpl, err := NewManifestTemplate("test", `
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data:
  key: {{ .Value }}
`)
	if err != nil {
		t.Fatalf("failed to create template: %v", err)
	}

	result, err := tmpl.Execute(map[string]string{
		"Name":      "my-config",
		"Namespace": "default",
		"Value":     "test-value",
	})
	if err != nil {
		t.Fatalf("failed to execute template: %v", err)
	}

	if !strings.Contains(result, "name: my-config") {
		t.Error("expected name in result")
	}
	if !strings.Contains(result, "key: test-value") {
		t.Error("expected value in result")
	}
}

func TestCopyLabels(t *testing.T) {
	original := map[string]string{"a": "1", "b": "2"}
	copied := copyLabels(original)

	// Modify copy
	copied["c"] = "3"

	// Original should be unchanged
	if _, ok := original["c"]; ok {
		t.Error("original should not be modified")
	}
}

func TestBoolPtr(t *testing.T) {
	truePtr := boolPtr(true)
	falsePtr := boolPtr(false)

	if !*truePtr {
		t.Error("expected true")
	}
	if *falsePtr {
		t.Error("expected false")
	}
}

func TestWithDefault(t *testing.T) {
	if withDefault("", "default") != "default" {
		t.Error("expected default value")
	}
	if withDefault("custom", "default") != "custom" {
		t.Error("expected custom value")
	}
}

func TestGeneratedAt(t *testing.T) {
	result := GeneratedAt()
	if !strings.HasPrefix(result, "# Generated by Vaultaire at") {
		t.Error("expected generated comment")
	}
}
