// internal/devops/alerting.go
package devops

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// AlertSeverity represents alert severity levels
type AlertSeverity string

const (
	AlertSeverityInfo     AlertSeverity = "info"
	AlertSeverityWarning  AlertSeverity = "warning"
	AlertSeverityCritical AlertSeverity = "critical"
	AlertSeverityFatal    AlertSeverity = "fatal"
)

// AlertState represents the current state of an alert
type AlertState string

const (
	AlertStatePending  AlertState = "pending"
	AlertStateFiring   AlertState = "firing"
	AlertStateResolved AlertState = "resolved"
	AlertStateSilenced AlertState = "silenced"
)

// NotificationChannel represents alert delivery channels
type NotificationChannel string

const (
	ChannelEmail     NotificationChannel = "email"
	ChannelSlack     NotificationChannel = "slack"
	ChannelPagerDuty NotificationChannel = "pagerduty"
	ChannelWebhook   NotificationChannel = "webhook"
	ChannelSMS       NotificationChannel = "sms"
)

// AlertRule defines when to trigger an alert
type AlertRule struct {
	Name        string                `json:"name"`
	Description string                `json:"description"`
	Severity    AlertSeverity         `json:"severity"`
	Condition   string                `json:"condition"`
	Threshold   float64               `json:"threshold"`
	Duration    time.Duration         `json:"duration"`
	Labels      map[string]string     `json:"labels,omitempty"`
	Channels    []NotificationChannel `json:"channels"`
	Enabled     bool                  `json:"enabled"`
	Cooldown    time.Duration         `json:"cooldown"`
	LastFired   *time.Time            `json:"last_fired,omitempty"`
}

// Alert represents an active or historical alert
type Alert struct {
	ID         string            `json:"id"`
	RuleName   string            `json:"rule_name"`
	Severity   AlertSeverity     `json:"severity"`
	State      AlertState        `json:"state"`
	Message    string            `json:"message"`
	Value      float64           `json:"value"`
	Threshold  float64           `json:"threshold"`
	Labels     map[string]string `json:"labels,omitempty"`
	FiredAt    time.Time         `json:"fired_at"`
	ResolvedAt *time.Time        `json:"resolved_at,omitempty"`
	AckedAt    *time.Time        `json:"acked_at,omitempty"`
	AckedBy    string            `json:"acked_by,omitempty"`
	NotifiedAt *time.Time        `json:"notified_at,omitempty"`
}

// Duration returns how long the alert has been active
func (a *Alert) Duration() time.Duration {
	if a.ResolvedAt != nil {
		return a.ResolvedAt.Sub(a.FiredAt)
	}
	return time.Since(a.FiredAt)
}

// NotificationConfig configures a notification channel
type NotificationConfig struct {
	Channel    NotificationChannel `json:"channel"`
	Enabled    bool                `json:"enabled"`
	Endpoint   string              `json:"endpoint"`
	APIKey     string              `json:"api_key,omitempty"`
	Recipients []string            `json:"recipients,omitempty"`
	Template   string              `json:"template,omitempty"`
}

// Silence represents a period where alerts are suppressed
type Silence struct {
	ID        string            `json:"id"`
	Matchers  map[string]string `json:"matchers"`
	StartsAt  time.Time         `json:"starts_at"`
	EndsAt    time.Time         `json:"ends_at"`
	CreatedBy string            `json:"created_by"`
	Comment   string            `json:"comment"`
}

// IsActive checks if a silence is currently active
func (s *Silence) IsActive() bool {
	now := time.Now()
	return now.After(s.StartsAt) && now.Before(s.EndsAt)
}

// AlertingConfig configures the alerting system
type AlertingConfig struct {
	Enabled            bool          `json:"enabled"`
	EvaluationInterval time.Duration `json:"evaluation_interval"`
	NotificationDelay  time.Duration `json:"notification_delay"`
	RepeatInterval     time.Duration `json:"repeat_interval"`
	ResolveTimeout     time.Duration `json:"resolve_timeout"`
	MaxAlerts          int           `json:"max_alerts"`
}

// DefaultAlertingConfigs returns environment-specific configurations
var DefaultAlertingConfigs = map[string]*AlertingConfig{
	EnvTypeDevelopment: {
		Enabled:            false,
		EvaluationInterval: 1 * time.Minute,
		NotificationDelay:  5 * time.Minute,
		RepeatInterval:     1 * time.Hour,
		ResolveTimeout:     5 * time.Minute,
		MaxAlerts:          100,
	},
	EnvTypeStaging: {
		Enabled:            true,
		EvaluationInterval: 30 * time.Second,
		NotificationDelay:  2 * time.Minute,
		RepeatInterval:     30 * time.Minute,
		ResolveTimeout:     5 * time.Minute,
		MaxAlerts:          500,
	},
	EnvTypeProduction: {
		Enabled:            true,
		EvaluationInterval: 15 * time.Second,
		NotificationDelay:  1 * time.Minute,
		RepeatInterval:     15 * time.Minute,
		ResolveTimeout:     5 * time.Minute,
		MaxAlerts:          1000,
	},
}

// alertCounter for unique IDs
var alertCounter int64
var alertCounterMu sync.Mutex

// AlertManager manages alerts
type AlertManager struct {
	config        *AlertingConfig
	rules         map[string]*AlertRule
	alerts        map[string]*Alert
	silences      map[string]*Silence
	notifications map[NotificationChannel]*NotificationConfig
	alertChan     chan *Alert
	mu            sync.RWMutex
}

// NewAlertManager creates an alert manager
func NewAlertManager(config *AlertingConfig) *AlertManager {
	if config == nil {
		config = DefaultAlertingConfigs[EnvTypeDevelopment]
	}
	return &AlertManager{
		config:        config,
		rules:         make(map[string]*AlertRule),
		alerts:        make(map[string]*Alert),
		silences:      make(map[string]*Silence),
		notifications: make(map[NotificationChannel]*NotificationConfig),
		alertChan:     make(chan *Alert, 100),
	}
}

// GetConfig returns the configuration
func (m *AlertManager) GetConfig() *AlertingConfig {
	return m.config
}

// IsEnabled returns whether alerting is enabled
func (m *AlertManager) IsEnabled() bool {
	return m.config.Enabled
}

// AddRule adds an alerting rule
func (m *AlertManager) AddRule(rule *AlertRule) error {
	if rule == nil {
		return errors.New("alerting: rule is required")
	}
	if rule.Name == "" {
		return errors.New("alerting: rule name is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.rules[rule.Name]; exists {
		return fmt.Errorf("alerting: rule %s already exists", rule.Name)
	}

	if rule.Severity == "" {
		rule.Severity = AlertSeverityWarning
	}
	if rule.Duration == 0 {
		rule.Duration = 5 * time.Minute
	}
	if rule.Cooldown == 0 {
		rule.Cooldown = 15 * time.Minute
	}
	rule.Enabled = true

	m.rules[rule.Name] = rule
	return nil
}

// GetRule returns a rule by name
func (m *AlertManager) GetRule(name string) *AlertRule {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rules[name]
}

// ListRules returns all rules
func (m *AlertManager) ListRules() []*AlertRule {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rules := make([]*AlertRule, 0, len(m.rules))
	for _, r := range m.rules {
		rules = append(rules, r)
	}
	return rules
}

// EnableRule enables a rule
func (m *AlertManager) EnableRule(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rule, exists := m.rules[name]
	if !exists {
		return fmt.Errorf("alerting: rule %s not found", name)
	}
	rule.Enabled = true
	return nil
}

// DisableRule disables a rule
func (m *AlertManager) DisableRule(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rule, exists := m.rules[name]
	if !exists {
		return fmt.Errorf("alerting: rule %s not found", name)
	}
	rule.Enabled = false
	return nil
}

// RemoveRule removes a rule
func (m *AlertManager) RemoveRule(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.rules[name]; !exists {
		return fmt.Errorf("alerting: rule %s not found", name)
	}
	delete(m.rules, name)
	return nil
}

// FireAlert creates and fires a new alert
func (m *AlertManager) FireAlert(ruleName, message string, value float64, labels map[string]string) (*Alert, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rule, exists := m.rules[ruleName]
	if !exists {
		return nil, fmt.Errorf("alerting: rule %s not found", ruleName)
	}

	if !rule.Enabled {
		return nil, fmt.Errorf("alerting: rule %s is disabled", ruleName)
	}

	// Check cooldown
	if rule.LastFired != nil && time.Since(*rule.LastFired) < rule.Cooldown {
		return nil, nil // Still in cooldown
	}

	// Check for silences
	if m.isSilenced(labels) {
		return nil, nil
	}

	alertCounterMu.Lock()
	alertCounter++
	counter := alertCounter
	alertCounterMu.Unlock()

	id := fmt.Sprintf("alert-%d-%d", time.Now().UnixNano(), counter)
	now := time.Now()

	alert := &Alert{
		ID:        id,
		RuleName:  ruleName,
		Severity:  rule.Severity,
		State:     AlertStateFiring,
		Message:   message,
		Value:     value,
		Threshold: rule.Threshold,
		Labels:    labels,
		FiredAt:   now,
	}

	m.alerts[id] = alert
	rule.LastFired = &now

	// Non-blocking send to channel
	select {
	case m.alertChan <- alert:
	default:
	}

	return alert, nil
}

// ResolveAlert resolves an active alert
func (m *AlertManager) ResolveAlert(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	alert, exists := m.alerts[id]
	if !exists {
		return fmt.Errorf("alerting: alert %s not found", id)
	}

	now := time.Now()
	alert.State = AlertStateResolved
	alert.ResolvedAt = &now

	return nil
}

// AcknowledgeAlert acknowledges an alert
func (m *AlertManager) AcknowledgeAlert(id, user string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	alert, exists := m.alerts[id]
	if !exists {
		return fmt.Errorf("alerting: alert %s not found", id)
	}

	now := time.Now()
	alert.AckedAt = &now
	alert.AckedBy = user

	return nil
}

// GetAlert returns an alert by ID
func (m *AlertManager) GetAlert(id string) *Alert {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if alert, exists := m.alerts[id]; exists {
		copy := *alert
		return &copy
	}
	return nil
}

// ListAlerts returns all alerts
func (m *AlertManager) ListAlerts() []*Alert {
	m.mu.RLock()
	defer m.mu.RUnlock()

	alerts := make([]*Alert, 0, len(m.alerts))
	for _, a := range m.alerts {
		alerts = append(alerts, a)
	}
	return alerts
}

// ListActiveAlerts returns only firing alerts
func (m *AlertManager) ListActiveAlerts() []*Alert {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var alerts []*Alert
	for _, a := range m.alerts {
		if a.State == AlertStateFiring {
			alerts = append(alerts, a)
		}
	}
	return alerts
}

// ListAlertsBySeverity returns alerts of a specific severity
func (m *AlertManager) ListAlertsBySeverity(severity AlertSeverity) []*Alert {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var alerts []*Alert
	for _, a := range m.alerts {
		if a.Severity == severity {
			alerts = append(alerts, a)
		}
	}
	return alerts
}

// GetAlertChannel returns channel for alert notifications
func (m *AlertManager) GetAlertChannel() <-chan *Alert {
	return m.alertChan
}

// AddSilence adds a silence period
func (m *AlertManager) AddSilence(silence *Silence) error {
	if silence == nil {
		return errors.New("alerting: silence is required")
	}
	if silence.ID == "" {
		silence.ID = fmt.Sprintf("silence-%d", time.Now().UnixNano())
	}
	if silence.EndsAt.Before(silence.StartsAt) {
		return errors.New("alerting: silence end time must be after start time")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.silences[silence.ID] = silence
	return nil
}

// RemoveSilence removes a silence
func (m *AlertManager) RemoveSilence(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.silences[id]; !exists {
		return fmt.Errorf("alerting: silence %s not found", id)
	}
	delete(m.silences, id)
	return nil
}

// ListActiveSilences returns currently active silences
func (m *AlertManager) ListActiveSilences() []*Silence {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var silences []*Silence
	for _, s := range m.silences {
		if s.IsActive() {
			silences = append(silences, s)
		}
	}
	return silences
}

func (m *AlertManager) isSilenced(labels map[string]string) bool {
	for _, silence := range m.silences {
		if !silence.IsActive() {
			continue
		}

		matches := true
		for k, v := range silence.Matchers {
			if labels[k] != v {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

// ConfigureNotification configures a notification channel
func (m *AlertManager) ConfigureNotification(config *NotificationConfig) error {
	if config == nil {
		return errors.New("alerting: notification config is required")
	}
	if config.Endpoint == "" && config.Channel != ChannelSMS {
		return errors.New("alerting: endpoint is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	config.Enabled = true
	m.notifications[config.Channel] = config
	return nil
}

// GetNotificationConfig returns config for a channel
func (m *AlertManager) GetNotificationConfig(channel NotificationChannel) *NotificationConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.notifications[channel]
}

// ListNotificationChannels returns all configured channels
func (m *AlertManager) ListNotificationChannels() []NotificationChannel {
	m.mu.RLock()
	defer m.mu.RUnlock()

	channels := make([]NotificationChannel, 0, len(m.notifications))
	for ch := range m.notifications {
		channels = append(channels, ch)
	}
	return channels
}

// SetupProductionAlerting sets up standard production alerts
func (m *AlertManager) SetupProductionAlerting() error {
	rules := []*AlertRule{
		{
			Name:        "high_error_rate",
			Description: "Error rate exceeds threshold",
			Severity:    AlertSeverityCritical,
			Condition:   "error_rate > threshold",
			Threshold:   0.05, // 5%
			Duration:    5 * time.Minute,
			Channels:    []NotificationChannel{ChannelSlack, ChannelPagerDuty},
		},
		{
			Name:        "high_latency",
			Description: "Request latency exceeds threshold",
			Severity:    AlertSeverityWarning,
			Condition:   "p99_latency > threshold",
			Threshold:   2.0, // 2 seconds
			Duration:    5 * time.Minute,
			Channels:    []NotificationChannel{ChannelSlack},
		},
		{
			Name:        "storage_backend_down",
			Description: "Storage backend is unavailable",
			Severity:    AlertSeverityCritical,
			Condition:   "backend_healthy == 0",
			Threshold:   0,
			Duration:    2 * time.Minute,
			Channels:    []NotificationChannel{ChannelSlack, ChannelPagerDuty},
		},
		{
			Name:        "database_connection_pool_exhausted",
			Description: "Database connection pool is exhausted",
			Severity:    AlertSeverityCritical,
			Condition:   "db_available_connections == 0",
			Threshold:   0,
			Duration:    1 * time.Minute,
			Channels:    []NotificationChannel{ChannelSlack, ChannelPagerDuty},
		},
		{
			Name:        "disk_space_low",
			Description: "Disk space is running low",
			Severity:    AlertSeverityWarning,
			Condition:   "disk_free_percent < threshold",
			Threshold:   0.1, // 10%
			Duration:    10 * time.Minute,
			Channels:    []NotificationChannel{ChannelSlack},
		},
		{
			Name:        "disk_space_critical",
			Description: "Disk space critically low",
			Severity:    AlertSeverityCritical,
			Condition:   "disk_free_percent < threshold",
			Threshold:   0.05, // 5%
			Duration:    5 * time.Minute,
			Channels:    []NotificationChannel{ChannelSlack, ChannelPagerDuty},
		},
		{
			Name:        "memory_usage_high",
			Description: "Memory usage exceeds threshold",
			Severity:    AlertSeverityWarning,
			Condition:   "memory_usage_percent > threshold",
			Threshold:   0.85, // 85%
			Duration:    10 * time.Minute,
			Channels:    []NotificationChannel{ChannelSlack},
		},
		{
			Name:        "certificate_expiring",
			Description: "SSL certificate expiring soon",
			Severity:    AlertSeverityWarning,
			Condition:   "cert_days_remaining < threshold",
			Threshold:   30, // 30 days
			Duration:    1 * time.Hour,
			Channels:    []NotificationChannel{ChannelSlack, ChannelEmail},
		},
		{
			Name:        "backup_failed",
			Description: "Backup job failed",
			Severity:    AlertSeverityCritical,
			Condition:   "backup_status == failed",
			Threshold:   0,
			Duration:    0,
			Channels:    []NotificationChannel{ChannelSlack, ChannelPagerDuty},
		},
		{
			Name:        "ddos_attack_detected",
			Description: "Potential DDoS attack detected",
			Severity:    AlertSeverityFatal,
			Condition:   "threat_level >= critical",
			Threshold:   0,
			Duration:    1 * time.Minute,
			Channels:    []NotificationChannel{ChannelSlack, ChannelPagerDuty, ChannelSMS},
		},
	}

	for _, rule := range rules {
		if err := m.AddRule(rule); err != nil {
			return fmt.Errorf("alerting: failed to add rule %s: %w", rule.Name, err)
		}
	}

	return nil
}

// GetStats returns alerting statistics
func (m *AlertManager) GetStats() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	activeCount := 0
	severityCounts := make(map[AlertSeverity]int)

	for _, alert := range m.alerts {
		if alert.State == AlertStateFiring {
			activeCount++
		}
		severityCounts[alert.Severity]++
	}

	return map[string]interface{}{
		"enabled":             m.config.Enabled,
		"total_rules":         len(m.rules),
		"total_alerts":        len(m.alerts),
		"active_alerts":       activeCount,
		"by_severity":         severityCounts,
		"active_silences":     len(m.ListActiveSilences()),
		"channels_configured": len(m.notifications),
	}
}

// GetAlertingConfigForEnvironment returns config for an environment
func GetAlertingConfigForEnvironment(envType string) *AlertingConfig {
	if config, ok := DefaultAlertingConfigs[envType]; ok {
		return config
	}
	return DefaultAlertingConfigs[EnvTypeDevelopment]
}
