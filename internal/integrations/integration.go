// internal/integrations/integration.go
package integrations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Integration types
const (
	IntegrationTypeWebhook = "webhook"
	IntegrationTypeLambda  = "lambda"
	IntegrationTypeSNS     = "sns"
	IntegrationTypeSQS     = "sqs"
	IntegrationTypeCustom  = "custom"
)

// Auth types
const (
	AuthTypeNone   = "none"
	AuthTypeBasic  = "basic"
	AuthTypeBearer = "bearer"
	AuthTypeAPIKey = "api_key"
)

// IntegrationConfig configures an integration
type IntegrationConfig struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	TenantID   string            `json:"tenant_id"`
	Endpoint   string            `json:"endpoint"`
	Enabled    bool              `json:"enabled"`
	EventTypes []string          `json:"event_types"`
	Auth       *AuthConfig       `json:"auth,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// Validate checks configuration
func (c *IntegrationConfig) Validate() error {
	if c.ID == "" {
		return errors.New("integration: id is required")
	}
	if c.Name == "" {
		return errors.New("integration: name is required")
	}

	validTypes := map[string]bool{
		IntegrationTypeWebhook: true,
		IntegrationTypeLambda:  true,
		IntegrationTypeSNS:     true,
		IntegrationTypeSQS:     true,
		IntegrationTypeCustom:  true,
	}
	if !validTypes[c.Type] {
		return fmt.Errorf("integration: invalid type: %s", c.Type)
	}

	return nil
}

// AuthConfig configures authentication
type AuthConfig struct {
	Type     string `json:"type"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	Header   string `json:"header,omitempty"`
}

// IntegrationEvent represents an event to send
type IntegrationEvent struct {
	Type      string                 `json:"type"`
	Timestamp time.Time              `json:"timestamp"`
	Payload   map[string]interface{} `json:"payload"`
}

// NewS3Event creates an S3-style event
func NewS3Event(eventType, bucket, key string, size int64) *IntegrationEvent {
	return &IntegrationEvent{
		Type:      "s3:" + eventType,
		Timestamp: time.Now().UTC(),
		Payload: map[string]interface{}{
			"bucket": bucket,
			"key":    key,
			"size":   size,
		},
	}
}

// IntegrationStatus tracks integration health
type IntegrationStatus struct {
	LastSuccess  time.Time `json:"last_success"`
	LastFailure  time.Time `json:"last_failure"`
	SuccessCount int64     `json:"success_count"`
	FailureCount int64     `json:"failure_count"`
	LastError    string    `json:"last_error,omitempty"`
}

// ManagerConfig configures the integration manager
type ManagerConfig struct {
	MaxRetries int
	RetryDelay time.Duration
	Timeout    time.Duration
}

// IntegrationHandler is a custom handler function
type IntegrationHandler func(ctx context.Context, event *IntegrationEvent) error

// IntegrationManager manages integrations
type IntegrationManager struct {
	config       *ManagerConfig
	integrations map[string]*IntegrationConfig
	handlers     map[string]IntegrationHandler
	statuses     map[string]*IntegrationStatus
	client       *http.Client
	mu           sync.RWMutex
}

// NewIntegrationManager creates an integration manager
func NewIntegrationManager(config *ManagerConfig) *IntegrationManager {
	if config == nil {
		config = &ManagerConfig{
			MaxRetries: 3,
			RetryDelay: time.Second,
			Timeout:    30 * time.Second,
		}
	}

	return &IntegrationManager{
		config:       config,
		integrations: make(map[string]*IntegrationConfig),
		handlers:     make(map[string]IntegrationHandler),
		statuses:     make(map[string]*IntegrationStatus),
		client:       &http.Client{Timeout: config.Timeout},
	}
}

// Register registers an integration
func (m *IntegrationManager) Register(config *IntegrationConfig) error {
	if err := config.Validate(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.integrations[config.ID]; exists {
		return fmt.Errorf("integration: %s already exists", config.ID)
	}

	m.integrations[config.ID] = config
	m.statuses[config.ID] = &IntegrationStatus{}
	return nil
}

// Unregister removes an integration
func (m *IntegrationManager) Unregister(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.integrations[id]; !exists {
		return fmt.Errorf("integration: %s not found", id)
	}

	delete(m.integrations, id)
	delete(m.handlers, id)
	delete(m.statuses, id)
	return nil
}

// Get returns an integration by ID
func (m *IntegrationManager) Get(id string) *IntegrationConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.integrations[id]
}

// ListByTenant returns integrations for a tenant
func (m *IntegrationManager) ListByTenant(tenantID string) []*IntegrationConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*IntegrationConfig
	for _, config := range m.integrations {
		if config.TenantID == tenantID {
			result = append(result, config)
		}
	}
	return result
}

// SetHandler sets a custom handler for an integration
func (m *IntegrationManager) SetHandler(id string, handler IntegrationHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[id] = handler
}

// Trigger triggers an integration
func (m *IntegrationManager) Trigger(ctx context.Context, id string, event *IntegrationEvent) error {
	m.mu.RLock()
	config, exists := m.integrations[id]
	handler := m.handlers[id]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("integration: %s not found", id)
	}

	if !config.Enabled {
		return nil // Skip disabled integrations
	}

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}

	var err error
	switch config.Type {
	case IntegrationTypeWebhook:
		err = m.triggerWebhook(ctx, config, event)
	case IntegrationTypeCustom:
		if handler != nil {
			err = handler(ctx, event)
		}
	default:
		err = m.triggerWebhook(ctx, config, event)
	}

	m.recordResult(id, err)
	return err
}

// TriggerByEventType triggers all integrations matching an event type
func (m *IntegrationManager) TriggerByEventType(ctx context.Context, tenantID string, event *IntegrationEvent) error {
	m.mu.RLock()
	var toTrigger []string
	for id, config := range m.integrations {
		if config.TenantID != tenantID && config.TenantID != "" {
			continue
		}
		if m.matchesEventType(config, event.Type) {
			toTrigger = append(toTrigger, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range toTrigger {
		if err := m.Trigger(ctx, id, event); err != nil {
			// Log but continue
			continue
		}
	}

	return nil
}

func (m *IntegrationManager) matchesEventType(config *IntegrationConfig, eventType string) bool {
	if len(config.EventTypes) == 0 {
		return true // No filter, match all
	}

	for _, pattern := range config.EventTypes {
		if pattern == eventType {
			return true
		}
		// Wildcard matching
		if strings.HasSuffix(pattern, "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			if strings.HasPrefix(eventType, prefix) {
				return true
			}
		}
	}

	return false
}

func (m *IntegrationManager) triggerWebhook(ctx context.Context, config *IntegrationConfig, event *IntegrationEvent) error {
	payload := map[string]interface{}{
		"type":      event.Type,
		"timestamp": event.Timestamp.Format(time.RFC3339),
		"payload":   event.Payload,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("integration: failed to marshal payload: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= m.config.MaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(m.config.RetryDelay)
		}

		req, err := http.NewRequestWithContext(ctx, "POST", config.Endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("integration: failed to create request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")

		// Add custom headers
		for key, value := range config.Headers {
			req.Header.Set(key, value)
		}

		// Add auth
		m.applyAuth(req, config.Auth)

		resp, err := m.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		_ = resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}

		lastErr = fmt.Errorf("integration: webhook returned status %d", resp.StatusCode)
	}

	return lastErr
}

func (m *IntegrationManager) applyAuth(req *http.Request, auth *AuthConfig) {
	if auth == nil {
		return
	}

	switch auth.Type {
	case AuthTypeBasic:
		credentials := base64.StdEncoding.EncodeToString([]byte(auth.Username + ":" + auth.Password))
		req.Header.Set("Authorization", "Basic "+credentials)
	case AuthTypeBearer:
		req.Header.Set("Authorization", "Bearer "+auth.Token)
	case AuthTypeAPIKey:
		header := auth.Header
		if header == "" {
			header = "X-API-Key"
		}
		req.Header.Set(header, auth.APIKey)
	}
}

func (m *IntegrationManager) recordResult(id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	status, exists := m.statuses[id]
	if !exists {
		return
	}

	if err != nil {
		status.LastFailure = time.Now()
		status.FailureCount++
		status.LastError = err.Error()
	} else {
		status.LastSuccess = time.Now()
		status.SuccessCount++
		status.LastError = ""
	}
}

// GetStatus returns integration status
func (m *IntegrationManager) GetStatus(id string) *IntegrationStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.statuses[id]
}

// ListAll returns all integrations
func (m *IntegrationManager) ListAll() []*IntegrationConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*IntegrationConfig, 0, len(m.integrations))
	for _, config := range m.integrations {
		result = append(result, config)
	}
	return result
}

// Enable enables an integration
func (m *IntegrationManager) Enable(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	config, exists := m.integrations[id]
	if !exists {
		return fmt.Errorf("integration: %s not found", id)
	}

	config.Enabled = true
	return nil
}

// Disable disables an integration
func (m *IntegrationManager) Disable(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	config, exists := m.integrations[id]
	if !exists {
		return fmt.Errorf("integration: %s not found", id)
	}

	config.Enabled = false
	return nil
}
