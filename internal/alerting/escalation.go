// internal/alerting/escalation.go
package alerting

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Priorities
const (
	PriorityLow      = 1
	PriorityMedium   = 2
	PriorityHigh     = 3
	PriorityCritical = 4
)

// Incident statuses
const (
	IncidentTriggered    = "triggered"
	IncidentAcknowledged = "acknowledged"
	IncidentResolved     = "resolved"
)

// EscalationStep defines a step in escalation
type EscalationStep struct {
	Delay   time.Duration `json:"delay"`
	Targets []string      `json:"targets"`
}

// EscalationPolicyConfig configures an escalation policy
type EscalationPolicyConfig struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Steps       []EscalationStep `json:"steps"`
	RepeatAfter time.Duration    `json:"repeat_after"`
}

// Validate checks configuration
func (c *EscalationPolicyConfig) Validate() error {
	if c.Name == "" {
		return errors.New("escalation: name is required")
	}
	if len(c.Steps) == 0 {
		return errors.New("escalation: at least one step is required")
	}
	for i, step := range c.Steps {
		if len(step.Targets) == 0 {
			return fmt.Errorf("escalation: step %d has no targets", i)
		}
	}
	return nil
}

// TimelineEntry represents an entry in incident timeline
type TimelineEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"`
	Actor     string    `json:"actor"`
	Details   string    `json:"details"`
}

// Incident represents an escalation incident
type Incident struct {
	id             string
	alertID        string
	policy         *EscalationPolicy
	currentStep    int
	status         string
	priority       int
	acknowledgedBy string
	resolvedBy     string
	resolution     string
	createdAt      time.Time
	acknowledgedAt time.Time
	resolvedAt     time.Time
	snoozedUntil   time.Time
	timeline       []TimelineEntry
	targets        []string
	mu             sync.Mutex
}

// ID returns the incident ID
func (i *Incident) ID() string {
	return i.id
}

// AlertID returns the associated alert ID
func (i *Incident) AlertID() string {
	return i.alertID
}

// CurrentStep returns the current escalation step
func (i *Incident) CurrentStep() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.currentStep
}

// CurrentTargets returns current notification targets
func (i *Incident) CurrentTargets() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.targets) > 0 {
		return i.targets
	}
	if i.currentStep < len(i.policy.config.Steps) {
		return i.policy.config.Steps[i.currentStep].Targets
	}
	return nil
}

// Status returns the incident status
func (i *Incident) Status() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.status
}

// Priority returns the priority
func (i *Incident) Priority() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.priority
}

// SetPriority sets the priority
func (i *Incident) SetPriority(priority int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.priority = priority
	i.addTimelineEntry("priority_changed", "", fmt.Sprintf("Priority set to %d", priority))
}

// IsAcknowledged returns whether incident is acknowledged
func (i *Incident) IsAcknowledged() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.status == IncidentAcknowledged || i.status == IncidentResolved
}

// AcknowledgedBy returns who acknowledged
func (i *Incident) AcknowledgedBy() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.acknowledgedBy
}

// IsResolved returns whether incident is resolved
func (i *Incident) IsResolved() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.status == IncidentResolved
}

// Resolution returns the resolution note
func (i *Incident) Resolution() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.resolution
}

// Acknowledge acknowledges the incident
func (i *Incident) Acknowledge(by string) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.status == IncidentResolved {
		return errors.New("incident already resolved")
	}

	i.status = IncidentAcknowledged
	i.acknowledgedBy = by
	i.acknowledgedAt = time.Now()
	i.addTimelineEntry("acknowledged", by, "")

	return nil
}

// Resolve resolves the incident
func (i *Incident) Resolve(by string, resolution string) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.status = IncidentResolved
	i.resolvedBy = by
	i.resolution = resolution
	i.resolvedAt = time.Now()
	i.addTimelineEntry("resolved", by, resolution)

	return nil
}

// Reassign reassigns to new target
func (i *Incident) Reassign(target string, reason string) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.targets = []string{target}
	i.addTimelineEntry("reassigned", "", fmt.Sprintf("Reassigned to %s: %s", target, reason))

	return nil
}

// Snooze snoozes escalation
func (i *Incident) Snooze(duration time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.snoozedUntil = time.Now().Add(duration)
	i.addTimelineEntry("snoozed", "", fmt.Sprintf("Snoozed for %v", duration))
}

// CheckEscalation checks if escalation should occur
func (i *Incident) CheckEscalation() {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Don't escalate if acknowledged or resolved
	if i.status != IncidentTriggered {
		return
	}

	// Don't escalate if snoozed
	if time.Now().Before(i.snoozedUntil) {
		return
	}

	// Check if we should escalate to next step
	if i.currentStep < len(i.policy.config.Steps)-1 {
		nextStep := i.policy.config.Steps[i.currentStep+1]
		escalateAt := i.createdAt.Add(nextStep.Delay)

		if time.Now().After(escalateAt) {
			i.currentStep++
			i.targets = nil // Reset to use step targets
			i.addTimelineEntry("escalated", "", fmt.Sprintf("Escalated to step %d", i.currentStep))
		}
	}
}

// Timeline returns the incident timeline
func (i *Incident) Timeline() []TimelineEntry {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.timeline
}

func (i *Incident) addTimelineEntry(action, actor, details string) {
	i.timeline = append(i.timeline, TimelineEntry{
		Timestamp: time.Now(),
		Action:    action,
		Actor:     actor,
		Details:   details,
	})
}

// EscalationPolicy represents an escalation policy
type EscalationPolicy struct {
	config    *EscalationPolicyConfig
	incidents []*Incident
	manager   *EscalationManager
	mu        sync.Mutex
}

// Name returns the policy name
func (p *EscalationPolicy) Name() string {
	return p.config.Name
}

// CreateIncident creates a new incident
func (p *EscalationPolicy) CreateIncident(alertID string) *Incident {
	p.mu.Lock()
	defer p.mu.Unlock()

	incident := &Incident{
		id:        uuid.New().String(),
		alertID:   alertID,
		policy:    p,
		status:    IncidentTriggered,
		priority:  PriorityMedium,
		createdAt: time.Now(),
		timeline:  make([]TimelineEntry, 0),
	}

	incident.addTimelineEntry("created", "", fmt.Sprintf("Incident created for alert %s", alertID))

	p.incidents = append(p.incidents, incident)

	if p.manager != nil {
		p.manager.addIncident(incident)
	}

	return incident
}

// Incidents returns all incidents for this policy
func (p *EscalationPolicy) Incidents() []*Incident {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.incidents
}

// EscalationManager manages escalation policies
type EscalationManager struct {
	policies  map[string]*EscalationPolicy
	incidents []*Incident
	mu        sync.RWMutex
}

// NewEscalationManager creates an escalation manager
func NewEscalationManager() *EscalationManager {
	return &EscalationManager{
		policies:  make(map[string]*EscalationPolicy),
		incidents: make([]*Incident, 0),
	}
}

// AddPolicy adds an escalation policy
func (m *EscalationManager) AddPolicy(config *EscalationPolicyConfig) (*EscalationPolicy, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.policies[config.Name]; exists {
		return nil, fmt.Errorf("escalation: policy %s already exists", config.Name)
	}

	policy := &EscalationPolicy{
		config:    config,
		incidents: make([]*Incident, 0),
		manager:   m,
	}

	m.policies[config.Name] = policy
	return policy, nil
}

// GetPolicy returns a policy by name
func (m *EscalationManager) GetPolicy(name string) *EscalationPolicy {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.policies[name]
}

// ListPolicies returns all policies
func (m *EscalationManager) ListPolicies() []*EscalationPolicy {
	m.mu.RLock()
	defer m.mu.RUnlock()

	policies := make([]*EscalationPolicy, 0, len(m.policies))
	for _, p := range m.policies {
		policies = append(policies, p)
	}
	return policies
}

// GetIncidents returns all incidents
func (m *EscalationManager) GetIncidents() []*Incident {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.incidents
}

// GetActiveIncidents returns active incidents
func (m *EscalationManager) GetActiveIncidents() []*Incident {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var active []*Incident
	for _, inc := range m.incidents {
		if !inc.IsResolved() {
			active = append(active, inc)
		}
	}
	return active
}

func (m *EscalationManager) addIncident(incident *Incident) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.incidents = append(m.incidents, incident)
}
