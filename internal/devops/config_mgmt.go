// internal/devops/config_mgmt.go
package devops

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Config types
const (
	ConfigTypeString   = "string"
	ConfigTypeInt      = "int"
	ConfigTypeBool     = "bool"
	ConfigTypeFloat    = "float"
	ConfigTypeDuration = "duration"
)

// ConfigEntry represents a configuration entry
type ConfigEntry struct {
	Value       string `json:"value"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Secret      bool   `json:"secret"`
}

// ConfigSpec defines a configuration specification
type ConfigSpec struct {
	Name    string                 `json:"name"`
	Version string                 `json:"version"`
	Entries map[string]ConfigEntry `json:"entries"`
}

// Validate checks the spec
func (s *ConfigSpec) Validate() error {
	if s.Name == "" {
		return errors.New("config: name is required")
	}
	return nil
}

// ConfigVersion tracks a config version
type ConfigVersion struct {
	Version   int       `json:"version"`
	Value     string    `json:"value"`
	Timestamp time.Time `json:"timestamp"`
}

// SchemaEntry defines validation rules
type SchemaEntry struct {
	Type     string      `json:"type"`
	Required bool        `json:"required"`
	Min      interface{} `json:"min,omitempty"`
	Max      interface{} `json:"max,omitempty"`
	Pattern  string      `json:"pattern,omitempty"`
}

// ConfigSchema maps keys to schema entries
type ConfigSchema map[string]SchemaEntry

// WatchCallback is called when config changes
type WatchCallback func(key, value string)

// ConfigManagerConfig configures the manager
type ConfigManagerConfig struct {
	EncryptSecrets bool
}

// ConfigManager manages configuration
type ConfigManager struct {
	config       *ConfigManagerConfig
	values       map[string]string
	secrets      map[string]string
	environments map[string]map[string]string
	history      map[string][]ConfigVersion
	schema       *ConfigSchema
	watchers     map[string][]WatchCallback
	mu           sync.RWMutex
}

// NewConfigManager creates a config manager
func NewConfigManager(config *ConfigManagerConfig) *ConfigManager {
	if config == nil {
		config = &ConfigManagerConfig{}
	}

	return &ConfigManager{
		config:       config,
		values:       make(map[string]string),
		secrets:      make(map[string]string),
		environments: make(map[string]map[string]string),
		history:      make(map[string][]ConfigVersion),
		watchers:     make(map[string][]WatchCallback),
	}
}

// Set sets a config value
func (m *ConfigManager) Set(key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.schema != nil {
		if err := m.validateValue(key, value); err != nil {
			return err
		}
	}

	m.addToHistory(key, value)
	m.values[key] = value
	m.notifyWatchers(key, value)

	return nil
}

// Get gets a config value
func (m *ConfigManager) Get(key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	value, exists := m.values[key]
	if !exists {
		return "", fmt.Errorf("config: key %s not found", key)
	}
	return value, nil
}

// GetWithDefault gets a value or returns default
func (m *ConfigManager) GetWithDefault(key, defaultValue string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if value, exists := m.values[key]; exists {
		return value
	}
	return defaultValue
}

// Delete removes a config value
func (m *ConfigManager) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.values, key)
	return nil
}

// Keys returns all config keys
func (m *ConfigManager) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	keys := make([]string, 0, len(m.values))
	for k := range m.values {
		keys = append(keys, k)
	}
	return keys
}

// Clear removes all config
func (m *ConfigManager) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.values = make(map[string]string)
	m.history = make(map[string][]ConfigVersion)
}

// CreateEnvironment creates an environment
func (m *ConfigManager) CreateEnvironment(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.environments[name] = make(map[string]string)
	return nil
}

// SetForEnv sets a value for an environment
func (m *ConfigManager) SetForEnv(env, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.environments[env]; !exists {
		return fmt.Errorf("config: environment %s not found", env)
	}

	m.environments[env][key] = value
	return nil
}

// GetForEnv gets a value for an environment
func (m *ConfigManager) GetForEnv(env, key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if envValues, exists := m.environments[env]; exists {
		if value, exists := envValues[key]; exists {
			return value, nil
		}
	}

	// Fall back to default
	if value, exists := m.values[key]; exists {
		return value, nil
	}

	return "", fmt.Errorf("config: key %s not found in env %s", key, env)
}

// SetSecret sets a secret value
func (m *ConfigManager) SetSecret(key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.secrets[key] = value
	return nil
}

// GetSecret gets a secret value
func (m *ConfigManager) GetSecret(key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	value, exists := m.secrets[key]
	if !exists {
		return "", fmt.Errorf("config: secret %s not found", key)
	}
	return value, nil
}

// History returns version history for a key
func (m *ConfigManager) History(key string) []ConfigVersion {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if history, exists := m.history[key]; exists {
		return history
	}
	return nil
}

// Rollback rolls back to a specific version
func (m *ConfigManager) Rollback(key string, version int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	history, exists := m.history[key]
	if !exists {
		return fmt.Errorf("config: no history for key %s", key)
	}

	for _, v := range history {
		if v.Version == version {
			m.values[key] = v.Value
			m.addToHistory(key, v.Value)
			return nil
		}
	}

	return fmt.Errorf("config: version %d not found for key %s", version, key)
}

func (m *ConfigManager) addToHistory(key, value string) {
	version := 1
	if existing, exists := m.history[key]; exists {
		version = len(existing) + 1
	}

	m.history[key] = append(m.history[key], ConfigVersion{
		Version:   version,
		Value:     value,
		Timestamp: time.Now(),
	})
}

// SetSchema sets the validation schema
func (m *ConfigManager) SetSchema(schema *ConfigSchema) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.schema = schema
}

func (m *ConfigManager) validateValue(key, value string) error {
	if m.schema == nil {
		return nil
	}

	entry, exists := (*m.schema)[key]
	if !exists {
		return nil
	}

	switch entry.Type {
	case ConfigTypeInt:
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("config: %s must be an integer", key)
		}
		if entry.Min != nil {
			if min, ok := entry.Min.(int); ok && v < min {
				return fmt.Errorf("config: %s must be >= %d", key, min)
			}
		}
		if entry.Max != nil {
			if max, ok := entry.Max.(int); ok && v > max {
				return fmt.Errorf("config: %s must be <= %d", key, max)
			}
		}
	case ConfigTypeBool:
		if value != "true" && value != "false" {
			return fmt.Errorf("config: %s must be a boolean", key)
		}
	case ConfigTypeFloat:
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return fmt.Errorf("config: %s must be a float", key)
		}
	}

	return nil
}

// Watch registers a callback for config changes
func (m *ConfigManager) Watch(key string, callback WatchCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.watchers[key] = append(m.watchers[key], callback)
}

func (m *ConfigManager) notifyWatchers(key, value string) {
	if callbacks, exists := m.watchers[key]; exists {
		for _, cb := range callbacks {
			go cb(key, value)
		}
	}
}

// ImportJSON imports config from JSON
func (m *ConfigManager) ImportJSON(data string) error {
	var nested map[string]interface{}
	if err := json.Unmarshal([]byte(data), &nested); err != nil {
		return fmt.Errorf("config: invalid JSON: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.flattenMap("", nested)
	return nil
}

// ImportYAML imports config from YAML
func (m *ConfigManager) ImportYAML(data string) error {
	var nested map[string]interface{}
	if err := yaml.Unmarshal([]byte(data), &nested); err != nil {
		return fmt.Errorf("config: invalid YAML: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.flattenMap("", nested)
	return nil
}

func (m *ConfigManager) flattenMap(prefix string, data map[string]interface{}) {
	for k, v := range data {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}

		switch val := v.(type) {
		case map[string]interface{}:
			m.flattenMap(key, val)
		case string:
			m.values[key] = val
		default:
			m.values[key] = fmt.Sprintf("%v", val)
		}
	}
}

// ImportEnv imports config from environment variables
func (m *ConfigManager) ImportEnv(prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key, value := parts[0], parts[1]
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		// Convert APP_DATABASE_HOST to database.host
		configKey := strings.TrimPrefix(key, prefix)
		configKey = strings.ToLower(configKey)
		configKey = strings.ReplaceAll(configKey, "_", ".")

		m.values[configKey] = value
	}

	return nil
}

// Export exports config as string
func (m *ConfigManager) Export(includeSecrets bool) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var sb strings.Builder

	for k, v := range m.values {
		sb.WriteString(fmt.Sprintf("%s=%s\n", k, v))
	}

	for k := range m.secrets {
		if includeSecrets {
			sb.WriteString(fmt.Sprintf("%s=%s\n", k, m.secrets[k]))
		} else {
			sb.WriteString(fmt.Sprintf("%s=***\n", k))
		}
	}

	return sb.String()
}

// ExportJSON exports config as JSON
func (m *ConfigManager) ExportJSON() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Build nested structure
	nested := m.buildNestedMap()

	data, err := json.MarshalIndent(nested, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ExportYAML exports config as YAML
func (m *ConfigManager) ExportYAML() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	nested := m.buildNestedMap()

	data, err := yaml.Marshal(nested)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (m *ConfigManager) buildNestedMap() map[string]interface{} {
	nested := make(map[string]interface{})

	for k, v := range m.values {
		parts := strings.Split(k, ".")
		current := nested

		for i, part := range parts {
			if i == len(parts)-1 {
				current[part] = v
			} else {
				if _, exists := current[part]; !exists {
					current[part] = make(map[string]interface{})
				}
				current = current[part].(map[string]interface{})
			}
		}
	}

	return nested
}
