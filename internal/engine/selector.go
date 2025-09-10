// internal/engine/selector.go
package engine

import (
	"context"
	"sort"
	"sync"
)

// BackendSelector chooses the best backend based on health
type BackendSelector struct {
	mu      sync.RWMutex
	scores  map[string]float64
	weights map[string]float64 // Per-backend preference weights
}

// NewBackendSelector creates a selector
func NewBackendSelector() *BackendSelector {
	return &BackendSelector{
		scores:  make(map[string]float64),
		weights: make(map[string]float64),
	}
}

// UpdateScore updates a backend's health score
func (s *BackendSelector) UpdateScore(backendID string, score float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scores[backendID] = score
}

// SelectBackend chooses the best backend for an operation
func (s *BackendSelector) SelectBackend(ctx context.Context, operation string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type candidate struct {
		id    string
		score float64
	}

	var candidates []candidate

	// Filter out unhealthy backends (score < 50)
	for id, score := range s.scores {
		if score >= 50.0 {
			candidates = append(candidates, candidate{id: id, score: score})
		}
	}

	if len(candidates) == 0 {
		// No healthy backends, return any available
		for id := range s.scores {
			return id
		}
		return ""
	}

	// Sort by score (highest first)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	// Return the best one
	return candidates[0].id
}

// SelectBackendWithFallback returns primary and fallback options
func (s *BackendSelector) SelectBackendWithFallback(ctx context.Context, operation string) (primary, fallback string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type candidate struct {
		id    string
		score float64
	}

	var candidates []candidate
	for id, score := range s.scores {
		if score >= 30.0 { // Lower threshold for fallback
			candidates = append(candidates, candidate{id: id, score: score})
		}
	}

	if len(candidates) == 0 {
		return "", ""
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	primary = candidates[0].id
	if len(candidates) > 1 {
		fallback = candidates[1].id
	}

	return primary, fallback
}

func (s *BackendSelector) GetHealthyBackends(ctx context.Context) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var healthy []string
	for id, score := range s.scores {
		if score >= 50.0 {
			healthy = append(healthy, id)
		}
	}
	return healthy
}
