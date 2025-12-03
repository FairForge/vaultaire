// internal/webhooks/webhook.go
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Standard event types
const (
	EventObjectCreated  = "object.created"
	EventObjectDeleted  = "object.deleted"
	EventObjectAccessed = "object.accessed"
	EventObjectModified = "object.modified"
	EventBucketCreated  = "bucket.created"
	EventBucketDeleted  = "bucket.deleted"
	EventUserCreated    = "user.created"
	EventUserDeleted    = "user.deleted"
	EventQuotaExceeded  = "quota.exceeded"
)

// Delivery statuses
const (
	DeliveryStatusPending = "pending"
	DeliveryStatusSuccess = "success"
	DeliveryStatusFailed  = "failed"
)

// WebhookConfig configures a webhook endpoint
type WebhookConfig struct {
	ID           string            `json:"id"`
	URL          string            `json:"url"`
	Events       []string          `json:"events"`
	Secret       string            `json:"secret,omitempty"`
	TenantID     string            `json:"tenant_id"`
	Headers      map[string]string `json:"headers,omitempty"`
	Enabled      bool              `json:"enabled"`
	RequireHTTPS bool              `json:"require_https"`
	Description  string            `json:"description,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// Validate checks if the configuration is valid
func (c *WebhookConfig) Validate() error {
	if c.URL == "" {
		return errors.New("webhook: URL is required")
	}

	parsedURL, err := url.Parse(c.URL)
	if err != nil {
		return fmt.Errorf("webhook: invalid URL: %w", err)
	}

	if c.RequireHTTPS && parsedURL.Scheme != "https" {
		return errors.New("webhook: HTTPS is required")
	}

	if len(c.Events) == 0 {
		return errors.New("webhook: at least one event is required")
	}

	return nil
}

// MatchesEvent checks if webhook should receive an event
func (c *WebhookConfig) MatchesEvent(eventType string) bool {
	for _, e := range c.Events {
		if e == "*" || e == eventType {
			return true
		}
		// Support prefix matching (e.g., "object.*")
		if strings.HasSuffix(e, ".*") {
			prefix := strings.TrimSuffix(e, ".*")
			if strings.HasPrefix(eventType, prefix+".") {
				return true
			}
		}
	}
	return false
}

// Event represents an event to dispatch
type Event struct {
	ID        string                 `json:"id"`
	Type      string                 `json:"type"`
	TenantID  string                 `json:"tenant_id"`
	Data      map[string]interface{} `json:"data"`
	Timestamp time.Time              `json:"timestamp"`
}

// WebhookPayload is sent to webhook endpoints
type WebhookPayload struct {
	ID        string                 `json:"id"`
	Type      string                 `json:"type"`
	TenantID  string                 `json:"tenant_id"`
	Data      map[string]interface{} `json:"data,omitempty"`
	Timestamp time.Time              `json:"timestamp"`
	Attempt   int                    `json:"attempt"`
}

// NewWebhookPayload creates a new webhook payload
func NewWebhookPayload(eventType, tenantID string, data map[string]interface{}) *WebhookPayload {
	return &WebhookPayload{
		ID:        uuid.New().String(),
		Type:      eventType,
		TenantID:  tenantID,
		Data:      data,
		Timestamp: time.Now().UTC(),
		Attempt:   1,
	}
}

// WebhookDelivery tracks a delivery attempt
type WebhookDelivery struct {
	ID           string        `json:"id"`
	WebhookID    string        `json:"webhook_id"`
	EventID      string        `json:"event_id"`
	Status       string        `json:"status"`
	StatusCode   int           `json:"status_code,omitempty"`
	ResponseBody string        `json:"response_body,omitempty"`
	Error        string        `json:"error,omitempty"`
	Duration     time.Duration `json:"duration"`
	Attempts     int           `json:"attempts"`
	CreatedAt    time.Time     `json:"created_at"`
}

// ManagerConfig configures the webhook manager
type ManagerConfig struct {
	MaxRetries     int           `json:"max_retries"`
	RetryInterval  time.Duration `json:"retry_interval"`
	RequestTimeout time.Duration `json:"request_timeout"`
	MaxConcurrent  int           `json:"max_concurrent"`
}

// DefaultManagerConfig returns sensible defaults
func DefaultManagerConfig() *ManagerConfig {
	return &ManagerConfig{
		MaxRetries:     3,
		RetryInterval:  time.Second,
		RequestTimeout: 30 * time.Second,
		MaxConcurrent:  100,
	}
}

// WebhookManager manages webhook registrations and dispatching
type WebhookManager struct {
	config     *ManagerConfig
	webhooks   map[string]*WebhookConfig
	deliveries map[string][]*WebhookDelivery
	httpClient *http.Client
	mu         sync.RWMutex
	sem        chan struct{}
}

// NewWebhookManager creates a new webhook manager
func NewWebhookManager(config *ManagerConfig) *WebhookManager {
	if config == nil {
		config = DefaultManagerConfig()
	}

	return &WebhookManager{
		config:     config,
		webhooks:   make(map[string]*WebhookConfig),
		deliveries: make(map[string][]*WebhookDelivery),
		httpClient: &http.Client{
			Timeout: config.RequestTimeout,
		},
		sem: make(chan struct{}, config.MaxConcurrent),
	}
}

// Config returns the manager configuration
func (m *WebhookManager) Config() *ManagerConfig {
	return m.config
}

// Register registers a webhook
func (m *WebhookManager) Register(config *WebhookConfig) error {
	if err := config.Validate(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.webhooks[config.ID]; exists {
		return fmt.Errorf("webhook: ID %s already exists", config.ID)
	}

	config.Enabled = true
	config.CreatedAt = time.Now().UTC()
	config.UpdatedAt = config.CreatedAt
	m.webhooks[config.ID] = config

	return nil
}

// Unregister removes a webhook
func (m *WebhookManager) Unregister(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.webhooks[id]; !exists {
		return fmt.Errorf("webhook: ID %s not found", id)
	}

	delete(m.webhooks, id)
	return nil
}

// Get retrieves a webhook by ID
func (m *WebhookManager) Get(id string) *WebhookConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.webhooks[id]
}

// ListByTenant returns all webhooks for a tenant
func (m *WebhookManager) ListByTenant(tenantID string) []*WebhookConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*WebhookConfig
	for _, wh := range m.webhooks {
		if wh.TenantID == tenantID {
			result = append(result, wh)
		}
	}
	return result
}

// Dispatch sends an event to all matching webhooks asynchronously
func (m *WebhookManager) Dispatch(ctx context.Context, event *Event) error {
	webhooks := m.findMatchingWebhooks(event)

	for _, wh := range webhooks {
		wh := wh // capture for goroutine
		go func() {
			m.sem <- struct{}{}
			defer func() { <-m.sem }()
			_ = m.deliver(ctx, wh, event)
		}()
	}

	return nil
}

// DispatchSync sends an event synchronously (for testing)
func (m *WebhookManager) DispatchSync(ctx context.Context, event *Event) error {
	webhooks := m.findMatchingWebhooks(event)

	var lastErr error
	for _, wh := range webhooks {
		if err := m.deliver(ctx, wh, event); err != nil {
			lastErr = err
		}
	}

	return lastErr
}

// findMatchingWebhooks finds webhooks that match an event
func (m *WebhookManager) findMatchingWebhooks(event *Event) []*WebhookConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*WebhookConfig
	for _, wh := range m.webhooks {
		if !wh.Enabled {
			continue
		}
		if wh.TenantID != event.TenantID {
			continue
		}
		if wh.MatchesEvent(event.Type) {
			result = append(result, wh)
		}
	}
	return result
}

// deliver sends a webhook with retries
func (m *WebhookManager) deliver(ctx context.Context, wh *WebhookConfig, event *Event) error {
	payload := &WebhookPayload{
		ID:        event.ID,
		Type:      event.Type,
		TenantID:  event.TenantID,
		Data:      event.Data,
		Timestamp: time.Now().UTC(),
	}

	var lastErr error
	for attempt := 1; attempt <= m.config.MaxRetries; attempt++ {
		payload.Attempt = attempt

		delivery := &WebhookDelivery{
			ID:        uuid.New().String(),
			WebhookID: wh.ID,
			EventID:   event.ID,
			Status:    DeliveryStatusPending,
			Attempts:  attempt,
			CreatedAt: time.Now().UTC(),
		}

		start := time.Now()
		statusCode, respBody, err := m.sendRequest(ctx, wh, payload)
		delivery.Duration = time.Since(start)
		delivery.StatusCode = statusCode
		delivery.ResponseBody = respBody

		if err == nil && statusCode >= 200 && statusCode < 300 {
			delivery.Status = DeliveryStatusSuccess
			m.recordDelivery(wh.ID, delivery)
			return nil
		}

		delivery.Status = DeliveryStatusFailed
		if err != nil {
			delivery.Error = err.Error()
			lastErr = err
		} else {
			lastErr = fmt.Errorf("webhook returned status %d", statusCode)
			delivery.Error = lastErr.Error()
		}

		m.recordDelivery(wh.ID, delivery)

		if attempt < m.config.MaxRetries {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.config.RetryInterval):
			}
		}
	}

	return lastErr
}

// sendRequest sends a single webhook request
func (m *WebhookManager) sendRequest(ctx context.Context, wh *WebhookConfig, payload *WebhookPayload) (int, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", wh.URL, bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Vaultaire-Webhooks/1.0")
	req.Header.Set("X-Webhook-ID", wh.ID)
	req.Header.Set("X-Event-Type", payload.Type)
	req.Header.Set("X-Event-ID", payload.ID)
	req.Header.Set("X-Delivery-Attempt", fmt.Sprintf("%d", payload.Attempt))

	if wh.Secret != "" {
		signature := m.GenerateSignature(body, wh.Secret)
		req.Header.Set("X-Webhook-Signature", signature)
	}

	for key, value := range wh.Headers {
		req.Header.Set(key, value)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var respBody strings.Builder
	respBody.Grow(1024)
	buf := make([]byte, 1024)
	n, _ := resp.Body.Read(buf)
	respBody.Write(buf[:n])

	return resp.StatusCode, respBody.String(), nil
}

// GenerateSignature generates HMAC-SHA256 signature
func (m *WebhookManager) GenerateSignature(payload []byte, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payload)
	return "sha256=" + hex.EncodeToString(h.Sum(nil))
}

// VerifySignature verifies a webhook signature
func (m *WebhookManager) VerifySignature(payload []byte, signature, secret string) bool {
	expected := m.GenerateSignature(payload, secret)
	return hmac.Equal([]byte(expected), []byte(signature))
}

// recordDelivery stores a delivery record
func (m *WebhookManager) recordDelivery(webhookID string, delivery *WebhookDelivery) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.deliveries[webhookID] = append(m.deliveries[webhookID], delivery)

	// Keep only last 100 deliveries per webhook
	if len(m.deliveries[webhookID]) > 100 {
		m.deliveries[webhookID] = m.deliveries[webhookID][len(m.deliveries[webhookID])-100:]
	}
}

// GetDeliveryHistory returns delivery history for a webhook
func (m *WebhookManager) GetDeliveryHistory(webhookID string, limit int) []*WebhookDelivery {
	m.mu.RLock()
	defer m.mu.RUnlock()

	deliveries := m.deliveries[webhookID]
	if len(deliveries) == 0 {
		return nil
	}

	if limit > len(deliveries) {
		limit = len(deliveries)
	}

	// Return most recent first
	result := make([]*WebhookDelivery, limit)
	for i := 0; i < limit; i++ {
		result[i] = deliveries[len(deliveries)-1-i]
	}

	return result
}

// Update updates a webhook configuration
func (m *WebhookManager) Update(id string, updates *WebhookConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, exists := m.webhooks[id]
	if !exists {
		return fmt.Errorf("webhook: ID %s not found", id)
	}

	if updates.URL != "" {
		existing.URL = updates.URL
	}
	if len(updates.Events) > 0 {
		existing.Events = updates.Events
	}
	if updates.Secret != "" {
		existing.Secret = updates.Secret
	}
	if updates.Headers != nil {
		existing.Headers = updates.Headers
	}
	existing.UpdatedAt = time.Now().UTC()

	return nil
}

// Enable enables a webhook
func (m *WebhookManager) Enable(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	wh, exists := m.webhooks[id]
	if !exists {
		return fmt.Errorf("webhook: ID %s not found", id)
	}

	wh.Enabled = true
	wh.UpdatedAt = time.Now().UTC()
	return nil
}

// Disable disables a webhook
func (m *WebhookManager) Disable(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	wh, exists := m.webhooks[id]
	if !exists {
		return fmt.Errorf("webhook: ID %s not found", id)
	}

	wh.Enabled = false
	wh.UpdatedAt = time.Now().UTC()
	return nil
}
