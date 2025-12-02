// internal/ha/rto_rpo.go
package ha

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ServiceTier represents the service level for RTO/RPO targets
type ServiceTier string

const (
	TierCritical   ServiceTier = "critical"
	TierStandard   ServiceTier = "standard"
	TierBestEffort ServiceTier = "best-effort"
)

// RTORPOStatus represents the current health status
type RTORPOStatus string

const (
	StatusHealthy  RTORPOStatus = "healthy"
	StatusWarning  RTORPOStatus = "warning"
	StatusCritical RTORPOStatus = "critical"
)

// RTORPOConfig defines RTO/RPO targets for a service tier
type RTORPOConfig struct {
	RTO              time.Duration // Recovery Time Objective
	RPO              time.Duration // Recovery Point Objective
	Tier             ServiceTier
	AlertThreshold   float64 // Percentage of target to trigger alerts (0.8 = 80%)
	EscalationPolicy string  // Policy name for escalations
}

// Validate checks if the config is valid
func (c RTORPOConfig) Validate() error {
	if c.RTO <= 0 {
		return errors.New("RTO must be greater than zero")
	}
	if c.RPO <= 0 {
		return errors.New("RPO must be greater than zero")
	}
	if c.RPO > c.RTO {
		return errors.New("RPO should not exceed RTO")
	}
	return nil
}

// GetTierDefaults returns default RTO/RPO for a service tier
func GetTierDefaults(tier ServiceTier) RTORPOConfig {
	switch tier {
	case TierCritical:
		return RTORPOConfig{
			RTO:            time.Minute * 1,
			RPO:            time.Second * 30,
			Tier:           TierCritical,
			AlertThreshold: 0.8,
		}
	case TierStandard:
		return RTORPOConfig{
			RTO:            time.Minute * 15,
			RPO:            time.Minute * 5,
			Tier:           TierStandard,
			AlertThreshold: 0.8,
		}
	case TierBestEffort:
		return RTORPOConfig{
			RTO:            time.Hour * 4,
			RPO:            time.Hour * 1,
			Tier:           TierBestEffort,
			AlertThreshold: 0.9,
		}
	default:
		return GetTierDefaults(TierStandard)
	}
}

// RecoveryEvent represents a recovery incident
type RecoveryEvent struct {
	IncidentID   string
	BackendName  string
	FailureTime  time.Time
	RecoveryTime time.Time
	DataLoss     time.Duration // Amount of data loss (time-based)
	Successful   bool
}

// RecoveryResult contains the outcome of a recovery event
type RecoveryResult struct {
	IncidentID string
	RTOMet     bool
	RPOMet     bool
	ActualRTO  time.Duration
	ActualRPO  time.Duration
	Timestamp  time.Time
}

// RTORPOMetrics contains aggregated metrics
type RTORPOMetrics struct {
	TotalIncidents    int
	RTOCompliant      int
	RPOCompliant      int
	RTOComplianceRate float64
	RPOComplianceRate float64
	AverageRTO        time.Duration
	AverageRPO        time.Duration
	WorstRTO          time.Duration
	WorstRPO          time.Duration
}

// StatusCheck represents the current RTO/RPO status
type StatusCheck struct {
	Status          RTORPOStatus
	RTOAtRisk       bool
	RPOAtRisk       bool
	RTOBreached     bool
	RPOBreached     bool
	ActiveIncidents int
	Message         string
	CheckedAt       time.Time
}

// SLAReport represents a periodic SLA compliance report
type SLAReport struct {
	GeneratedAt          time.Time
	PeriodStart          time.Time
	PeriodEnd            time.Time
	Tier                 ServiceTier
	TotalIncidents       int
	RTOTarget            time.Duration
	RPOTarget            time.Duration
	RTOCompliancePercent float64
	RPOCompliancePercent float64
	AverageRecoveryTime  time.Duration
	AverageDataLoss      time.Duration
	Incidents            []RecoveryResult
}

// activeIncident tracks an ongoing incident
type activeIncident struct {
	ID          string
	BackendName string
	StartTime   time.Time
}

// RTORPOTracker tracks RTO/RPO compliance
type RTORPOTracker struct {
	config          RTORPOConfig
	backendConfigs  map[string]RTORPOConfig
	history         []RecoveryResult
	activeIncidents map[string]*activeIncident
	mu              sync.RWMutex
}

// NewRTORPOTracker creates a new tracker with a single config
func NewRTORPOTracker(config RTORPOConfig) (*RTORPOTracker, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &RTORPOTracker{
		config:          config,
		backendConfigs:  make(map[string]RTORPOConfig),
		history:         make([]RecoveryResult, 0),
		activeIncidents: make(map[string]*activeIncident),
	}, nil
}

// NewRTORPOTrackerWithBackends creates a tracker with per-backend configs
func NewRTORPOTrackerWithBackends(configs map[string]RTORPOConfig) (*RTORPOTracker, error) {
	if len(configs) == 0 {
		return nil, errors.New("at least one backend config required")
	}

	// Validate all configs
	for name, cfg := range configs {
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("invalid config for backend %s: %w", name, err)
		}
	}

	// Use first config as default
	var defaultConfig RTORPOConfig
	for _, cfg := range configs {
		defaultConfig = cfg
		break
	}

	return &RTORPOTracker{
		config:          defaultConfig,
		backendConfigs:  configs,
		history:         make([]RecoveryResult, 0),
		activeIncidents: make(map[string]*activeIncident),
	}, nil
}

// Tier returns the service tier
func (t *RTORPOTracker) Tier() ServiceTier {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.config.Tier
}

// GetBackendConfig returns config for a specific backend
func (t *RTORPOTracker) GetBackendConfig(backendName string) RTORPOConfig {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if cfg, ok := t.backendConfigs[backendName]; ok {
		return cfg
	}
	return t.config
}

// StartIncident begins tracking a new incident
func (t *RTORPOTracker) StartIncident(incidentID, backendName string, failureTime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.activeIncidents[incidentID] = &activeIncident{
		ID:          incidentID,
		BackendName: backendName,
		StartTime:   failureTime,
	}
}

// HasActiveIncident checks if an incident is currently active
func (t *RTORPOTracker) HasActiveIncident(incidentID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	_, exists := t.activeIncidents[incidentID]
	return exists
}

// ResolveIncident resolves an active incident and records the result
func (t *RTORPOTracker) ResolveIncident(incidentID string, dataLoss time.Duration) (RecoveryResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	incident, exists := t.activeIncidents[incidentID]
	if !exists {
		return RecoveryResult{}, fmt.Errorf("incident %s not found", incidentID)
	}

	now := time.Now()
	event := RecoveryEvent{
		IncidentID:   incidentID,
		BackendName:  incident.BackendName,
		FailureTime:  incident.StartTime,
		RecoveryTime: now,
		DataLoss:     dataLoss,
		Successful:   true,
	}

	result := t.recordRecoveryLocked(event)
	delete(t.activeIncidents, incidentID)

	return result, nil
}

// RecordRecovery records a recovery event and returns the result
func (t *RTORPOTracker) RecordRecovery(event RecoveryEvent) RecoveryResult {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.recordRecoveryLocked(event)
}

// recordRecoveryLocked records recovery without acquiring lock (caller must hold lock)
func (t *RTORPOTracker) recordRecoveryLocked(event RecoveryEvent) RecoveryResult {
	// Get config for this backend
	cfg := t.config
	if bcfg, ok := t.backendConfigs[event.BackendName]; ok {
		cfg = bcfg
	}

	actualRTO := event.RecoveryTime.Sub(event.FailureTime)
	actualRPO := event.DataLoss

	result := RecoveryResult{
		IncidentID: event.IncidentID,
		RTOMet:     actualRTO <= cfg.RTO,
		RPOMet:     actualRPO <= cfg.RPO,
		ActualRTO:  actualRTO,
		ActualRPO:  actualRPO,
		Timestamp:  event.RecoveryTime, // Use event's recovery time, not time.Now()
	}

	t.history = append(t.history, result)

	return result
}

// GetMetrics returns aggregated metrics from history
func (t *RTORPOTracker) GetMetrics() RTORPOMetrics {
	t.mu.RLock()
	defer t.mu.RUnlock()

	metrics := RTORPOMetrics{
		TotalIncidents:    len(t.history),
		RTOComplianceRate: 100.0,
		RPOComplianceRate: 100.0,
	}

	if len(t.history) == 0 {
		return metrics
	}

	var totalRTO, totalRPO time.Duration

	for _, result := range t.history {
		if result.RTOMet {
			metrics.RTOCompliant++
		}
		if result.RPOMet {
			metrics.RPOCompliant++
		}

		totalRTO += result.ActualRTO
		totalRPO += result.ActualRPO

		if result.ActualRTO > metrics.WorstRTO {
			metrics.WorstRTO = result.ActualRTO
		}
		if result.ActualRPO > metrics.WorstRPO {
			metrics.WorstRPO = result.ActualRPO
		}
	}

	metrics.RTOComplianceRate = float64(metrics.RTOCompliant) / float64(metrics.TotalIncidents) * 100
	metrics.RPOComplianceRate = float64(metrics.RPOCompliant) / float64(metrics.TotalIncidents) * 100
	metrics.AverageRTO = totalRTO / time.Duration(metrics.TotalIncidents)
	metrics.AverageRPO = totalRPO / time.Duration(metrics.TotalIncidents)

	return metrics
}

// CheckStatus returns the current RTO/RPO status
func (t *RTORPOTracker) CheckStatus(ctx context.Context) StatusCheck {
	t.mu.RLock()
	defer t.mu.RUnlock()

	status := StatusCheck{
		Status:          StatusHealthy,
		ActiveIncidents: len(t.activeIncidents),
		CheckedAt:       time.Now(),
	}

	if len(t.activeIncidents) == 0 {
		status.Message = "No active incidents"
		return status
	}

	now := time.Now()
	rtoThreshold := time.Duration(float64(t.config.RTO) * t.config.AlertThreshold)

	for _, incident := range t.activeIncidents {
		elapsed := now.Sub(incident.StartTime)

		// Check for RTO breach
		if elapsed > t.config.RTO {
			status.RTOBreached = true
			status.RTOAtRisk = true
			status.Status = StatusCritical
			status.Message = fmt.Sprintf("RTO breached for incident %s", incident.ID)
		} else if elapsed > rtoThreshold {
			status.RTOAtRisk = true
			if status.Status != StatusCritical {
				status.Status = StatusWarning
				status.Message = fmt.Sprintf("Approaching RTO threshold for incident %s", incident.ID)
			}
		}
	}

	return status
}

// GenerateSLAReport generates a report for a time period
func (t *RTORPOTracker) GenerateSLAReport(start, end time.Time) SLAReport {
	t.mu.RLock()
	defer t.mu.RUnlock()

	report := SLAReport{
		GeneratedAt:          time.Now(),
		PeriodStart:          start,
		PeriodEnd:            end,
		Tier:                 t.config.Tier,
		RTOTarget:            t.config.RTO,
		RPOTarget:            t.config.RPO,
		RTOCompliancePercent: 100.0,
		RPOCompliancePercent: 100.0,
		Incidents:            make([]RecoveryResult, 0),
	}

	var rtoCompliant, rpoCompliant int
	var totalRecoveryTime, totalDataLoss time.Duration

	for _, result := range t.history {
		// Filter by time period
		if result.Timestamp.Before(start) || result.Timestamp.After(end) {
			continue
		}

		report.Incidents = append(report.Incidents, result)
		report.TotalIncidents++

		if result.RTOMet {
			rtoCompliant++
		}
		if result.RPOMet {
			rpoCompliant++
		}

		totalRecoveryTime += result.ActualRTO
		totalDataLoss += result.ActualRPO
	}

	if report.TotalIncidents > 0 {
		report.RTOCompliancePercent = float64(rtoCompliant) / float64(report.TotalIncidents) * 100
		report.RPOCompliancePercent = float64(rpoCompliant) / float64(report.TotalIncidents) * 100
		report.AverageRecoveryTime = totalRecoveryTime / time.Duration(report.TotalIncidents)
		report.AverageDataLoss = totalDataLoss / time.Duration(report.TotalIncidents)
	}

	return report
}
