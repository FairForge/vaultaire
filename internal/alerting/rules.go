// internal/alerting/rules.go
package alerting

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Severities
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
	SeverityPage     = "page"
)

// States
const (
	StateInactive = "inactive"
	StatePending  = "pending"
	StateFiring   = "firing"
)

// AlertRuleConfig configures an alert rule
type AlertRuleConfig struct {
	Name        string            `json:"name"`
	Condition   string            `json:"condition"`
	Severity    string            `json:"severity"`
	Duration    time.Duration     `json:"duration"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

// Validate checks configuration
func (c *AlertRuleConfig) Validate() error {
	if c.Name == "" {
		return errors.New("alerting: name is required")
	}
	if c.Condition == "" {
		return errors.New("alerting: condition is required")
	}
	return nil
}

// Alert represents a fired alert
type Alert struct {
	ID         string            `json:"id"`
	RuleName   string            `json:"rule_name"`
	Severity   string            `json:"severity"`
	State      string            `json:"state"`
	Message    string            `json:"message"`
	FiredAt    time.Time         `json:"fired_at"`
	ResolvedAt time.Time         `json:"resolved_at,omitempty"`
	Labels     map[string]string `json:"labels"`
	Silenced   bool              `json:"silenced"`
}

// AlertRule represents an alerting rule
type AlertRule struct {
	config       *AlertRuleConfig
	state        string
	pendingSince time.Time
	firedAt      time.Time
	manager      *AlertManager
	mu           sync.Mutex
}

// Name returns the rule name
func (r *AlertRule) Name() string {
	return r.config.Name
}

// State returns the current state
func (r *AlertRule) State() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// Evaluate evaluates the rule against metrics
func (r *AlertRule) Evaluate(metrics map[string]float64) bool {
	result := r.evaluateCondition(metrics)

	r.mu.Lock()
	defer r.mu.Unlock()

	if result {
		if r.config.Duration > 0 {
			if r.state == StateInactive {
				r.state = StatePending
				r.pendingSince = time.Now()
				return false
			}
			if r.state == StatePending {
				if time.Since(r.pendingSince) >= r.config.Duration {
					r.state = StateFiring
					r.firedAt = time.Now()
					r.fireAlert()
					return true
				}
				return false
			}
		} else {
			if r.state != StateFiring {
				r.state = StateFiring
				r.firedAt = time.Now()
				r.fireAlert()
			}
			return true
		}
	} else {
		if r.state != StateInactive {
			r.state = StateInactive
			r.pendingSince = time.Time{}
		}
	}

	return false
}

func (r *AlertRule) fireAlert() {
	if r.manager != nil {
		alert := &Alert{
			ID:       uuid.New().String(),
			RuleName: r.config.Name,
			Severity: r.config.Severity,
			State:    StateFiring,
			FiredAt:  r.firedAt,
			Labels:   r.config.Labels,
			Silenced: r.manager.isSilenced(r.config.Name),
		}
		r.manager.addAlert(alert)
	}
}

func (r *AlertRule) evaluateCondition(metrics map[string]float64) bool {
	condition := r.config.Condition

	// Parse condition: "metric > value", "metric < value", "metric == value"
	patterns := []struct {
		regex *regexp.Regexp
		eval  func(actual, threshold float64) bool
	}{
		{regexp.MustCompile(`(\w+)\s*>\s*([\d.]+)`), func(a, t float64) bool { return a > t }},
		{regexp.MustCompile(`(\w+)\s*<\s*([\d.]+)`), func(a, t float64) bool { return a < t }},
		{regexp.MustCompile(`(\w+)\s*>=\s*([\d.]+)`), func(a, t float64) bool { return a >= t }},
		{regexp.MustCompile(`(\w+)\s*<=\s*([\d.]+)`), func(a, t float64) bool { return a <= t }},
		{regexp.MustCompile(`(\w+)\s*==\s*([\d.]+)`), func(a, t float64) bool { return a == t }},
		{regexp.MustCompile(`(\w+)\s*!=\s*([\d.]+)`), func(a, t float64) bool { return a != t }},
	}

	for _, p := range patterns {
		if matches := p.regex.FindStringSubmatch(condition); len(matches) == 3 {
			metricName := matches[1]
			threshold, err := strconv.ParseFloat(matches[2], 64)
			if err != nil {
				return false
			}

			actual, ok := metrics[metricName]
			if !ok {
				return false
			}

			return p.eval(actual, threshold)
		}
	}

	return false
}

// SilenceConfig configures a silence
type SilenceConfig struct {
	Matchers  map[string]string `json:"matchers"`
	StartsAt  time.Time         `json:"starts_at"`
	EndsAt    time.Time         `json:"ends_at"`
	CreatedBy string            `json:"created_by"`
	Comment   string            `json:"comment"`
}

// Silence represents an alert silence
type Silence struct {
	ID        string            `json:"id"`
	Matchers  map[string]string `json:"matchers"`
	StartsAt  time.Time         `json:"starts_at"`
	EndsAt    time.Time         `json:"ends_at"`
	CreatedBy string            `json:"created_by"`
	Comment   string            `json:"comment"`
}

// InhibitionRule defines alert inhibition
type InhibitionRule struct {
	SourceMatch map[string]string `json:"source_match"`
	TargetMatch map[string]string `json:"target_match"`
	Equal       []string          `json:"equal"`
}

// AlertManagerConfig configures the alert manager
type AlertManagerConfig struct {
	EvaluationInterval time.Duration `json:"evaluation_interval"`
}

// AlertManager manages alert rules
type AlertManager struct {
	config          *AlertManagerConfig
	rules           map[string]*AlertRule
	alerts          []*Alert
	silences        []*Silence
	inhibitions     []*InhibitionRule
	callbacks       []func(*Alert)
	metricsProvider func() map[string]float64
	mu              sync.RWMutex
}

// NewAlertManager creates an alert manager
func NewAlertManager(config *AlertManagerConfig) *AlertManager {
	if config == nil {
		config = &AlertManagerConfig{
			EvaluationInterval: time.Minute,
		}
	}

	return &AlertManager{
		config:      config,
		rules:       make(map[string]*AlertRule),
		alerts:      make([]*Alert, 0),
		silences:    make([]*Silence, 0),
		inhibitions: make([]*InhibitionRule, 0),
		callbacks:   make([]func(*Alert), 0),
	}
}

// AddRule adds an alerting rule
func (m *AlertManager) AddRule(config *AlertRuleConfig) (*AlertRule, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.rules[config.Name]; exists {
		return nil, fmt.Errorf("alerting: rule %s already exists", config.Name)
	}

	rule := &AlertRule{
		config:  config,
		state:   StateInactive,
		manager: m,
	}

	m.rules[config.Name] = rule
	return rule, nil
}

// GetRule returns a rule by name
func (m *AlertManager) GetRule(name string) *AlertRule {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rules[name]
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

// ListRules returns all rules
func (m *AlertManager) ListRules() []*AlertRule {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rules := make([]*AlertRule, 0, len(m.rules))
	for _, rule := range m.rules {
		rules = append(rules, rule)
	}
	return rules
}

// GetAlerts returns alerts by state
func (m *AlertManager) GetAlerts(state string) []*Alert {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*Alert
	for _, alert := range m.alerts {
		if alert.State == state {
			// Check if silenced
			alert.Silenced = m.isSilencedLocked(alert.RuleName)
			result = append(result, alert)
		}
	}
	return result
}

func (m *AlertManager) addAlert(alert *Alert) {
	m.mu.Lock()
	m.alerts = append(m.alerts, alert)
	callbacks := m.callbacks
	m.mu.Unlock()

	for _, cb := range callbacks {
		cb(alert)
	}
}

// CreateSilence creates a silence
func (m *AlertManager) CreateSilence(config *SilenceConfig) (*Silence, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	silence := &Silence{
		ID:        uuid.New().String(),
		Matchers:  config.Matchers,
		StartsAt:  config.StartsAt,
		EndsAt:    config.EndsAt,
		CreatedBy: config.CreatedBy,
		Comment:   config.Comment,
	}

	m.silences = append(m.silences, silence)
	return silence, nil
}

func (m *AlertManager) isSilenced(alertName string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.isSilencedLocked(alertName)
}

func (m *AlertManager) isSilencedLocked(alertName string) bool {
	now := time.Now()
	for _, silence := range m.silences {
		if now.Before(silence.StartsAt) || now.After(silence.EndsAt) {
			continue
		}
		if name, ok := silence.Matchers["alertname"]; ok {
			if strings.Contains(alertName, name) || name == alertName {
				return true
			}
		}
	}
	return false
}

// AddInhibition adds an inhibition rule
func (m *AlertManager) AddInhibition(rule *InhibitionRule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inhibitions = append(m.inhibitions, rule)
	return nil
}

// OnAlert registers an alert callback
func (m *AlertManager) OnAlert(callback func(*Alert)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callbacks = append(m.callbacks, callback)
}

// EvaluateAll evaluates all rules
func (m *AlertManager) EvaluateAll(metrics map[string]float64) {
	m.mu.RLock()
	rules := make([]*AlertRule, 0, len(m.rules))
	for _, rule := range m.rules {
		rules = append(rules, rule)
	}
	m.mu.RUnlock()

	for _, rule := range rules {
		rule.Evaluate(metrics)
	}
}

// SetMetricsProvider sets the metrics provider
func (m *AlertManager) SetMetricsProvider(provider func() map[string]float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.metricsProvider = provider
}

// Start starts periodic evaluation
func (m *AlertManager) Start(ctx context.Context) {
	ticker := time.NewTicker(m.config.EvaluationInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.RLock()
			provider := m.metricsProvider
			m.mu.RUnlock()

			if provider != nil {
				metrics := provider()
				m.EvaluateAll(metrics)
			}
		}
	}
}
