// internal/k8s/configsecrets_test.go
package k8s

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewConfigMapManager(t *testing.T) {
	cm := NewConfigMapManager("default")

	if cm.namespace != "default" {
		t.Errorf("expected namespace 'default', got '%s'", cm.namespace)
	}
	if cm.labels["app.kubernetes.io/managed-by"] != "vaultaire" {
		t.Error("expected managed-by label")
	}
}

func TestCreateConfigMap(t *testing.T) {
	cm := NewConfigMapManager("default")

	data := map[string]string{
		"config.yaml": "key: value",
		"settings":    "debug=true",
	}

	config, err := cm.CreateConfigMap("app-config", data)
	if err != nil {
		t.Fatalf("failed to create configmap: %v", err)
	}

	if config.Name != "app-config" {
		t.Errorf("expected name 'app-config', got '%s'", config.Name)
	}
	if config.Namespace != "default" {
		t.Errorf("expected namespace 'default', got '%s'", config.Namespace)
	}
	if config.Data["config.yaml"] != "key: value" {
		t.Error("expected config.yaml data")
	}
	if config.Hash == "" {
		t.Error("expected hash to be computed")
	}
}

func TestCreateConfigMapEmptyName(t *testing.T) {
	cm := NewConfigMapManager("default")

	_, err := cm.CreateConfigMap("", map[string]string{"key": "value"})
	if err == nil {
		t.Error("expected error for empty name")
	}
}

func TestCreateConfigMapFromFile(t *testing.T) {
	cm := NewConfigMapManager("default")

	// Create temp file
	tmpDir, err := os.MkdirTemp("", "configmap-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("key: value"), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	config, err := cm.CreateConfigMapFromFile("app-config", "", configPath)
	if err != nil {
		t.Fatalf("failed to create configmap from file: %v", err)
	}

	if config.Data["config.yaml"] != "key: value" {
		t.Error("expected config.yaml content")
	}
}

func TestCreateConfigMapFromDirectory(t *testing.T) {
	cm := NewConfigMapManager("default")

	// Create temp directory with files
	tmpDir, err := os.MkdirTemp("", "configmap-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	files := map[string]string{
		"config.yaml":   "key: value",
		"settings.json": `{"debug": true}`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(tmpDir, name), []byte(content), 0644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}
	}

	config, err := cm.CreateConfigMapFromDirectory("app-config", tmpDir)
	if err != nil {
		t.Fatalf("failed to create configmap from directory: %v", err)
	}

	if len(config.Data) != 2 {
		t.Errorf("expected 2 files, got %d", len(config.Data))
	}
	if config.Data["config.yaml"] != "key: value" {
		t.Error("expected config.yaml content")
	}
}

func TestCreateConfigMapFromEnvFile(t *testing.T) {
	cm := NewConfigMapManager("default")

	// Create temp env file
	tmpDir, err := os.MkdirTemp("", "configmap-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	envContent := `
# Comment
DB_HOST=localhost
DB_PORT=5432
API_KEY="secret123"
EMPTY=
`
	envPath := filepath.Join(tmpDir, ".env")
	if err := os.WriteFile(envPath, []byte(envContent), 0644); err != nil {
		t.Fatalf("failed to write env file: %v", err)
	}

	config, err := cm.CreateConfigMapFromEnvFile("app-config", envPath)
	if err != nil {
		t.Fatalf("failed to create configmap from env file: %v", err)
	}

	if config.Data["DB_HOST"] != "localhost" {
		t.Errorf("expected DB_HOST 'localhost', got '%s'", config.Data["DB_HOST"])
	}
	if config.Data["DB_PORT"] != "5432" {
		t.Errorf("expected DB_PORT '5432', got '%s'", config.Data["DB_PORT"])
	}
	if config.Data["API_KEY"] != "secret123" {
		t.Errorf("expected API_KEY 'secret123', got '%s'", config.Data["API_KEY"])
	}
}

func TestGetConfigMap(t *testing.T) {
	cm := NewConfigMapManager("default")

	_, err := cm.CreateConfigMap("app-config", map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("failed to create configmap: %v", err)
	}

	config, exists := cm.GetConfigMap("app-config")
	if !exists {
		t.Error("expected configmap to exist")
	}
	if config.Name != "app-config" {
		t.Errorf("expected name 'app-config', got '%s'", config.Name)
	}

	_, exists = cm.GetConfigMap("nonexistent")
	if exists {
		t.Error("expected configmap to not exist")
	}
}

func TestUpdateConfigMap(t *testing.T) {
	cm := NewConfigMapManager("default")

	config, _ := cm.CreateConfigMap("app-config", map[string]string{"key": "value"})
	oldHash := config.Hash

	updated, err := cm.UpdateConfigMap("app-config", map[string]string{"key": "new-value"})
	if err != nil {
		t.Fatalf("failed to update configmap: %v", err)
	}

	if updated.Data["key"] != "new-value" {
		t.Error("expected updated value")
	}
	if updated.Hash == oldHash {
		t.Error("expected hash to change")
	}
}

func TestUpdateImmutableConfigMap(t *testing.T) {
	cm := NewConfigMapManager("default")

	config, _ := cm.CreateConfigMap("app-config", map[string]string{"key": "value"})
	config.Immutable = true

	_, err := cm.UpdateConfigMap("app-config", map[string]string{"key": "new-value"})
	if err == nil {
		t.Error("expected error for immutable configmap")
	}
}

func TestDeleteConfigMap(t *testing.T) {
	cm := NewConfigMapManager("default")

	_, _ = cm.CreateConfigMap("app-config", map[string]string{"key": "value"})

	err := cm.DeleteConfigMap("app-config")
	if err != nil {
		t.Fatalf("failed to delete configmap: %v", err)
	}

	_, exists := cm.GetConfigMap("app-config")
	if exists {
		t.Error("expected configmap to be deleted")
	}

	err = cm.DeleteConfigMap("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent configmap")
	}
}

func TestListConfigMaps(t *testing.T) {
	cm := NewConfigMapManager("default")

	_, _ = cm.CreateConfigMap("config-a", map[string]string{"a": "1"})
	_, _ = cm.CreateConfigMap("config-b", map[string]string{"b": "2"})
	_, _ = cm.CreateConfigMap("config-c", map[string]string{"c": "3"})

	configs := cm.ListConfigMaps()

	if len(configs) != 3 {
		t.Errorf("expected 3 configmaps, got %d", len(configs))
	}

	// Should be sorted
	if configs[0].Name != "config-a" {
		t.Error("expected configs to be sorted")
	}
}

func TestConfigMapToManifest(t *testing.T) {
	cm := NewConfigMapManager("default")

	config, _ := cm.CreateConfigMap("app-config", map[string]string{"key": "value"})

	manifest := config.ToManifest()

	if manifest.Kind != KindConfigMap {
		t.Errorf("expected kind ConfigMap, got %s", manifest.Kind)
	}
	if manifest.APIVersion != "v1" {
		t.Errorf("expected apiVersion v1, got %s", manifest.APIVersion)
	}
	if manifest.Metadata.Name != "app-config" {
		t.Errorf("expected name 'app-config', got '%s'", manifest.Metadata.Name)
	}
	if manifest.Metadata.Annotations["vaultaire.io/hash"] == "" {
		t.Error("expected hash annotation")
	}
}

// Secret Manager Tests

func TestNewSecretManager(t *testing.T) {
	key, _ := GenerateEncryptionKey()
	sm := NewSecretManager("default", key)

	if sm.namespace != "default" {
		t.Errorf("expected namespace 'default', got '%s'", sm.namespace)
	}
}

func TestCreateSecret(t *testing.T) {
	sm := NewSecretManager("default", nil)

	data := map[string]string{
		"username": "admin",
		"password": "secret123",
	}

	secret, err := sm.CreateSecret("db-credentials", data, SecretTypeOpaque)
	if err != nil {
		t.Fatalf("failed to create secret: %v", err)
	}

	if secret.Name != "db-credentials" {
		t.Errorf("expected name 'db-credentials', got '%s'", secret.Name)
	}
	if secret.Type != SecretTypeOpaque {
		t.Errorf("expected type Opaque, got %s", secret.Type)
	}
	if string(secret.Data["username"]) != "admin" {
		t.Error("expected username data")
	}
}

func TestCreateSecretEmptyName(t *testing.T) {
	sm := NewSecretManager("default", nil)

	_, err := sm.CreateSecret("", map[string]string{"key": "value"}, SecretTypeOpaque)
	if err == nil {
		t.Error("expected error for empty name")
	}
}

func TestCreateSecretDefaultType(t *testing.T) {
	sm := NewSecretManager("default", nil)

	secret, err := sm.CreateSecret("test", map[string]string{"key": "value"}, "")
	if err != nil {
		t.Fatalf("failed to create secret: %v", err)
	}

	if secret.Type != SecretTypeOpaque {
		t.Errorf("expected default type Opaque, got %s", secret.Type)
	}
}

func TestCreateTLSSecret(t *testing.T) {
	sm := NewSecretManager("default", nil)

	// Create temp cert and key files
	tmpDir, err := os.MkdirTemp("", "secret-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	certPath := filepath.Join(tmpDir, "tls.crt")
	keyPath := filepath.Join(tmpDir, "tls.key")

	if err := os.WriteFile(certPath, []byte("CERT_CONTENT"), 0644); err != nil {
		t.Fatalf("failed to write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("KEY_CONTENT"), 0600); err != nil {
		t.Fatalf("failed to write key: %v", err)
	}

	secret, err := sm.CreateTLSSecret("tls-secret", certPath, keyPath)
	if err != nil {
		t.Fatalf("failed to create TLS secret: %v", err)
	}

	if secret.Type != SecretTypeTLS {
		t.Errorf("expected type kubernetes.io/tls, got %s", secret.Type)
	}
	if string(secret.Data["tls.crt"]) != "CERT_CONTENT" {
		t.Error("expected tls.crt content")
	}
	if string(secret.Data["tls.key"]) != "KEY_CONTENT" {
		t.Error("expected tls.key content")
	}
}

func TestCreateDockerConfigSecret(t *testing.T) {
	sm := NewSecretManager("default", nil)

	registries := map[string]DockerRegistryAuth{
		"docker.io": {
			Username: "user",
			Password: "pass",
			Email:    "user@example.com",
		},
	}

	secret, err := sm.CreateDockerConfigSecret("docker-secret", registries)
	if err != nil {
		t.Fatalf("failed to create docker config secret: %v", err)
	}

	if secret.Type != SecretTypeDockerConfigJSON {
		t.Errorf("expected type dockerconfigjson, got %s", secret.Type)
	}
	if _, ok := secret.Data[".dockerconfigjson"]; !ok {
		t.Error("expected .dockerconfigjson key")
	}
}

func TestCreateBasicAuthSecret(t *testing.T) {
	sm := NewSecretManager("default", nil)

	secret, err := sm.CreateBasicAuthSecret("basic-auth", "admin", "password123")
	if err != nil {
		t.Fatalf("failed to create basic auth secret: %v", err)
	}

	if secret.Type != SecretTypeBasicAuth {
		t.Errorf("expected type basic-auth, got %s", secret.Type)
	}
	if string(secret.Data["username"]) != "admin" {
		t.Error("expected username")
	}
	if string(secret.Data["password"]) != "password123" {
		t.Error("expected password")
	}
}

func TestCreateSSHAuthSecret(t *testing.T) {
	sm := NewSecretManager("default", nil)

	secret, err := sm.CreateSSHAuthSecret("ssh-auth", "PRIVATE_KEY_CONTENT")
	if err != nil {
		t.Fatalf("failed to create SSH auth secret: %v", err)
	}

	if secret.Type != SecretTypeSSHAuth {
		t.Errorf("expected type ssh-auth, got %s", secret.Type)
	}
	if string(secret.Data["ssh-privatekey"]) != "PRIVATE_KEY_CONTENT" {
		t.Error("expected ssh-privatekey")
	}
}

func TestGetSecret(t *testing.T) {
	sm := NewSecretManager("default", nil)

	_, _ = sm.CreateSecret("test-secret", map[string]string{"key": "value"}, SecretTypeOpaque)

	secret, exists := sm.GetSecret("test-secret")
	if !exists {
		t.Error("expected secret to exist")
	}
	if secret.Name != "test-secret" {
		t.Errorf("expected name 'test-secret', got '%s'", secret.Name)
	}

	_, exists = sm.GetSecret("nonexistent")
	if exists {
		t.Error("expected secret to not exist")
	}
}

func TestUpdateSecret(t *testing.T) {
	sm := NewSecretManager("default", nil)

	secret, _ := sm.CreateSecret("test-secret", map[string]string{"key": "value"}, SecretTypeOpaque)
	oldVersion := secret.Version

	updated, err := sm.UpdateSecret("test-secret", map[string]string{"key": "new-value"})
	if err != nil {
		t.Fatalf("failed to update secret: %v", err)
	}

	if string(updated.Data["key"]) != "new-value" {
		t.Error("expected updated value")
	}
	if updated.Version != oldVersion+1 {
		t.Error("expected version to increment")
	}
}

func TestRotateSecret(t *testing.T) {
	sm := NewSecretManager("default", nil)

	secret, _ := sm.CreateSecret("test-secret", map[string]string{"key": "value"}, SecretTypeOpaque)
	oldVersion := secret.Version

	rotated, err := sm.RotateSecret("test-secret", map[string]string{"key": "rotated-value"})
	if err != nil {
		t.Fatalf("failed to rotate secret: %v", err)
	}

	if string(rotated.Data["key"]) != "rotated-value" {
		t.Error("expected rotated value")
	}
	if rotated.Version != oldVersion+1 {
		t.Error("expected version to increment")
	}
	if rotated.Annotations["vaultaire.io/previous-version"] == "" {
		t.Error("expected previous version annotation")
	}
	if rotated.Annotations["vaultaire.io/rotated-at"] == "" {
		t.Error("expected rotated-at annotation")
	}
}

func TestDeleteSecret(t *testing.T) {
	sm := NewSecretManager("default", nil)

	_, _ = sm.CreateSecret("test-secret", map[string]string{"key": "value"}, SecretTypeOpaque)

	err := sm.DeleteSecret("test-secret")
	if err != nil {
		t.Fatalf("failed to delete secret: %v", err)
	}

	_, exists := sm.GetSecret("test-secret")
	if exists {
		t.Error("expected secret to be deleted")
	}
}

func TestListSecrets(t *testing.T) {
	sm := NewSecretManager("default", nil)

	_, _ = sm.CreateSecret("secret-a", map[string]string{"a": "1"}, SecretTypeOpaque)
	_, _ = sm.CreateSecret("secret-b", map[string]string{"b": "2"}, SecretTypeOpaque)
	_, _ = sm.CreateSecret("secret-c", map[string]string{"c": "3"}, SecretTypeOpaque)

	secrets := sm.ListSecrets()

	if len(secrets) != 3 {
		t.Errorf("expected 3 secrets, got %d", len(secrets))
	}

	// Should be sorted
	if secrets[0].Name != "secret-a" {
		t.Error("expected secrets to be sorted")
	}
}

func TestSecretToManifest(t *testing.T) {
	sm := NewSecretManager("default", nil)

	secret, _ := sm.CreateSecret("test-secret", map[string]string{"key": "value"}, SecretTypeOpaque)

	manifest := secret.ToManifest()

	if manifest.Kind != KindSecret {
		t.Errorf("expected kind Secret, got %s", manifest.Kind)
	}
	if manifest.Type != string(SecretTypeOpaque) {
		t.Errorf("expected type Opaque, got %s", manifest.Type)
	}
	if manifest.Metadata.Annotations["vaultaire.io/hash"] == "" {
		t.Error("expected hash annotation")
	}
	if manifest.Metadata.Annotations["vaultaire.io/version"] == "" {
		t.Error("expected version annotation")
	}
}

func TestEncryptDecryptSecret(t *testing.T) {
	key, err := GenerateEncryptionKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	sm := NewSecretManager("default", key)

	_, _ = sm.CreateSecret("test-secret", map[string]string{
		"password": "super-secret",
	}, SecretTypeOpaque)

	// Encrypt
	if err := sm.EncryptSecret("test-secret"); err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	secret, _ := sm.GetSecret("test-secret")
	if !secret.Encrypted {
		t.Error("expected secret to be marked as encrypted")
	}

	// Decrypt
	decrypted, err := sm.DecryptSecret("test-secret")
	if err != nil {
		t.Fatalf("failed to decrypt: %v", err)
	}

	if string(decrypted["password"]) != "super-secret" {
		t.Errorf("expected 'super-secret', got '%s'", string(decrypted["password"]))
	}
}

func TestEncryptSecretNoKey(t *testing.T) {
	sm := NewSecretManager("default", nil)

	_, _ = sm.CreateSecret("test-secret", map[string]string{"key": "value"}, SecretTypeOpaque)

	err := sm.EncryptSecret("test-secret")
	if err == nil {
		t.Error("expected error when encrypting without key")
	}
}

func TestSealUnsealSecret(t *testing.T) {
	key, _ := GenerateEncryptionKey()
	sm := NewSecretManager("default", key)

	_, _ = sm.CreateSecret("test-secret", map[string]string{
		"api-key": "secret-api-key",
	}, SecretTypeOpaque)

	// Seal
	sealed, err := sm.SealSecret("test-secret")
	if err != nil {
		t.Fatalf("failed to seal: %v", err)
	}

	if sealed.Name != "test-secret" {
		t.Error("expected sealed secret name")
	}
	if len(sealed.EncryptedData) != 1 {
		t.Error("expected encrypted data")
	}

	// Delete original
	_ = sm.DeleteSecret("test-secret")

	// Unseal
	unsealed, err := sm.UnsealSecret(sealed)
	if err != nil {
		t.Fatalf("failed to unseal: %v", err)
	}

	if string(unsealed.Data["api-key"]) != "secret-api-key" {
		t.Error("expected unsealed data to match original")
	}
}

func TestSealedSecretToYAML(t *testing.T) {
	sealed := &SealedSecret{
		Name:      "test-secret",
		Namespace: "default",
		EncryptedData: map[string]string{
			"key": "encrypted-value",
		},
		Type:      SecretTypeOpaque,
		CreatedAt: time.Now(),
	}

	yaml, err := sealed.ToYAML()
	if err != nil {
		t.Fatalf("failed to convert to YAML: %v", err)
	}

	if yaml == "" {
		t.Error("expected non-empty YAML")
	}
}

func TestDeriveEncryptionKey(t *testing.T) {
	key := DeriveEncryptionKey("password123", "salt-value")

	if len(key) != 32 {
		t.Errorf("expected 32-byte key, got %d", len(key))
	}

	// Same inputs should produce same key
	key2 := DeriveEncryptionKey("password123", "salt-value")
	for i := range key {
		if key[i] != key2[i] {
			t.Error("expected deterministic key derivation")
			break
		}
	}

	// Different salt should produce different key
	key3 := DeriveEncryptionKey("password123", "different-salt")
	same := true
	for i := range key {
		if key[i] != key3[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("expected different salt to produce different key")
	}
}

func TestGenerateEncryptionKey(t *testing.T) {
	key, err := GenerateEncryptionKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	if len(key) != 32 {
		t.Errorf("expected 32-byte key, got %d", len(key))
	}

	// Should be random
	key2, _ := GenerateEncryptionKey()
	same := true
	for i := range key {
		if key[i] != key2[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("expected random keys to be different")
	}
}

func TestParseEnvFile(t *testing.T) {
	content := `
# This is a comment
DB_HOST=localhost
DB_PORT=5432
API_KEY="quoted-value"
SINGLE_QUOTE='single'
EMPTY=
SPACES_IN_VALUE=hello world
`

	result := parseEnvFile(content)

	tests := map[string]string{
		"DB_HOST":         "localhost",
		"DB_PORT":         "5432",
		"API_KEY":         "quoted-value",
		"SINGLE_QUOTE":    "single",
		"EMPTY":           "",
		"SPACES_IN_VALUE": "hello world",
	}

	for key, expected := range tests {
		if result[key] != expected {
			t.Errorf("expected %s='%s', got '%s'", key, expected, result[key])
		}
	}
}

func TestEnvSubstitution(t *testing.T) {
	// Set test env vars
	if err := os.Setenv("TEST_VAR", "test-value"); err != nil {
		t.Fatalf("failed to set env var: %v", err)
	}
	defer func() {
		_ = os.Unsetenv("TEST_VAR")
	}()

	data := map[string]string{
		"key1": "prefix-${TEST_VAR}-suffix",
		"key2": "${TEST_VAR}",
		"key3": "${NONEXISTENT}",
		"key4": "no-vars",
	}

	result := EnvSubstitution(data)

	if result["key1"] != "prefix-test-value-suffix" {
		t.Errorf("expected 'prefix-test-value-suffix', got '%s'", result["key1"])
	}
	if result["key2"] != "test-value" {
		t.Errorf("expected 'test-value', got '%s'", result["key2"])
	}
	if result["key3"] != "${NONEXISTENT}" {
		t.Errorf("expected '${NONEXISTENT}' (unchanged), got '%s'", result["key3"])
	}
	if result["key4"] != "no-vars" {
		t.Errorf("expected 'no-vars', got '%s'", result["key4"])
	}
}

func TestMergeConfigMaps(t *testing.T) {
	config1 := &ConfigMapData{
		Name:      "config1",
		Namespace: "default",
		Data:      map[string]string{"key1": "value1"},
		Labels:    map[string]string{"app": "test"},
	}

	config2 := &ConfigMapData{
		Name:      "config2",
		Namespace: "default",
		Data:      map[string]string{"key2": "value2", "key1": "overwritten"},
	}

	merged := MergeConfigMaps("merged", config1, config2)

	if merged.Name != "merged" {
		t.Errorf("expected name 'merged', got '%s'", merged.Name)
	}
	if merged.Data["key1"] != "overwritten" {
		t.Error("expected later value to override")
	}
	if merged.Data["key2"] != "value2" {
		t.Error("expected key2 from config2")
	}
	if merged.Labels["app"] != "test" {
		t.Error("expected labels to be merged")
	}
}

func TestMergeConfigMapsWithNil(t *testing.T) {
	config1 := &ConfigMapData{
		Name: "config1",
		Data: map[string]string{"key1": "value1"},
	}

	merged := MergeConfigMaps("merged", config1, nil)

	if merged.Data["key1"] != "value1" {
		t.Error("expected key1 from config1")
	}
}

func TestValidateSecretData(t *testing.T) {
	tests := []struct {
		name       string
		secretType SecretType
		data       map[string]string
		expectErr  bool
	}{
		{
			name:       "valid TLS",
			secretType: SecretTypeTLS,
			data:       map[string]string{"tls.crt": "cert", "tls.key": "key"},
			expectErr:  false,
		},
		{
			name:       "invalid TLS missing cert",
			secretType: SecretTypeTLS,
			data:       map[string]string{"tls.key": "key"},
			expectErr:  true,
		},
		{
			name:       "invalid TLS missing key",
			secretType: SecretTypeTLS,
			data:       map[string]string{"tls.crt": "cert"},
			expectErr:  true,
		},
		{
			name:       "valid docker config",
			secretType: SecretTypeDockerConfigJSON,
			data:       map[string]string{".dockerconfigjson": "{}"},
			expectErr:  false,
		},
		{
			name:       "invalid docker config",
			secretType: SecretTypeDockerConfigJSON,
			data:       map[string]string{},
			expectErr:  true,
		},
		{
			name:       "valid basic auth",
			secretType: SecretTypeBasicAuth,
			data:       map[string]string{"username": "user", "password": "pass"},
			expectErr:  false,
		},
		{
			name:       "invalid basic auth",
			secretType: SecretTypeBasicAuth,
			data:       map[string]string{"username": "user"},
			expectErr:  true,
		},
		{
			name:       "valid SSH auth",
			secretType: SecretTypeSSHAuth,
			data:       map[string]string{"ssh-privatekey": "key"},
			expectErr:  false,
		},
		{
			name:       "invalid SSH auth",
			secretType: SecretTypeSSHAuth,
			data:       map[string]string{},
			expectErr:  true,
		},
		{
			name:       "opaque no validation",
			secretType: SecretTypeOpaque,
			data:       map[string]string{},
			expectErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSecretData(tt.secretType, tt.data)
			if (err != nil) != tt.expectErr {
				t.Errorf("expected error: %v, got: %v", tt.expectErr, err)
			}
		})
	}
}

func TestConfigWatcher(t *testing.T) {
	cm := NewConfigMapManager("default")
	sm := NewSecretManager("default", nil)

	watcher := NewConfigWatcher(50 * time.Millisecond)

	changeDetected := make(chan string, 1)
	watcher.Watch("test-config", func(name, oldHash, newHash string) {
		changeDetected <- name
	})

	// Create initial config
	_, _ = cm.CreateConfigMap("test-config", map[string]string{"key": "value"})

	watcher.Start(cm, sm)
	defer watcher.Stop()

	// Wait for initial hash to be recorded
	time.Sleep(100 * time.Millisecond)

	// Update config
	_, _ = cm.UpdateConfigMap("test-config", map[string]string{"key": "new-value"})

	// Wait for change detection
	select {
	case name := <-changeDetected:
		if name != "test-config" {
			t.Errorf("expected 'test-config', got '%s'", name)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("timeout waiting for change detection")
	}
}

// Mock external secret source for testing
type mockExternalSource struct {
	secrets map[string]map[string][]byte
}

func (m *mockExternalSource) GetSecret(_ context.Context, path string) (map[string][]byte, error) {
	if data, ok := m.secrets[path]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("secret not found: %s", path)
}

func (m *mockExternalSource) ListSecrets(_ context.Context, prefix string) ([]string, error) {
	var result []string
	for k := range m.secrets {
		result = append(result, k)
	}
	return result, nil
}

func (m *mockExternalSource) Name() string {
	return "mock"
}

func TestFetchExternalSecret(t *testing.T) {
	sm := NewSecretManager("default", nil)

	mockSource := &mockExternalSource{
		secrets: map[string]map[string][]byte{
			"secret/data/myapp": {
				"username": []byte("admin"),
				"password": []byte("secret123"),
			},
		},
	}
	sm.SetExternalSource(mockSource)

	secret, err := sm.FetchExternalSecret(context.Background(), "myapp-secret", "secret/data/myapp", SecretTypeOpaque)
	if err != nil {
		t.Fatalf("failed to fetch external secret: %v", err)
	}

	if string(secret.Data["username"]) != "admin" {
		t.Error("expected username from external source")
	}
	if secret.Annotations["vaultaire.io/external-source"] != "mock" {
		t.Error("expected external source annotation")
	}
}

func TestFetchExternalSecretNoSource(t *testing.T) {
	sm := NewSecretManager("default", nil)

	_, err := sm.FetchExternalSecret(context.Background(), "test", "path", SecretTypeOpaque)
	if err == nil {
		t.Error("expected error when no external source configured")
	}
}

func TestCopyStringMap(t *testing.T) {
	original := map[string]string{"a": "1", "b": "2"}
	copied := copyStringMap(original)

	// Modify copy
	copied["c"] = "3"

	// Original should be unchanged
	if _, ok := original["c"]; ok {
		t.Error("original should not be modified")
	}
}

func TestCopyStringMapNil(t *testing.T) {
	result := copyStringMap(nil)
	if result != nil {
		t.Error("expected nil for nil input")
	}
}
