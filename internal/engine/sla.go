// internal/engine/sla.go
package engine

import (
	"sync"
	"time"
)

// SLAMonitor tracks SLA compliance
type SLAMonitor struct {
	mu           sync.RWMutex
	requirements map[string]SLARequirements
	metrics      map[string]*SLAMetrics
}

// SLARequirements defines service level requirements
type SLARequirements struct {
	UptimePercent float64       // e.g., 99.9
	MaxLatencyP99 time.Duration // P99 latency threshold
	MaxLatencyP50 time.Duration // P50 latency threshold
	MinThroughput int64         // Bytes per second
	MaxErrorRate  float64       // Maximum error percentage
}

// SLAMetrics tracks actual performance
type SLAMetrics struct {
	TotalRequests   int
	SuccessRequests int
	Latencies       []time.Duration
	StartTime       time.Time
	LastDowntime    time.Time
	DowntimeTotal   time.Duration
}

// SLACompliance represents compliance status
type SLACompliance struct {
	MeetsUptime     bool
	MeetsLatency    bool
	MeetsThroughput bool
	MeetsErrorRate  bool
	UptimePercent   float64
	CurrentP99      time.Duration
	CurrentP50      time.Duration
}

// SLAViolation represents an SLA breach
type SLAViolation struct {
	Type      string
	Timestamp time.Time
	Details   string
	Severity  string // "WARNING", "CRITICAL"
}

// NewSLAMonitor creates an SLA monitor
func NewSLAMonitor() *SLAMonitor {
	return &SLAMonitor{
		requirements: make(map[string]SLARequirements),
		metrics:      make(map[string]*SLAMetrics),
	}
}

// DefineSLA sets SLA requirements for a backend
func (s *SLAMonitor) DefineSLA(backend string, req SLARequirements) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.requirements[backend] = req
	if _, ok := s.metrics[backend]; !ok {
		s.metrics[backend] = &SLAMetrics{
			StartTime: time.Now(),
		}
	}
}

// RecordOperation records an operation for SLA tracking
func (s *SLAMonitor) RecordOperation(backend string, latency time.Duration, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	m, ok := s.metrics[backend]
	if !ok {
		m = &SLAMetrics{StartTime: time.Now()}
		s.metrics[backend] = m
	}

	m.TotalRequests++
	if success {
		m.SuccessRequests++
	}
	m.Latencies = append(m.Latencies, latency)

	// Keep only last 10000 latencies
	if len(m.Latencies) > 10000 {
		m.Latencies = m.Latencies[1:]
	}
}

// GetCompliance checks SLA compliance
func (s *SLAMonitor) GetCompliance(backend string) SLACompliance {
	s.mu.RLock()
	defer s.mu.RUnlock()

	req, hasReq := s.requirements[backend]
	metrics, hasMetrics := s.metrics[backend]

	if !hasReq || !hasMetrics {
		return SLACompliance{}
	}

	compliance := SLACompliance{}

	// Calculate uptime
	if metrics.TotalRequests > 0 {
		compliance.UptimePercent = float64(metrics.SuccessRequests) / float64(metrics.TotalRequests) * 100
		compliance.MeetsUptime = compliance.UptimePercent >= req.UptimePercent
	}

	// Calculate latency percentiles
	if len(metrics.Latencies) > 0 {
		compliance.CurrentP99 = percentile(metrics.Latencies, 99)
		compliance.CurrentP50 = percentile(metrics.Latencies, 50)

		if req.MaxLatencyP99 > 0 {
			compliance.MeetsLatency = compliance.CurrentP99 <= req.MaxLatencyP99
		}
	}

	// Error rate
	errorRate := float64(metrics.TotalRequests-metrics.SuccessRequests) / float64(metrics.TotalRequests)
	compliance.MeetsErrorRate = errorRate <= req.MaxErrorRate

	return compliance
}

// GetViolations returns current SLA violations
func (s *SLAMonitor) GetViolations(backend string) []SLAViolation {
	compliance := s.GetCompliance(backend)

	var violations []SLAViolation

	if !compliance.MeetsUptime {
		violations = append(violations, SLAViolation{
			Type:      "UPTIME_VIOLATION",
			Timestamp: time.Now(),
			Details:   "Uptime below required threshold",
			Severity:  "CRITICAL",
		})
	}

	if !compliance.MeetsLatency {
		violations = append(violations, SLAViolation{
			Type:      "LATENCY_VIOLATION",
			Timestamp: time.Now(),
			Details:   "P99 latency exceeds threshold",
			Severity:  "WARNING",
		})
	}

	if !compliance.MeetsErrorRate {
		violations = append(violations, SLAViolation{
			Type:      "ERROR_RATE_VIOLATION",
			Timestamp: time.Now(),
			Details:   "Error rate exceeds maximum",
			Severity:  "CRITICAL",
		})
	}

	return violations
}

// GenerateReport creates an SLA compliance report
func (s *SLAMonitor) GenerateReport(backend string) SLAReport {
	compliance := s.GetCompliance(backend)
	violations := s.GetViolations(backend)

	return SLAReport{
		Backend:    backend,
		Period:     time.Since(s.metrics[backend].StartTime),
		Compliance: compliance,
		Violations: violations,
		Generated:  time.Now(),
	}
}

// SLAReport represents a compliance report
type SLAReport struct {
	Backend    string
	Period     time.Duration
	Compliance SLACompliance
	Violations []SLAViolation
	Generated  time.Time
}
