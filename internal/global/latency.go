// internal/global/latency.go
package global

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"
)

// LatencyMeasurement represents a single latency measurement
type LatencyMeasurement struct {
	LocationID string
	Latency    time.Duration
	Timestamp  time.Time
	Success    bool
	Error      string
}

// LatencyStats contains statistical data about latency
type LatencyStats struct {
	LocationID  string
	SampleCount int
	Min         time.Duration
	Max         time.Duration
	Avg         time.Duration
	P50         time.Duration
	P90         time.Duration
	P99         time.Duration
	StdDev      time.Duration
	LastUpdated time.Time
	SuccessRate float64
}

// LatencyOptimizer optimizes routing based on latency measurements
type LatencyOptimizer struct {
	mu           sync.RWMutex
	measurements map[string][]LatencyMeasurement
	stats        map[string]*LatencyStats
	edgeManager  *EdgeManager
	config       *LatencyConfig
	probeStop    chan struct{}
	probeWg      sync.WaitGroup
}

// LatencyConfig configures the latency optimizer
type LatencyConfig struct {
	MaxSamples         int
	SampleWindow       time.Duration
	ProbeInterval      time.Duration
	ProbeTimeout       time.Duration
	StaleThreshold     time.Duration
	EnableAutoProbe    bool
	MinSamplesForStats int
}

// DefaultLatencyConfig returns default configuration
func DefaultLatencyConfig() *LatencyConfig {
	return &LatencyConfig{
		MaxSamples:         100,
		SampleWindow:       time.Hour,
		ProbeInterval:      30 * time.Second,
		ProbeTimeout:       5 * time.Second,
		StaleThreshold:     5 * time.Minute,
		EnableAutoProbe:    false,
		MinSamplesForStats: 5,
	}
}

// NewLatencyOptimizer creates a new latency optimizer
func NewLatencyOptimizer(edgeManager *EdgeManager, config *LatencyConfig) *LatencyOptimizer {
	if config == nil {
		config = DefaultLatencyConfig()
	}

	return &LatencyOptimizer{
		measurements: make(map[string][]LatencyMeasurement),
		stats:        make(map[string]*LatencyStats),
		edgeManager:  edgeManager,
		config:       config,
		probeStop:    make(chan struct{}),
	}
}

// RecordMeasurement records a latency measurement
func (lo *LatencyOptimizer) RecordMeasurement(m LatencyMeasurement) {
	lo.mu.Lock()
	defer lo.mu.Unlock()

	if m.Timestamp.IsZero() {
		m.Timestamp = time.Now()
	}

	measurements := lo.measurements[m.LocationID]
	measurements = append(measurements, m)

	// Trim old measurements
	cutoff := time.Now().Add(-lo.config.SampleWindow)
	var trimmed []LatencyMeasurement
	for _, measurement := range measurements {
		if measurement.Timestamp.After(cutoff) {
			trimmed = append(trimmed, measurement)
		}
	}

	// Limit to max samples (only if MaxSamples is set)
	if lo.config.MaxSamples > 0 && len(trimmed) > lo.config.MaxSamples {
		trimmed = trimmed[len(trimmed)-lo.config.MaxSamples:]
	}

	lo.measurements[m.LocationID] = trimmed
	lo.updateStats(m.LocationID)
}

func (lo *LatencyOptimizer) updateStats(locationID string) {
	measurements := lo.measurements[locationID]
	if len(measurements) < lo.config.MinSamplesForStats {
		return
	}

	// Collect successful measurements
	var latencies []time.Duration
	successCount := 0
	for _, m := range measurements {
		if m.Success {
			latencies = append(latencies, m.Latency)
			successCount++
		}
	}

	if len(latencies) == 0 {
		lo.stats[locationID] = &LatencyStats{
			LocationID:  locationID,
			SampleCount: len(measurements),
			SuccessRate: 0,
			LastUpdated: time.Now(),
		}
		return
	}

	// Sort for percentiles
	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	stats := &LatencyStats{
		LocationID:  locationID,
		SampleCount: len(measurements),
		Min:         latencies[0],
		Max:         latencies[len(latencies)-1],
		P50:         percentile(latencies, 50),
		P90:         percentile(latencies, 90),
		P99:         percentile(latencies, 99),
		SuccessRate: float64(successCount) / float64(len(measurements)),
		LastUpdated: time.Now(),
	}

	// Calculate average
	var sum time.Duration
	for _, l := range latencies {
		sum += l
	}
	stats.Avg = sum / time.Duration(len(latencies))

	// Calculate standard deviation
	var variance float64
	avgNs := float64(stats.Avg.Nanoseconds())
	for _, l := range latencies {
		diff := float64(l.Nanoseconds()) - avgNs
		variance += diff * diff
	}
	variance /= float64(len(latencies))
	stats.StdDev = time.Duration(math.Sqrt(variance))

	lo.stats[locationID] = stats
}

func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (p * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// GetStats returns latency stats for a location
func (lo *LatencyOptimizer) GetStats(locationID string) (*LatencyStats, bool) {
	lo.mu.RLock()
	defer lo.mu.RUnlock()
	stats, ok := lo.stats[locationID]
	return stats, ok
}

// GetAllStats returns stats for all locations
func (lo *LatencyOptimizer) GetAllStats() map[string]*LatencyStats {
	lo.mu.RLock()
	defer lo.mu.RUnlock()

	result := make(map[string]*LatencyStats)
	for k, v := range lo.stats {
		result[k] = v
	}
	return result
}

// GetMeasurements returns raw measurements for a location
func (lo *LatencyOptimizer) GetMeasurements(locationID string) []LatencyMeasurement {
	lo.mu.RLock()
	defer lo.mu.RUnlock()

	measurements := lo.measurements[locationID]
	result := make([]LatencyMeasurement, len(measurements))
	copy(result, measurements)
	return result
}

// ClearMeasurements clears all measurements for a location
func (lo *LatencyOptimizer) ClearMeasurements(locationID string) {
	lo.mu.Lock()
	defer lo.mu.Unlock()

	delete(lo.measurements, locationID)
	delete(lo.stats, locationID)
}

// ClearAllMeasurements clears all measurements
func (lo *LatencyOptimizer) ClearAllMeasurements() {
	lo.mu.Lock()
	defer lo.mu.Unlock()

	lo.measurements = make(map[string][]LatencyMeasurement)
	lo.stats = make(map[string]*LatencyStats)
}

// LocationScore represents a scored location for routing
type LocationScore struct {
	LocationID    string
	Location      *EdgeLocation
	LatencyScore  float64
	DistanceScore float64
	CapacityScore float64
	TotalScore    float64
	Latency       time.Duration
	Distance      float64
}

// ScoreWeights defines weights for scoring factors
type ScoreWeights struct {
	Latency  float64
	Distance float64
	Capacity float64
}

// DefaultScoreWeights returns default scoring weights
func DefaultScoreWeights() ScoreWeights {
	return ScoreWeights{
		Latency:  0.5,
		Distance: 0.3,
		Capacity: 0.2,
	}
}

// RankLocations ranks locations by latency and other factors
func (lo *LatencyOptimizer) RankLocations(lat, lon float64, weights ScoreWeights) []LocationScore {
	lo.mu.RLock()
	defer lo.mu.RUnlock()

	locations := lo.edgeManager.GetEnabledLocations()
	scores := make([]LocationScore, 0, len(locations))

	// Find max values for normalization
	var maxLatency time.Duration
	var maxDistance float64

	for _, loc := range locations {
		dist := haversineDistance(lat, lon, loc.Latitude, loc.Longitude)
		if dist > maxDistance {
			maxDistance = dist
		}

		if stats, ok := lo.stats[loc.ID]; ok && stats.P50 > maxLatency {
			maxLatency = stats.P50
		}
	}

	// Default max values if not enough data
	if maxLatency == 0 {
		maxLatency = 500 * time.Millisecond
	}
	if maxDistance == 0 {
		maxDistance = 20000 // km
	}

	for _, loc := range locations {
		score := LocationScore{
			LocationID: loc.ID,
			Location:   loc,
		}

		// Distance score (lower is better, normalize to 0-1)
		score.Distance = haversineDistance(lat, lon, loc.Latitude, loc.Longitude)
		score.DistanceScore = 1.0 - (score.Distance / maxDistance)

		// Latency score (lower is better, normalize to 0-1)
		if stats, ok := lo.stats[loc.ID]; ok {
			score.Latency = stats.P50
			score.LatencyScore = 1.0 - (float64(stats.P50) / float64(maxLatency))
			// Factor in success rate
			score.LatencyScore *= stats.SuccessRate
		} else {
			// No latency data, use distance as proxy
			score.LatencyScore = score.DistanceScore * 0.8
			score.Latency = time.Duration(score.Distance/300) * time.Millisecond // rough estimate
		}

		// Capacity score (based on load if status available)
		if loc.Capacity > 0 {
			// Use status CPU/memory percentage if available
			status, ok := lo.edgeManager.GetStatus(loc.ID)
			if ok && status != nil {
				// Average of CPU and memory as load indicator
				avgLoad := (status.CPUPercent + status.MemoryPercent) / 200.0 // normalize to 0-1
				score.CapacityScore = 1.0 - avgLoad
			} else {
				score.CapacityScore = 1.0
			}
		} else {
			score.CapacityScore = 1.0
		}

		// Calculate weighted total
		score.TotalScore = score.LatencyScore*weights.Latency +
			score.DistanceScore*weights.Distance +
			score.CapacityScore*weights.Capacity

		scores = append(scores, score)
	}

	// Sort by total score descending
	sort.Slice(scores, func(i, j int) bool {
		return scores[i].TotalScore > scores[j].TotalScore
	})

	return scores
}

// GetOptimalLocation returns the best location based on latency
func (lo *LatencyOptimizer) GetOptimalLocation(lat, lon float64) *EdgeLocation {
	scores := lo.RankLocations(lat, lon, DefaultScoreWeights())
	if len(scores) == 0 {
		return nil
	}
	return scores[0].Location
}

// GetOptimalLocations returns top N locations
func (lo *LatencyOptimizer) GetOptimalLocations(lat, lon float64, n int) []*EdgeLocation {
	scores := lo.RankLocations(lat, lon, DefaultScoreWeights())

	result := make([]*EdgeLocation, 0, n)
	for i := 0; i < len(scores) && i < n; i++ {
		result = append(result, scores[i].Location)
	}
	return result
}

// LatencyProbe defines a function that measures latency to a location
type LatencyProbe func(ctx context.Context, locationID string) (time.Duration, error)

// StartAutoProbe starts automatic latency probing
func (lo *LatencyOptimizer) StartAutoProbe(probe LatencyProbe) {
	if !lo.config.EnableAutoProbe {
		return
	}

	lo.probeWg.Add(1)
	go lo.autoProbeLoop(probe)
}

// StopAutoProbe stops automatic probing
func (lo *LatencyOptimizer) StopAutoProbe() {
	close(lo.probeStop)
	lo.probeWg.Wait()
}

func (lo *LatencyOptimizer) autoProbeLoop(probe LatencyProbe) {
	defer lo.probeWg.Done()

	ticker := time.NewTicker(lo.config.ProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			lo.probeAllLocations(probe)
		case <-lo.probeStop:
			return
		}
	}
}

func (lo *LatencyOptimizer) probeAllLocations(probe LatencyProbe) {
	locations := lo.edgeManager.GetEnabledLocations()

	var wg sync.WaitGroup
	for _, loc := range locations {
		wg.Add(1)
		go func(locationID string) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), lo.config.ProbeTimeout)
			defer cancel()

			latency, err := probe(ctx, locationID)
			m := LatencyMeasurement{
				LocationID: locationID,
				Latency:    latency,
				Timestamp:  time.Now(),
				Success:    err == nil,
			}
			if err != nil {
				m.Error = err.Error()
			}
			lo.RecordMeasurement(m)
		}(loc.ID)
	}
	wg.Wait()
}

// ProbeLocation manually probes a single location
func (lo *LatencyOptimizer) ProbeLocation(ctx context.Context, locationID string, probe LatencyProbe) (*LatencyMeasurement, error) {
	latency, err := probe(ctx, locationID)
	m := LatencyMeasurement{
		LocationID: locationID,
		Latency:    latency,
		Timestamp:  time.Now(),
		Success:    err == nil,
	}
	if err != nil {
		m.Error = err.Error()
	}
	lo.RecordMeasurement(m)
	return &m, err
}

// IsStale checks if latency data is stale for a location
func (lo *LatencyOptimizer) IsStale(locationID string) bool {
	lo.mu.RLock()
	defer lo.mu.RUnlock()

	stats, ok := lo.stats[locationID]
	if !ok {
		return true
	}
	return time.Since(stats.LastUpdated) > lo.config.StaleThreshold
}

// GetStaleLocations returns locations with stale latency data
func (lo *LatencyOptimizer) GetStaleLocations() []string {
	lo.mu.RLock()
	defer lo.mu.RUnlock()

	locations := lo.edgeManager.GetEnabledLocations()
	var stale []string

	for _, loc := range locations {
		if stats, ok := lo.stats[loc.ID]; !ok || time.Since(stats.LastUpdated) > lo.config.StaleThreshold {
			stale = append(stale, loc.ID)
		}
	}
	return stale
}

// LatencyReport generates a latency report
type LatencyReport struct {
	GeneratedAt       time.Time
	TotalLocations    int
	LocationsWithData int
	StaleLocations    int
	BestLocation      string
	BestLatency       time.Duration
	WorstLocation     string
	WorstLatency      time.Duration
	AverageLatency    time.Duration
	LocationDetails   map[string]*LatencyStats
}

// GenerateReport generates a latency report
func (lo *LatencyOptimizer) GenerateReport() *LatencyReport {
	lo.mu.RLock()
	defer lo.mu.RUnlock()

	report := &LatencyReport{
		GeneratedAt:     time.Now(),
		TotalLocations:  len(lo.edgeManager.GetEnabledLocations()),
		LocationDetails: make(map[string]*LatencyStats),
	}

	var totalLatency time.Duration
	var bestLatency, worstLatency time.Duration
	var bestLoc, worstLoc string
	first := true

	for locID, stats := range lo.stats {
		report.LocationsWithData++
		report.LocationDetails[locID] = stats

		if stats.P50 > 0 {
			totalLatency += stats.P50

			if first || stats.P50 < bestLatency {
				bestLatency = stats.P50
				bestLoc = locID
			}
			if first || stats.P50 > worstLatency {
				worstLatency = stats.P50
				worstLoc = locID
			}
			first = false
		}

		if time.Since(stats.LastUpdated) > lo.config.StaleThreshold {
			report.StaleLocations++
		}
	}

	report.BestLocation = bestLoc
	report.BestLatency = bestLatency
	report.WorstLocation = worstLoc
	report.WorstLatency = worstLatency

	if report.LocationsWithData > 0 {
		report.AverageLatency = totalLatency / time.Duration(report.LocationsWithData)
	}

	return report
}

// LatencyBudget defines latency requirements
type LatencyBudget struct {
	MaxP50         time.Duration
	MaxP99         time.Duration
	MinSuccessRate float64
}

// MeetsBudget checks if a location meets the latency budget
func (lo *LatencyOptimizer) MeetsBudget(locationID string, budget LatencyBudget) bool {
	lo.mu.RLock()
	defer lo.mu.RUnlock()

	stats, ok := lo.stats[locationID]
	if !ok {
		return false
	}

	if budget.MaxP50 > 0 && stats.P50 > budget.MaxP50 {
		return false
	}
	if budget.MaxP99 > 0 && stats.P99 > budget.MaxP99 {
		return false
	}
	if budget.MinSuccessRate > 0 && stats.SuccessRate < budget.MinSuccessRate {
		return false
	}

	return true
}

// GetLocationsWithinBudget returns locations meeting the budget
func (lo *LatencyOptimizer) GetLocationsWithinBudget(budget LatencyBudget) []*EdgeLocation {
	lo.mu.RLock()
	defer lo.mu.RUnlock()

	var result []*EdgeLocation
	for locID, stats := range lo.stats {
		meetsBudget := true
		if budget.MaxP50 > 0 && stats.P50 > budget.MaxP50 {
			meetsBudget = false
		}
		if budget.MaxP99 > 0 && stats.P99 > budget.MaxP99 {
			meetsBudget = false
		}
		if budget.MinSuccessRate > 0 && stats.SuccessRate < budget.MinSuccessRate {
			meetsBudget = false
		}

		if meetsBudget {
			if loc, ok := lo.edgeManager.GetLocation(locID); ok {
				result = append(result, loc)
			}
		}
	}
	return result
}
