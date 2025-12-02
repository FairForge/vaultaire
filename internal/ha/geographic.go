package ha

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Region represents a geographic region
type Region string

const (
	RegionNYC Region = "nyc" // Primary Hub (NYC)
	RegionLA  Region = "la"  // Secondary (Los Angeles)
)

// RegionInfo contains metadata about a region
type RegionInfo struct {
	Region      Region        `json:"region"`
	DisplayName string        `json:"display_name"`
	Location    string        `json:"location"`
	Latency     time.Duration `json:"latency_ms"`
	Tier        RegionTier    `json:"tier"`
	Active      bool          `json:"active"`
}

// RegionTier indicates the region's role
type RegionTier string

const (
	TierPrimary   RegionTier = "primary"
	TierSecondary RegionTier = "secondary"
	TierEdge      RegionTier = "edge"
)

// ReplicationPolicy defines how data replicates between regions
type ReplicationPolicy struct {
	Name           string        `json:"name"`
	SourceRegion   Region        `json:"source_region"`
	TargetRegions  []Region      `json:"target_regions"`
	Mode           RepMode       `json:"mode"`
	MaxLagDuration time.Duration `json:"max_lag_duration"`
	Priority       int           `json:"priority"`
}

// RepMode defines replication behavior
type RepMode string

const (
	RepModeSync   RepMode = "sync"
	RepModeAsync  RepMode = "async"
	RepModeQuorum RepMode = "quorum"
)

// GeoConfig holds geographic configuration
type GeoConfig struct {
	PrimaryRegion Region
	Regions       map[Region]*RegionInfo
	Policies      []*ReplicationPolicy
	AffinityRules []AffinityRule
}

// AffinityRule defines region routing preferences
type AffinityRule struct {
	Name          string   `json:"name"`
	SourceRegions []Region `json:"source_regions"`
	TargetRegion  Region   `json:"target_region"`
	Weight        int      `json:"weight"`
}

// GeoManager manages geographic redundancy
type GeoManager struct {
	config      *GeoConfig
	latencies   map[Region]time.Duration
	healthState map[Region]BackendState // Uses existing BackendState type
	mu          sync.RWMutex
}

// NewGeoManager creates a new geographic manager
func NewGeoManager(config *GeoConfig) (*GeoManager, error) {
	if config == nil {
		return nil, fmt.Errorf("config required")
	}
	if config.PrimaryRegion == "" {
		return nil, fmt.Errorf("primary region required")
	}
	if config.Regions == nil {
		config.Regions = make(map[Region]*RegionInfo)
	}

	return &GeoManager{
		config:      config,
		latencies:   make(map[Region]time.Duration),
		healthState: make(map[Region]BackendState),
	}, nil
}

// DefaultGeoConfig returns config for your 2x 9950X servers (NYC + LA)
func DefaultGeoConfig() *GeoConfig {
	return &GeoConfig{
		PrimaryRegion: RegionNYC,
		Regions: map[Region]*RegionInfo{
			RegionNYC: {
				Region:      RegionNYC,
				DisplayName: "New York",
				Location:    "NYC - 9950X Primary",
				Tier:        TierPrimary,
				Active:      true,
			},
			RegionLA: {
				Region:      RegionLA,
				DisplayName: "Los Angeles",
				Location:    "LA - 9950X Secondary",
				Latency:     60 * time.Millisecond, // Cross-country latency
				Tier:        TierSecondary,
				Active:      true,
			},
		},
		Policies: []*ReplicationPolicy{
			{
				Name:           "nyc-to-la-async",
				SourceRegion:   RegionNYC,
				TargetRegions:  []Region{RegionLA},
				Mode:           RepModeAsync,
				MaxLagDuration: 5 * time.Minute,
				Priority:       1,
			},
		},
		AffinityRules: []AffinityRule{
			{
				Name:          "east-coast-affinity",
				SourceRegions: []Region{RegionNYC},
				TargetRegion:  RegionNYC,
				Weight:        100,
			},
			{
				Name:          "west-coast-affinity",
				SourceRegions: []Region{RegionLA},
				TargetRegion:  RegionLA,
				Weight:        100,
			},
		},
	}
}

// GetRegion returns region info
func (g *GeoManager) GetRegion(region Region) (*RegionInfo, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	info, ok := g.config.Regions[region]
	return info, ok
}

// GetActiveRegions returns all active regions
func (g *GeoManager) GetActiveRegions() []*RegionInfo {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var active []*RegionInfo
	for _, info := range g.config.Regions {
		if info.Active {
			active = append(active, info)
		}
	}
	return active
}

// SetRegionHealth updates a region's health state
func (g *GeoManager) SetRegionHealth(region Region, state BackendState) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.healthState[region] = state
}

// GetRegionHealth returns a region's health state
func (g *GeoManager) GetRegionHealth(region Region) BackendState {
	g.mu.RLock()
	defer g.mu.RUnlock()
	state, ok := g.healthState[region]
	if !ok {
		return StateHealthy
	}
	return state
}

// SelectRegion chooses the best region based on health
func (g *GeoManager) SelectRegion(ctx context.Context, clientRegion Region) Region {
	g.mu.RLock()
	defer g.mu.RUnlock()

	// Check affinity rules first
	for _, rule := range g.config.AffinityRules {
		for _, src := range rule.SourceRegions {
			if src == clientRegion {
				if g.healthState[rule.TargetRegion] == StateHealthy {
					return rule.TargetRegion
				}
			}
		}
	}

	// Primary if healthy
	if g.healthState[g.config.PrimaryRegion] == StateHealthy {
		return g.config.PrimaryRegion
	}

	// Fallback to LA
	if g.healthState[RegionLA] == StateHealthy {
		return RegionLA
	}

	// Last resort
	return g.config.PrimaryRegion
}

// GetReplicationTargets returns target regions for a source
func (g *GeoManager) GetReplicationTargets(source Region) []Region {
	g.mu.RLock()
	defer g.mu.RUnlock()

	for _, policy := range g.config.Policies {
		if policy.SourceRegion == source {
			return policy.TargetRegions
		}
	}
	return nil
}

// UpdateLatency updates measured latency to a region
func (g *GeoManager) UpdateLatency(region Region, latency time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.latencies[region] = latency

	if info, ok := g.config.Regions[region]; ok {
		info.Latency = latency
	}
}

// GetLatency returns the current latency to a region
func (g *GeoManager) GetLatency(region Region) time.Duration {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.latencies[region]
}

// FailoverRegion marks a region as failed and returns the best alternative
func (g *GeoManager) FailoverRegion(failed Region) Region {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.healthState[failed] = StateFailed

	if info, ok := g.config.Regions[failed]; ok {
		info.Active = false
	}

	// Return the other region
	if failed == RegionNYC {
		return RegionLA
	}
	return RegionNYC
}

// RecoverRegion marks a region as recovered
func (g *GeoManager) RecoverRegion(region Region) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.healthState[region] = StateHealthy

	if info, ok := g.config.Regions[region]; ok {
		info.Active = true
	}
}

// GetStatus returns current status of all regions
func (g *GeoManager) GetStatus() map[Region]*RegionStatus {
	g.mu.RLock()
	defer g.mu.RUnlock()

	status := make(map[Region]*RegionStatus)
	for region, info := range g.config.Regions {
		status[region] = &RegionStatus{
			Info:    info,
			Health:  g.healthState[region],
			Latency: g.latencies[region],
		}
	}
	return status
}

// RegionStatus combines region info with current state
type RegionStatus struct {
	Info    *RegionInfo   `json:"info"`
	Health  BackendState  `json:"health"`
	Latency time.Duration `json:"latency"`
}
