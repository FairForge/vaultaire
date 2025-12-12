// internal/k8s/configsecrets.go
package k8s

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/pbkdf2"
	"gopkg.in/yaml.v3"
)

// ConfigMapManager handles ConfigMap operations
type ConfigMapManager struct {
	namespace string
	labels    map[string]string
	mu        sync.RWMutex
	configs   map[string]*ConfigMapData
}

// ConfigMapData represents ConfigMap content
type ConfigMapData struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
	Data        map[string]string
	BinaryData  map[string][]byte
	Immutable   bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Hash        string
}

// SecretManager handles Secret operations
type SecretManager struct {
	namespace      string
	labels         map[string]string
	encryptionKey  []byte
	mu             sync.RWMutex
	secrets        map[string]*SecretData
	externalSource ExternalSecretSource
}

// SecretData represents Secret content
type SecretData struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
	Type        SecretType
	Data        map[string][]byte
	StringData  map[string]string
	Immutable   bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Hash        string
	Encrypted   bool
	Version     int
}

// SecretType represents Kubernetes secret types
type SecretType string

const (
	SecretTypeOpaque              SecretType = "Opaque"
	SecretTypeServiceAccountToken SecretType = "kubernetes.io/service-account-token"
	SecretTypeDockerConfigJSON    SecretType = "kubernetes.io/dockerconfigjson"
	SecretTypeTLS                 SecretType = "kubernetes.io/tls"
	SecretTypeBasicAuth           SecretType = "kubernetes.io/basic-auth"
	SecretTypeSSHAuth             SecretType = "kubernetes.io/ssh-auth"
	SecretTypeBootstrapToken      SecretType = "bootstrap.kubernetes.io/token"
)

// ExternalSecretSource interface for external secret providers
type ExternalSecretSource interface {
	GetSecret(ctx context.Context, path string) (map[string][]byte, error)
	ListSecrets(ctx context.Context, prefix string) ([]string, error)
	Name() string
}

// NewConfigMapManager creates a new ConfigMap manager
func NewConfigMapManager(namespace string) *ConfigMapManager {
	return &ConfigMapManager{
		namespace: namespace,
		labels: map[string]string{
			"app.kubernetes.io/managed-by": "vaultaire",
		},
		configs: make(map[string]*ConfigMapData),
	}
}

// NewSecretManager creates a new Secret manager
func NewSecretManager(namespace string, encryptionKey []byte) *SecretManager {
	return &SecretManager{
		namespace:     namespace,
		encryptionKey: encryptionKey,
		labels: map[string]string{
			"app.kubernetes.io/managed-by": "vaultaire",
		},
		secrets: make(map[string]*SecretData),
	}
}

// SetExternalSource sets an external secret source
func (sm *SecretManager) SetExternalSource(source ExternalSecretSource) {
	sm.externalSource = source
}

// CreateConfigMap creates a new ConfigMap
func (cm *ConfigMapManager) CreateConfigMap(name string, data map[string]string) (*ConfigMapData, error) {
	if name == "" {
		return nil, fmt.Errorf("configmap name is required")
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	now := time.Now()
	configMap := &ConfigMapData{
		Name:        name,
		Namespace:   cm.namespace,
		Labels:      copyStringMap(cm.labels),
		Annotations: make(map[string]string),
		Data:        copyStringMap(data),
		BinaryData:  make(map[string][]byte),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	configMap.Hash = configMap.computeHash()

	cm.configs[name] = configMap
	return configMap, nil
}

// CreateConfigMapFromFile creates a ConfigMap from a file
func (cm *ConfigMapManager) CreateConfigMapFromFile(name, key, filePath string) (*ConfigMapData, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", filePath, err)
	}

	if key == "" {
		key = filepath.Base(filePath)
	}

	return cm.CreateConfigMap(name, map[string]string{key: string(content)})
}

// CreateConfigMapFromDirectory creates a ConfigMap from all files in a directory
func (cm *ConfigMapManager) CreateConfigMapFromDirectory(name, dirPath string) (*ConfigMapData, error) {
	data := make(map[string]string)

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory %s: %w", dirPath, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filePath := filepath.Join(dirPath, entry.Name())
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read file %s: %w", filePath, err)
		}

		data[entry.Name()] = string(content)
	}

	return cm.CreateConfigMap(name, data)
}

// CreateConfigMapFromEnvFile creates a ConfigMap from a .env file
func (cm *ConfigMapManager) CreateConfigMapFromEnvFile(name, envFilePath string) (*ConfigMapData, error) {
	content, err := os.ReadFile(envFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read env file %s: %w", envFilePath, err)
	}

	data := parseEnvFile(string(content))
	return cm.CreateConfigMap(name, data)
}

// GetConfigMap retrieves a ConfigMap by name
func (cm *ConfigMapManager) GetConfigMap(name string) (*ConfigMapData, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	config, exists := cm.configs[name]
	return config, exists
}

// UpdateConfigMap updates an existing ConfigMap
func (cm *ConfigMapManager) UpdateConfigMap(name string, data map[string]string) (*ConfigMapData, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	config, exists := cm.configs[name]
	if !exists {
		return nil, fmt.Errorf("configmap %s not found", name)
	}

	if config.Immutable {
		return nil, fmt.Errorf("configmap %s is immutable", name)
	}

	config.Data = copyStringMap(data)
	config.UpdatedAt = time.Now()
	config.Hash = config.computeHash()

	return config, nil
}

// DeleteConfigMap deletes a ConfigMap
func (cm *ConfigMapManager) DeleteConfigMap(name string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if _, exists := cm.configs[name]; !exists {
		return fmt.Errorf("configmap %s not found", name)
	}

	delete(cm.configs, name)
	return nil
}

// ListConfigMaps lists all ConfigMaps
func (cm *ConfigMapManager) ListConfigMaps() []*ConfigMapData {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	result := make([]*ConfigMapData, 0, len(cm.configs))
	for _, config := range cm.configs {
		result = append(result, config)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result
}

// computeHash computes a hash of ConfigMap data
func (c *ConfigMapData) computeHash() string {
	h := sha256.New()

	// Sort keys for deterministic hashing
	keys := make([]string, 0, len(c.Data))
	for k := range c.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte(c.Data[k]))
	}

	return base64.StdEncoding.EncodeToString(h.Sum(nil))[:16]
}

// ToManifest converts ConfigMapData to a Kubernetes manifest
func (c *ConfigMapData) ToManifest() *Manifest {
	annotations := copyStringMap(c.Annotations)
	annotations["vaultaire.io/hash"] = c.Hash

	return &Manifest{
		APIVersion: "v1",
		Kind:       KindConfigMap,
		Metadata: ManifestMetadata{
			Name:        c.Name,
			Namespace:   c.Namespace,
			Labels:      c.Labels,
			Annotations: annotations,
		},
		Data: c.Data,
	}
}

// CreateSecret creates a new Secret
func (sm *SecretManager) CreateSecret(name string, data map[string]string, secretType SecretType) (*SecretData, error) {
	if name == "" {
		return nil, fmt.Errorf("secret name is required")
	}

	if secretType == "" {
		secretType = SecretTypeOpaque
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	secret := &SecretData{
		Name:        name,
		Namespace:   sm.namespace,
		Labels:      copyStringMap(sm.labels),
		Annotations: make(map[string]string),
		Type:        secretType,
		Data:        make(map[string][]byte),
		StringData:  copyStringMap(data),
		CreatedAt:   now,
		UpdatedAt:   now,
		Version:     1,
	}

	// Convert string data to byte data
	for k, v := range data {
		secret.Data[k] = []byte(v)
	}

	secret.Hash = secret.computeHash()
	sm.secrets[name] = secret

	return secret, nil
}

// CreateSecretFromFile creates a Secret from a file
func (sm *SecretManager) CreateSecretFromFile(name, key, filePath string, secretType SecretType) (*SecretData, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", filePath, err)
	}

	if key == "" {
		key = filepath.Base(filePath)
	}

	return sm.CreateSecret(name, map[string]string{key: string(content)}, secretType)
}

// CreateTLSSecret creates a TLS secret from cert and key files
func (sm *SecretManager) CreateTLSSecret(name, certPath, keyPath string) (*SecretData, error) {
	cert, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read cert file: %w", err)
	}

	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file: %w", err)
	}

	return sm.CreateSecret(name, map[string]string{
		"tls.crt": string(cert),
		"tls.key": string(key),
	}, SecretTypeTLS)
}

// CreateDockerConfigSecret creates a Docker config secret
func (sm *SecretManager) CreateDockerConfigSecret(name string, registries map[string]DockerRegistryAuth) (*SecretData, error) {
	auths := make(map[string]interface{})
	for registry, auth := range registries {
		auths[registry] = map[string]string{
			"username": auth.Username,
			"password": auth.Password,
			"email":    auth.Email,
			"auth":     base64.StdEncoding.EncodeToString([]byte(auth.Username + ":" + auth.Password)),
		}
	}

	dockerConfig := map[string]interface{}{
		"auths": auths,
	}

	configJSON, err := json.Marshal(dockerConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal docker config: %w", err)
	}

	return sm.CreateSecret(name, map[string]string{
		".dockerconfigjson": string(configJSON),
	}, SecretTypeDockerConfigJSON)
}

// DockerRegistryAuth contains Docker registry credentials
type DockerRegistryAuth struct {
	Username string
	Password string
	Email    string
}

// CreateBasicAuthSecret creates a basic auth secret
func (sm *SecretManager) CreateBasicAuthSecret(name, username, password string) (*SecretData, error) {
	return sm.CreateSecret(name, map[string]string{
		"username": username,
		"password": password,
	}, SecretTypeBasicAuth)
}

// CreateSSHAuthSecret creates an SSH auth secret
func (sm *SecretManager) CreateSSHAuthSecret(name, privateKey string) (*SecretData, error) {
	return sm.CreateSecret(name, map[string]string{
		"ssh-privatekey": privateKey,
	}, SecretTypeSSHAuth)
}

// GetSecret retrieves a Secret by name
func (sm *SecretManager) GetSecret(name string) (*SecretData, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	secret, exists := sm.secrets[name]
	return secret, exists
}

// UpdateSecret updates an existing Secret
func (sm *SecretManager) UpdateSecret(name string, data map[string]string) (*SecretData, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	secret, exists := sm.secrets[name]
	if !exists {
		return nil, fmt.Errorf("secret %s not found", name)
	}

	if secret.Immutable {
		return nil, fmt.Errorf("secret %s is immutable", name)
	}

	secret.StringData = copyStringMap(data)
	secret.Data = make(map[string][]byte)
	for k, v := range data {
		secret.Data[k] = []byte(v)
	}
	secret.UpdatedAt = time.Now()
	secret.Version++
	secret.Hash = secret.computeHash()

	return secret, nil
}

// RotateSecret rotates a secret with new data
func (sm *SecretManager) RotateSecret(name string, newData map[string]string) (*SecretData, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	secret, exists := sm.secrets[name]
	if !exists {
		return nil, fmt.Errorf("secret %s not found", name)
	}

	// Store previous version info in annotations
	if secret.Annotations == nil {
		secret.Annotations = make(map[string]string)
	}
	secret.Annotations["vaultaire.io/previous-version"] = fmt.Sprintf("%d", secret.Version)
	secret.Annotations["vaultaire.io/rotated-at"] = time.Now().Format(time.RFC3339)

	secret.StringData = copyStringMap(newData)
	secret.Data = make(map[string][]byte)
	for k, v := range newData {
		secret.Data[k] = []byte(v)
	}
	secret.UpdatedAt = time.Now()
	secret.Version++
	secret.Hash = secret.computeHash()

	return secret, nil
}

// DeleteSecret deletes a Secret
func (sm *SecretManager) DeleteSecret(name string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, exists := sm.secrets[name]; !exists {
		return fmt.Errorf("secret %s not found", name)
	}

	delete(sm.secrets, name)
	return nil
}

// ListSecrets lists all Secrets
func (sm *SecretManager) ListSecrets() []*SecretData {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	result := make([]*SecretData, 0, len(sm.secrets))
	for _, secret := range sm.secrets {
		result = append(result, secret)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result
}

// computeHash computes a hash of Secret data
func (s *SecretData) computeHash() string {
	h := sha256.New()

	// Sort keys for deterministic hashing
	keys := make([]string, 0, len(s.Data))
	for k := range s.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		h.Write([]byte(k))
		h.Write(s.Data[k])
	}

	return base64.StdEncoding.EncodeToString(h.Sum(nil))[:16]
}

// ToManifest converts SecretData to a Kubernetes manifest
func (s *SecretData) ToManifest() *Manifest {
	annotations := copyStringMap(s.Annotations)
	annotations["vaultaire.io/hash"] = s.Hash
	annotations["vaultaire.io/version"] = fmt.Sprintf("%d", s.Version)

	// Encode data to base64
	encodedData := make(map[string]string)
	for k, v := range s.Data {
		encodedData[k] = base64.StdEncoding.EncodeToString(v)
	}

	manifest := &Manifest{
		APIVersion: "v1",
		Kind:       KindSecret,
		Metadata: ManifestMetadata{
			Name:        s.Name,
			Namespace:   s.Namespace,
			Labels:      s.Labels,
			Annotations: annotations,
		},
		Type: string(s.Type),
	}

	// Use StringData for YAML output (more readable)
	manifest.StringData = s.StringData

	return manifest
}

// EncryptSecret encrypts secret data using AES-GCM
func (sm *SecretManager) EncryptSecret(name string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	secret, exists := sm.secrets[name]
	if !exists {
		return fmt.Errorf("secret %s not found", name)
	}

	if secret.Encrypted {
		return nil // Already encrypted
	}

	if sm.encryptionKey == nil {
		return fmt.Errorf("encryption key not set")
	}

	for k, v := range secret.Data {
		encrypted, err := encrypt(v, sm.encryptionKey)
		if err != nil {
			return fmt.Errorf("failed to encrypt key %s: %w", k, err)
		}
		secret.Data[k] = encrypted
	}

	secret.Encrypted = true
	secret.UpdatedAt = time.Now()

	return nil
}

// DecryptSecret decrypts secret data
func (sm *SecretManager) DecryptSecret(name string) (map[string][]byte, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	secret, exists := sm.secrets[name]
	if !exists {
		return nil, fmt.Errorf("secret %s not found", name)
	}

	if !secret.Encrypted {
		return secret.Data, nil
	}

	if sm.encryptionKey == nil {
		return nil, fmt.Errorf("encryption key not set")
	}

	decrypted := make(map[string][]byte)
	for k, v := range secret.Data {
		plaintext, err := decrypt(v, sm.encryptionKey)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt key %s: %w", k, err)
		}
		decrypted[k] = plaintext
	}

	return decrypted, nil
}

// SealedSecret represents an encrypted/sealed secret for GitOps
type SealedSecret struct {
	Name          string            `yaml:"name"`
	Namespace     string            `yaml:"namespace"`
	EncryptedData map[string]string `yaml:"encryptedData"`
	Type          SecretType        `yaml:"type"`
	CreatedAt     time.Time         `yaml:"createdAt"`
}

// SealSecret creates a sealed secret for GitOps workflows
func (sm *SecretManager) SealSecret(name string) (*SealedSecret, error) {
	sm.mu.RLock()
	secret, exists := sm.secrets[name]
	sm.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("secret %s not found", name)
	}

	if sm.encryptionKey == nil {
		return nil, fmt.Errorf("encryption key not set")
	}

	sealed := &SealedSecret{
		Name:          secret.Name,
		Namespace:     secret.Namespace,
		EncryptedData: make(map[string]string),
		Type:          secret.Type,
		CreatedAt:     time.Now(),
	}

	for k, v := range secret.Data {
		encrypted, err := encrypt(v, sm.encryptionKey)
		if err != nil {
			return nil, fmt.Errorf("failed to seal key %s: %w", k, err)
		}
		sealed.EncryptedData[k] = base64.StdEncoding.EncodeToString(encrypted)
	}

	return sealed, nil
}

// UnsealSecret unseals a sealed secret
func (sm *SecretManager) UnsealSecret(sealed *SealedSecret) (*SecretData, error) {
	if sm.encryptionKey == nil {
		return nil, fmt.Errorf("encryption key not set")
	}

	data := make(map[string]string)
	for k, v := range sealed.EncryptedData {
		encryptedBytes, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("failed to decode key %s: %w", k, err)
		}

		decrypted, err := decrypt(encryptedBytes, sm.encryptionKey)
		if err != nil {
			return nil, fmt.Errorf("failed to unseal key %s: %w", k, err)
		}
		data[k] = string(decrypted)
	}

	return sm.CreateSecret(sealed.Name, data, sealed.Type)
}

// ToYAML converts SealedSecret to YAML
func (ss *SealedSecret) ToYAML() (string, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(ss); err != nil {
		return "", fmt.Errorf("failed to encode sealed secret: %w", err)
	}
	return buf.String(), nil
}

// FetchExternalSecret fetches a secret from an external source
func (sm *SecretManager) FetchExternalSecret(ctx context.Context, name, path string, secretType SecretType) (*SecretData, error) {
	if sm.externalSource == nil {
		return nil, fmt.Errorf("no external secret source configured")
	}

	data, err := sm.externalSource.GetSecret(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch external secret: %w", err)
	}

	stringData := make(map[string]string)
	for k, v := range data {
		stringData[k] = string(v)
	}

	secret, err := sm.CreateSecret(name, stringData, secretType)
	if err != nil {
		return nil, err
	}

	secret.Annotations["vaultaire.io/external-source"] = sm.externalSource.Name()
	secret.Annotations["vaultaire.io/external-path"] = path

	return secret, nil
}

// ConfigWatcher watches for ConfigMap/Secret changes
type ConfigWatcher struct {
	mu        sync.RWMutex
	callbacks map[string][]ConfigChangeCallback
	interval  time.Duration
	stopCh    chan struct{}
	running   bool
}

// ConfigChangeCallback is called when a config changes
type ConfigChangeCallback func(name string, oldHash, newHash string)

// NewConfigWatcher creates a new config watcher
func NewConfigWatcher(interval time.Duration) *ConfigWatcher {
	return &ConfigWatcher{
		callbacks: make(map[string][]ConfigChangeCallback),
		interval:  interval,
		stopCh:    make(chan struct{}),
	}
}

// Watch registers a callback for config changes
func (cw *ConfigWatcher) Watch(name string, callback ConfigChangeCallback) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	cw.callbacks[name] = append(cw.callbacks[name], callback)
}

// Start starts the config watcher
func (cw *ConfigWatcher) Start(cm *ConfigMapManager, sm *SecretManager) {
	cw.mu.Lock()
	if cw.running {
		cw.mu.Unlock()
		return
	}
	cw.running = true
	cw.mu.Unlock()

	hashes := make(map[string]string)

	go func() {
		ticker := time.NewTicker(cw.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				cw.checkChanges(cm, sm, hashes)
			case <-cw.stopCh:
				return
			}
		}
	}()
}

// Stop stops the config watcher
func (cw *ConfigWatcher) Stop() {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	if cw.running {
		close(cw.stopCh)
		cw.running = false
	}
}

func (cw *ConfigWatcher) checkChanges(cm *ConfigMapManager, sm *SecretManager, hashes map[string]string) {
	cw.mu.RLock()
	defer cw.mu.RUnlock()

	// Check ConfigMaps
	for _, config := range cm.ListConfigMaps() {
		key := "configmap:" + config.Name
		if oldHash, exists := hashes[key]; exists && oldHash != config.Hash {
			if callbacks, ok := cw.callbacks[config.Name]; ok {
				for _, cb := range callbacks {
					cb(config.Name, oldHash, config.Hash)
				}
			}
		}
		hashes[key] = config.Hash
	}

	// Check Secrets
	for _, secret := range sm.ListSecrets() {
		key := "secret:" + secret.Name
		if oldHash, exists := hashes[key]; exists && oldHash != secret.Hash {
			if callbacks, ok := cw.callbacks[secret.Name]; ok {
				for _, cb := range callbacks {
					cb(secret.Name, oldHash, secret.Hash)
				}
			}
		}
		hashes[key] = secret.Hash
	}
}

// DeriveEncryptionKey derives an encryption key from a password
func DeriveEncryptionKey(password, salt string) []byte {
	return pbkdf2.Key([]byte(password), []byte(salt), 100000, 32, sha256.New)
}

// GenerateEncryptionKey generates a random encryption key
func GenerateEncryptionKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("failed to generate key: %w", err)
	}
	return key, nil
}

// encrypt encrypts data using AES-GCM
func encrypt(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt decrypts data using AES-GCM
func decrypt(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	if len(ciphertext) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

// parseEnvFile parses a .env file into a map
func parseEnvFile(content string) map[string]string {
	result := make(map[string]string)
	lines := strings.Split(content, "\n")

	envRegex := regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		matches := envRegex.FindStringSubmatch(line)
		if len(matches) == 3 {
			key := matches[1]
			value := matches[2]

			// Remove surrounding quotes if present
			if len(value) >= 2 {
				if (value[0] == '"' && value[len(value)-1] == '"') ||
					(value[0] == '\'' && value[len(value)-1] == '\'') {
					value = value[1 : len(value)-1]
				}
			}

			result[key] = value
		}
	}

	return result
}

// copyStringMap creates a copy of a string map
func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	result := make(map[string]string, len(m))
	for k, v := range m {
		result[k] = v
	}
	return result
}

// EnvSubstitution replaces environment variable references in values
func EnvSubstitution(data map[string]string) map[string]string {
	result := make(map[string]string, len(data))
	envRegex := regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

	for k, v := range data {
		result[k] = envRegex.ReplaceAllStringFunc(v, func(match string) string {
			varName := match[2 : len(match)-1]
			if envValue := os.Getenv(varName); envValue != "" {
				return envValue
			}
			return match // Keep original if not found
		})
	}

	return result
}

// MergeConfigMaps merges multiple ConfigMaps into one
func MergeConfigMaps(name string, configs ...*ConfigMapData) *ConfigMapData {
	merged := &ConfigMapData{
		Name:        name,
		Data:        make(map[string]string),
		BinaryData:  make(map[string][]byte),
		Labels:      make(map[string]string),
		Annotations: make(map[string]string),
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	for _, config := range configs {
		if config == nil {
			continue
		}

		for k, v := range config.Data {
			merged.Data[k] = v
		}
		for k, v := range config.BinaryData {
			merged.BinaryData[k] = v
		}
		for k, v := range config.Labels {
			merged.Labels[k] = v
		}

		if merged.Namespace == "" {
			merged.Namespace = config.Namespace
		}
	}

	merged.Hash = merged.computeHash()
	return merged
}

// ValidateSecretData validates secret data based on type
func ValidateSecretData(secretType SecretType, data map[string]string) error {
	switch secretType {
	case SecretTypeTLS:
		if _, ok := data["tls.crt"]; !ok {
			return fmt.Errorf("TLS secret requires 'tls.crt' key")
		}
		if _, ok := data["tls.key"]; !ok {
			return fmt.Errorf("TLS secret requires 'tls.key' key")
		}
	case SecretTypeDockerConfigJSON:
		if _, ok := data[".dockerconfigjson"]; !ok {
			return fmt.Errorf("docker config secret requires '.dockerconfigjson' key")
		}
	case SecretTypeBasicAuth:
		if _, ok := data["username"]; !ok {
			return fmt.Errorf("basic auth secret requires 'username' key")
		}
		if _, ok := data["password"]; !ok {
			return fmt.Errorf("basic auth secret requires 'password' key")
		}
	case SecretTypeSSHAuth:
		if _, ok := data["ssh-privatekey"]; !ok {
			return fmt.Errorf("SSH auth secret requires 'ssh-privatekey' key")
		}
	}
	return nil
}
