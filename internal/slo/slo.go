// internal/slo/slo.go
package slo

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Indicator types
const (
	IndicatorAvailability = "availability"
	IndicatorLatency      = "latency"
	IndicatorErrorRate    = "error_rate"
	IndicatorThroughput   = "throughput"
)

// SLOConfig configures an SLO
type SLOConfig struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Target      float64       `json:"target"`
	Window      time.Duration `json:"window"`
	Indicator   string        `json:"indicator"`
	Threshold   float64       `json:"threshold"`
}

// Validate checks configuration
func (c *SLOConfig) Validate() error {
	if c.Name == "" {
		return errors.New("slo: name is required")
	}
	if c.Target < 0 || c.Target > 100 {
		return fmt.Errorf("slo: target must be between 0 and 100")
	}
	return nil
}

// Event represents an SLI event
type Event struct {
	Timestamp time.Time
	Good      bool
	Value     float64
}

// ErrorBudgetInfo contains error budget details
type ErrorBudgetInfo struct {
	Total     float64 `json:"total"`
	Consumed  float64 `json:"consumed"`
	Remaining float64 `json:"remaining"`
}

// SLOStatus represents SLO status
type SLOStatus struct {
	Name        string    `json:"name"`
	Target      float64   `json:"target"`
	Current     float64   `json:"current"`
	TotalEvents int64     `json:"total_events"`
	GoodEvents  int64     `json:"good_events"`
	BadEvents   int64     `json:"bad_events"`
	IsMet       bool      `json:"is_met"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SLO represents a Service Level Objective
type SLO struct {
	config *SLOConfig
	events []Event
	mu     sync.RWMutex
}

// Name returns the SLO name
func (s *SLO) Name() string {
	return s.config.Name
}

// Target returns the target percentage
func (s *SLO) Target() float64 {
	return s.config.Target
}

// RecordSuccess records a successful event
func (s *SLO) RecordSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, Event{
		Timestamp: time.Now(),
		Good:      true,
	})
}

// RecordFailure records a failed event
func (s *SLO) RecordFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, Event{
		Timestamp: time.Now(),
		Good:      false,
	})
}

// RecordLatency records a latency measurement
func (s *SLO) RecordLatency(latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	good := float64(latency.Milliseconds()) <= s.config.Threshold
	s.events = append(s.events, Event{
		Timestamp: time.Now(),
		Good:      good,
		Value:     float64(latency.Milliseconds()),
	})
}

// RecordThroughput records a throughput measurement
func (s *SLO) RecordThroughput(throughput float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	good := throughput >= s.config.Threshold
	s.events = append(s.events, Event{
		Timestamp: time.Now(),
		Good:      good,
		Value:     throughput,
	})
}

// eventsInWindow returns events within the SLO window
func (s *SLO) eventsInWindow() []Event {
	cutoff := time.Now().Add(-s.config.Window)
	var result []Event
	for _, e := range s.events {
		if e.Timestamp.After(cutoff) {
			result = append(result, e)
		}
	}
	return result
}

// CurrentValue returns the current SLI value
func (s *SLO) CurrentValue() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	events := s.eventsInWindow()
	if len(events) == 0 {
		return 100.0
	}

	var good int64
	for _, e := range events {
		if e.Good {
			good++
		}
	}

	return float64(good) / float64(len(events)) * 100
}

// IsMet returns whether the SLO is currently met
func (s *SLO) IsMet() bool {
	return s.CurrentValue() >= s.config.Target
}

// ErrorRate returns the current error rate
func (s *SLO) ErrorRate() float64 {
	return 100 - s.CurrentValue()
}

// ErrorBudget returns error budget information
func (s *SLO) ErrorBudget() *ErrorBudgetInfo {
	total := 100 - s.config.Target
	consumed := 100 - s.CurrentValue()

	remaining := total - consumed
	if remaining < 0 {
		remaining = 0
	}

	return &ErrorBudgetInfo{
		Total:     total,
		Consumed:  consumed,
		Remaining: remaining,
	}
}

// BurnRate calculates the error budget burn rate
func (s *SLO) BurnRate(period time.Duration) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cutoff := time.Now().Add(-period)
	var total, bad int64

	for _, e := range s.events {
		if e.Timestamp.After(cutoff) {
			total++
			if !e.Good {
				bad++
			}
		}
	}

	if total == 0 {
		return 0
	}

	// Error rate in period
	errorRate := float64(bad) / float64(total) * 100

	// Allowed error rate
	allowedErrorRate := 100 - s.config.Target

	if allowedErrorRate == 0 {
		return 0
	}

	return errorRate / allowedErrorRate
}

// Status returns the current SLO status
func (s *SLO) Status() *SLOStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	events := s.eventsInWindow()
	var good, bad int64

	for _, e := range events {
		if e.Good {
			good++
		} else {
			bad++
		}
	}

	current := float64(100)
	if len(events) > 0 {
		current = float64(good) / float64(len(events)) * 100
	}

	return &SLOStatus{
		Name:        s.config.Name,
		Target:      s.config.Target,
		Current:     current,
		TotalEvents: int64(len(events)),
		GoodEvents:  good,
		BadEvents:   bad,
		IsMet:       current >= s.config.Target,
		UpdatedAt:   time.Now(),
	}
}

// ManagerConfig configures the SLO manager
type ManagerConfig struct {
	RetentionPeriod time.Duration
}

// Summary contains SLO summary
type Summary struct {
	Total     int `json:"total"`
	Meeting   int `json:"meeting"`
	Breaching int `json:"breaching"`
}

// SLOManager manages SLOs
type SLOManager struct {
	config *ManagerConfig
	slos   map[string]*SLO
	mu     sync.RWMutex
}

// NewSLOManager creates an SLO manager
func NewSLOManager(config *ManagerConfig) *SLOManager {
	if config == nil {
		config = &ManagerConfig{
			RetentionPeriod: 30 * 24 * time.Hour,
		}
	}

	return &SLOManager{
		config: config,
		slos:   make(map[string]*SLO),
	}
}

// Register registers an SLO
func (m *SLOManager) Register(config *SLOConfig) (*SLO, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.slos[config.Name]; exists {
		return nil, fmt.Errorf("slo: %s already exists", config.Name)
	}

	slo := &SLO{
		config: config,
		events: make([]Event, 0),
	}

	m.slos[config.Name] = slo
	return slo, nil
}

// Get returns an SLO by name
func (m *SLOManager) Get(name string) *SLO {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.slos[name]
}

// List returns all SLOs
func (m *SLOManager) List() []*SLO {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*SLO, 0, len(m.slos))
	for _, slo := range m.slos {
		result = append(result, slo)
	}
	return result
}

// Summary returns SLO summary
func (m *SLOManager) Summary() *Summary {
	m.mu.RLock()
	defer m.mu.RUnlock()

	summary := &Summary{Total: len(m.slos)}

	for _, slo := range m.slos {
		if slo.IsMet() {
			summary.Meeting++
		} else {
			summary.Breaching++
		}
	}

	return summary
}

// Unregister removes an SLO
func (m *SLOManager) Unregister(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.slos[name]; !exists {
		return fmt.Errorf("slo: %s not found", name)
	}

	delete(m.slos, name)
	return nil
}
