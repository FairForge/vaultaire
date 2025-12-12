// internal/k8s/security_test.go
package k8s

import (
	"strings"
	"testing"
)

func TestNewSecurityManager(t *testing.T) {
	sm := NewSecurityManager("default")

	if sm.namespace != "default" {
		t.Errorf("expected namespace 'default', got '%s'", sm.namespace)
	}
}

func TestGenerateRole(t *testing.T) {
	sm := NewSecurityManager("default")

	role := sm.GenerateRole(RoleConfig{
		Name: "test-role",
		Rules: []PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods", "services"},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	})

	if role.Kind != "Role" {
		t.Errorf("expected kind Role, got %s", role.Kind)
	}
	if role.APIVersion != "rbac.authorization.k8s.io/v1" {
		t.Errorf("expected apiVersion rbac.authorization.k8s.io/v1, got %s", role.APIVersion)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(role.Rules))
	}
	if len(role.Rules[0].Verbs) != 3 {
		t.Error("expected 3 verbs")
	}
}

func TestGenerateClusterRole(t *testing.T) {
	sm := NewSecurityManager("")

	role := sm.GenerateClusterRole(RoleConfig{
		Name: "test-cluster-role",
		Rules: []PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"get", "list"},
			},
		},
	})

	if role.Kind != "ClusterRole" {
		t.Errorf("expected kind ClusterRole, got %s", role.Kind)
	}
	if role.Metadata.Namespace != "" {
		t.Error("ClusterRole should not have namespace")
	}
}

func TestGenerateRoleWithResourceNames(t *testing.T) {
	sm := NewSecurityManager("default")

	role := sm.GenerateRole(RoleConfig{
		Name: "specific-secrets",
		Rules: []PolicyRule{
			{
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				ResourceNames: []string{"db-password", "api-key"},
				Verbs:         []string{"get"},
			},
		},
	})

	if len(role.Rules[0].ResourceNames) != 2 {
		t.Error("expected 2 resource names")
	}
}

func TestGenerateRoleWithNonResourceURLs(t *testing.T) {
	sm := NewSecurityManager("")

	role := sm.GenerateClusterRole(RoleConfig{
		Name: "health-checker",
		Rules: []PolicyRule{
			{
				NonResourceURLs: []string{"/healthz", "/readyz"},
				Verbs:           []string{"get"},
			},
		},
	})

	if len(role.Rules[0].NonResourceURLs) != 2 {
		t.Error("expected 2 non-resource URLs")
	}
}

func TestRoleToYAML(t *testing.T) {
	sm := NewSecurityManager("default")

	role := sm.GenerateRole(RoleConfig{
		Name:  "test-role",
		Rules: []PolicyRule{{Verbs: []string{"get"}}},
	})

	yaml, err := role.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "kind: Role") {
		t.Error("expected YAML to contain kind")
	}
}

func TestGenerateRoleBinding(t *testing.T) {
	sm := NewSecurityManager("default")

	rb := sm.GenerateRoleBinding(RoleBindingConfig{
		Name: "test-binding",
		RoleRef: RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "test-role",
		},
		Subjects: []Subject{
			{Kind: "ServiceAccount", Name: "test-sa", Namespace: "default"},
		},
	})

	if rb.Kind != "RoleBinding" {
		t.Errorf("expected kind RoleBinding, got %s", rb.Kind)
	}
	if rb.RoleRef.Name != "test-role" {
		t.Error("expected role ref name")
	}
	if len(rb.Subjects) != 1 {
		t.Error("expected 1 subject")
	}
}

func TestGenerateClusterRoleBinding(t *testing.T) {
	sm := NewSecurityManager("")

	rb := sm.GenerateClusterRoleBinding(RoleBindingConfig{
		Name: "test-cluster-binding",
		RoleRef: RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "test-cluster-role",
		},
		Subjects: []Subject{
			{Kind: "Group", Name: "developers", APIGroup: "rbac.authorization.k8s.io"},
		},
	})

	if rb.Kind != "ClusterRoleBinding" {
		t.Errorf("expected kind ClusterRoleBinding, got %s", rb.Kind)
	}
}

func TestGenerateRoleBindingMultipleSubjects(t *testing.T) {
	sm := NewSecurityManager("default")

	rb := sm.GenerateRoleBinding(RoleBindingConfig{
		Name: "multi-subject",
		RoleRef: RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "test-role",
		},
		Subjects: []Subject{
			{Kind: "ServiceAccount", Name: "sa-1", Namespace: "default"},
			{Kind: "ServiceAccount", Name: "sa-2", Namespace: "default"},
			{Kind: "User", Name: "admin", APIGroup: "rbac.authorization.k8s.io"},
		},
	})

	if len(rb.Subjects) != 3 {
		t.Errorf("expected 3 subjects, got %d", len(rb.Subjects))
	}
}

func TestRoleBindingToYAML(t *testing.T) {
	sm := NewSecurityManager("default")

	rb := sm.GenerateRoleBinding(RoleBindingConfig{
		Name: "test-binding",
		RoleRef: RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "test-role",
		},
		Subjects: []Subject{
			{Kind: "ServiceAccount", Name: "test-sa"},
		},
	})

	yaml, err := rb.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if !strings.Contains(yaml, "kind: RoleBinding") {
		t.Error("expected YAML to contain kind")
	}
}

func TestGetPodSecurityLabels(t *testing.T) {
	labels := GetPodSecurityLabels(PSSRestricted, PSSModeEnforce, "v1.28")

	if labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Error("expected enforce label")
	}
	if labels["pod-security.kubernetes.io/enforce-version"] != "v1.28" {
		t.Error("expected enforce-version label")
	}
}

func TestGetPodSecurityLabelsDefaultVersion(t *testing.T) {
	labels := GetPodSecurityLabels(PSSBaseline, PSSModeWarn, "")

	if labels["pod-security.kubernetes.io/warn-version"] != "latest" {
		t.Error("expected default version 'latest'")
	}
}

func TestRestrictedSecurityContext(t *testing.T) {
	ctx := RestrictedSecurityContext()

	if ctx.RunAsNonRoot == nil || !*ctx.RunAsNonRoot {
		t.Error("expected runAsNonRoot true")
	}
	if ctx.ReadOnlyRootFilesystem == nil || !*ctx.ReadOnlyRootFilesystem {
		t.Error("expected readOnlyRootFilesystem true")
	}
	if ctx.AllowPrivilegeEscalation == nil || *ctx.AllowPrivilegeEscalation {
		t.Error("expected allowPrivilegeEscalation false")
	}
	if ctx.Privileged == nil || *ctx.Privileged {
		t.Error("expected privileged false")
	}
	if ctx.Capabilities == nil || len(ctx.Capabilities.Drop) != 1 || ctx.Capabilities.Drop[0] != "ALL" {
		t.Error("expected capabilities drop ALL")
	}
	if ctx.SeccompProfile == nil || ctx.SeccompProfile.Type != "RuntimeDefault" {
		t.Error("expected seccomp RuntimeDefault")
	}
}

func TestBaselineSecurityContext(t *testing.T) {
	ctx := BaselineSecurityContext()

	if ctx.AllowPrivilegeEscalation == nil || *ctx.AllowPrivilegeEscalation {
		t.Error("expected allowPrivilegeEscalation false")
	}
	if ctx.Capabilities == nil {
		t.Fatal("expected capabilities")
	}
	if len(ctx.Capabilities.Add) != 1 || ctx.Capabilities.Add[0] != "NET_BIND_SERVICE" {
		t.Error("expected NET_BIND_SERVICE capability")
	}
}

func TestContainerSecurityContextToSpec(t *testing.T) {
	ctx := RestrictedSecurityContext()
	spec := ctx.ToSpec()

	if spec == nil {
		t.Fatal("expected spec")
	}
	if spec.RunAsNonRoot == nil || !*spec.RunAsNonRoot {
		t.Error("expected runAsNonRoot in spec")
	}
	if spec.Capabilities == nil {
		t.Error("expected capabilities in spec")
	}
	if spec.SeccompProfile == nil {
		t.Error("expected seccompProfile in spec")
	}
}

func TestContainerSecurityContextToSpecNil(t *testing.T) {
	var ctx *SecurityContainerContext
	spec := ctx.ToSpec()

	if spec != nil {
		t.Error("expected nil spec for nil context")
	}
}

func TestPodSecurityContextToSpec(t *testing.T) {
	runAsUser := int64(1000)
	fsGroup := int64(2000)
	nonRoot := true

	ctx := &SecurityPodContext{
		RunAsUser:    &runAsUser,
		FSGroup:      &fsGroup,
		RunAsNonRoot: &nonRoot,
		Sysctls: []Sysctl{
			{Name: "net.core.somaxconn", Value: "1024"},
		},
		SeccompProfile: &SeccompProfile{
			Type: "RuntimeDefault",
		},
	}

	spec := ctx.ToSpec()

	if spec == nil {
		t.Fatal("expected spec")
	}
	if *spec.RunAsUser != 1000 {
		t.Error("expected runAsUser 1000")
	}
	if *spec.FSGroup != 2000 {
		t.Error("expected fsGroup 2000")
	}
	if len(spec.Sysctls) != 1 {
		t.Error("expected 1 sysctl")
	}
}

func TestPodSecurityContextToSpecNil(t *testing.T) {
	var ctx *SecurityPodContext
	spec := ctx.ToSpec()

	if spec != nil {
		t.Error("expected nil spec for nil context")
	}
}

func TestContainerSecurityContextWithSELinux(t *testing.T) {
	ctx := &SecurityContainerContext{
		SELinuxOptions: &SELinuxOptions{
			User:  "system_u",
			Role:  "system_r",
			Type:  "container_t",
			Level: "s0:c123,c456",
		},
	}

	spec := ctx.ToSpec()

	if spec.SELinuxOptions == nil {
		t.Fatal("expected SELinux options")
	}
	if spec.SELinuxOptions.Type != "container_t" {
		t.Error("expected SELinux type")
	}
}

func TestGenerateVaultaireRBAC(t *testing.T) {
	roles, bindings := GenerateVaultaireRBAC("vaultaire")

	if len(roles) < 2 {
		t.Errorf("expected at least 2 roles, got %d", len(roles))
	}
	if len(bindings) < 2 {
		t.Errorf("expected at least 2 bindings, got %d", len(bindings))
	}

	roleNames := make(map[string]bool)
	for _, r := range roles {
		roleNames[r.Metadata.Name] = true
	}

	if !roleNames["vaultaire-api"] {
		t.Error("expected vaultaire-api role")
	}
	if !roleNames["vaultaire-worker"] {
		t.Error("expected vaultaire-worker role")
	}
}

func TestSecuritySet(t *testing.T) {
	sm := NewSecurityManager("default")

	set := &SecuritySet{
		Roles: []*RoleResource{
			sm.GenerateRole(RoleConfig{Name: "role-1", Rules: []PolicyRule{{Verbs: []string{"get"}}}}),
		},
		RoleBindings: []*RoleBindingResource{
			sm.GenerateRoleBinding(RoleBindingConfig{
				Name:     "binding-1",
				RoleRef:  RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "role-1"},
				Subjects: []Subject{{Kind: "ServiceAccount", Name: "sa-1"}},
			}),
		},
	}

	yaml, err := set.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	docs := strings.Split(yaml, "---")
	if len(docs) != 2 {
		t.Errorf("expected 2 YAML documents, got %d", len(docs))
	}
}

func TestRoleNamespaceDefault(t *testing.T) {
	sm := NewSecurityManager("production")

	role := sm.GenerateRole(RoleConfig{
		Name:  "test-role",
		Rules: []PolicyRule{{Verbs: []string{"get"}}},
	})

	if role.Metadata.Namespace != "production" {
		t.Errorf("expected namespace 'production', got '%s'", role.Metadata.Namespace)
	}
}

func TestRoleNamespaceOverride(t *testing.T) {
	sm := NewSecurityManager("production")

	role := sm.GenerateRole(RoleConfig{
		Name:      "test-role",
		Namespace: "staging",
		Rules:     []PolicyRule{{Verbs: []string{"get"}}},
	})

	if role.Metadata.Namespace != "staging" {
		t.Errorf("expected namespace 'staging', got '%s'", role.Metadata.Namespace)
	}
}

func TestPodSecurityStandardConstants(t *testing.T) {
	if PSSPrivileged != "privileged" {
		t.Error("expected privileged constant")
	}
	if PSSBaseline != "baseline" {
		t.Error("expected baseline constant")
	}
	if PSSRestricted != "restricted" {
		t.Error("expected restricted constant")
	}
}

func TestPodSecurityModeConstants(t *testing.T) {
	if PSSModeEnforce != "enforce" {
		t.Error("expected enforce constant")
	}
	if PSSModeAudit != "audit" {
		t.Error("expected audit constant")
	}
	if PSSModeWarn != "warn" {
		t.Error("expected warn constant")
	}
}
