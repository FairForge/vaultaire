// internal/cache/patterns.go
package cache

import (
	"sync"
	"time"
)

// AccessPattern represents analyzed access patterns
type AccessPatternAnalysis struct {
	Key            string
	AccessCount    int64
	LastAccess     time.Time
	AccessInterval time.Duration
	Predictability float64 // 0-1 score
}

// PatternAnalyzer tracks and analyzes access patterns
type PatternAnalyzer struct {
	mu       sync.RWMutex
	patterns map[string]*AccessPatternAnalysis
	window   time.Duration
}

// NewPatternAnalyzer creates a pattern analyzer
func NewPatternAnalyzer(window time.Duration) *PatternAnalyzer {
	return &PatternAnalyzer{
		patterns: make(map[string]*AccessPatternAnalysis),
		window:   window,
	}
}

// TrackAccess records an access to a key
func (p *PatternAnalyzer) TrackAccess(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	if pattern, exists := p.patterns[key]; exists {
		// Calculate interval
		interval := now.Sub(pattern.LastAccess)

		// Update running average of intervals
		if pattern.AccessCount > 0 {
			avgInterval := pattern.AccessInterval
			pattern.AccessInterval = time.Duration(
				(avgInterval.Nanoseconds()*pattern.AccessCount + interval.Nanoseconds()) /
					(pattern.AccessCount + 1),
			)
		}

		pattern.AccessCount++
		pattern.LastAccess = now

		// Calculate predictability based on interval consistency
		p.calculatePredictability(pattern)
	} else {
		p.patterns[key] = &AccessPatternAnalysis{
			Key:         key,
			AccessCount: 1,
			LastAccess:  now,
		}
	}
}

// GetPattern returns the access pattern for a key
func (p *PatternAnalyzer) GetPattern(key string) *AccessPatternAnalysis {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.patterns[key]
}

// GetHotKeys returns the most frequently accessed keys
func (p *PatternAnalyzer) GetHotKeys(limit int) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Sort by access count
	type kv struct {
		Key   string
		Count int64
	}

	var sorted []kv
	for k, v := range p.patterns {
		sorted = append(sorted, kv{k, v.AccessCount})
	}

	// Simple bubble sort for hot keys
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].Count > sorted[i].Count {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	result := make([]string, 0, limit)
	for i := 0; i < limit && i < len(sorted); i++ {
		result = append(result, sorted[i].Key)
	}

	return result
}

func (p *PatternAnalyzer) calculatePredictability(pattern *AccessPatternAnalysis) {
	// Simple predictability: consistent intervals = high predictability
	// This is a simplified version - production would use standard deviation
	pattern.Predictability = 0.8 // Simplified for now
}
