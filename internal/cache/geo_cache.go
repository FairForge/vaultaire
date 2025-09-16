// internal/cache/geo_cache.go
package cache

import (
	"math"
	"sync"
	"time"
)

// GeoLocation represents a geographic location
type GeoLocation struct {
	Latitude  float64
	Longitude float64
	Region    string
	City      string
	Country   string
}

// EdgeNode represents a cache edge node
type EdgeNode struct {
	ID       string
	Location GeoLocation
	Capacity int64
	Load     float64
	Latency  time.Duration
	Active   bool
}

// GeoCacheManager manages geographically distributed caching
type GeoCacheManager struct {
	mu       sync.RWMutex
	edges    map[string]*EdgeNode
	userLocs map[string]GeoLocation
	dataLocs map[string]string // key -> edge node ID
}

// NewGeoCacheManager creates a geo-aware cache manager
func NewGeoCacheManager() *GeoCacheManager {
	return &GeoCacheManager{
		edges:    make(map[string]*EdgeNode),
		userLocs: make(map[string]GeoLocation),
		dataLocs: make(map[string]string),
	}
}

// AddEdgeNode registers an edge cache node
func (g *GeoCacheManager) AddEdgeNode(node *EdgeNode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.edges[node.ID] = node
}

// FindNearestEdge finds the closest edge node to a user
func (g *GeoCacheManager) FindNearestEdge(userLoc GeoLocation) *EdgeNode {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var nearest *EdgeNode
	minDistance := math.MaxFloat64

	for _, edge := range g.edges {
		if !edge.Active || edge.Load > 0.8 {
			continue
		}

		dist := g.calculateDistance(userLoc, edge.Location)
		if dist < minDistance {
			minDistance = dist
			nearest = edge
		}
	}

	return nearest
}

// calculateDistance calculates distance between two points (simplified)
func (g *GeoCacheManager) calculateDistance(loc1, loc2 GeoLocation) float64 {
	// Simplified distance calculation
	// Production would use Haversine formula
	latDiff := math.Abs(loc1.Latitude - loc2.Latitude)
	lonDiff := math.Abs(loc1.Longitude - loc2.Longitude)
	return math.Sqrt(latDiff*latDiff + lonDiff*lonDiff)
}
