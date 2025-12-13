// internal/global/edge.go
package global

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// EdgeLocation represents a geographic edge location
type EdgeLocation struct {
	ID          string
	Name        string
	Region      string
	Country     string
	City        string
	Latitude    float64
	Longitude   float64
	Provider    string
	Endpoint    string
	Enabled     bool
	Capacity    int64 // bytes
	UsedBytes   int64
	Connections int
	MaxConns    int
	Weight      int // routing weight (higher = more traffic)
	Tags        []string
}

// EdgeStatus represents the current status of an edge location
type EdgeStatus struct {
	LocationID    string
	Healthy       bool
	Latency       time.Duration
	LastCheck     time.Time
	LastError     error
	RequestCount  int64
	BytesServed   int64
	ErrorCount    int64
	ActiveConns   int
	CPUPercent    float64
	MemoryPercent float64
}

// EdgeManager manages edge locations globally
type EdgeManager struct {
	mu        sync.RWMutex
	locations map[string]*EdgeLocation
	status    map[string]*EdgeStatus
	config    *EdgeConfig
}

// EdgeConfig configures the edge manager
type EdgeConfig struct {
	HealthCheckInterval time.Duration
	HealthCheckTimeout  time.Duration
	MaxLatency          time.Duration
	MinHealthyLocations int
	EnableAutoFailover  bool
}

// DefaultEdgeConfig returns default edge configuration
func DefaultEdgeConfig() *EdgeConfig {
	return &EdgeConfig{
		HealthCheckInterval: 30 * time.Second,
		HealthCheckTimeout:  5 * time.Second,
		MaxLatency:          500 * time.Millisecond,
		MinHealthyLocations: 1,
		EnableAutoFailover:  true,
	}
}

// NewEdgeManager creates a new edge manager
func NewEdgeManager(config *EdgeConfig) *EdgeManager {
	if config == nil {
		config = DefaultEdgeConfig()
	}
	return &EdgeManager{
		locations: make(map[string]*EdgeLocation),
		status:    make(map[string]*EdgeStatus),
		config:    config,
	}
}

// RegisterLocation adds an edge location
func (m *EdgeManager) RegisterLocation(loc *EdgeLocation) error {
	if loc.ID == "" {
		return fmt.Errorf("location ID required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.locations[loc.ID] = loc
	m.status[loc.ID] = &EdgeStatus{
		LocationID: loc.ID,
		Healthy:    loc.Enabled,
		LastCheck:  time.Now(),
	}

	return nil
}

// UnregisterLocation removes an edge location
func (m *EdgeManager) UnregisterLocation(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.locations, id)
	delete(m.status, id)
}

// GetLocation returns a specific edge location
func (m *EdgeManager) GetLocation(id string) (*EdgeLocation, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	loc, ok := m.locations[id]
	return loc, ok
}

// GetLocations returns all edge locations
func (m *EdgeManager) GetLocations() []*EdgeLocation {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*EdgeLocation, 0, len(m.locations))
	for _, loc := range m.locations {
		result = append(result, loc)
	}
	return result
}

// GetEnabledLocations returns only enabled locations
func (m *EdgeManager) GetEnabledLocations() []*EdgeLocation {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*EdgeLocation, 0)
	for _, loc := range m.locations {
		if loc.Enabled {
			result = append(result, loc)
		}
	}
	return result
}

// GetHealthyLocations returns only healthy locations
func (m *EdgeManager) GetHealthyLocations() []*EdgeLocation {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*EdgeLocation, 0)
	for _, loc := range m.locations {
		if status, ok := m.status[loc.ID]; ok && status.Healthy && loc.Enabled {
			result = append(result, loc)
		}
	}
	return result
}

// GetLocationsByRegion returns locations in a specific region
func (m *EdgeManager) GetLocationsByRegion(region string) []*EdgeLocation {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*EdgeLocation, 0)
	for _, loc := range m.locations {
		if loc.Region == region && loc.Enabled {
			result = append(result, loc)
		}
	}
	return result
}

// GetLocationsByCountry returns locations in a specific country
func (m *EdgeManager) GetLocationsByCountry(country string) []*EdgeLocation {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*EdgeLocation, 0)
	for _, loc := range m.locations {
		if loc.Country == country && loc.Enabled {
			result = append(result, loc)
		}
	}
	return result
}

// GetStatus returns the status of a location
func (m *EdgeManager) GetStatus(id string) (*EdgeStatus, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	status, ok := m.status[id]
	return status, ok
}

// UpdateStatus updates the status of a location
func (m *EdgeManager) UpdateStatus(status *EdgeStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.locations[status.LocationID]; ok {
		m.status[status.LocationID] = status
	}
}

// SetLocationHealth sets the health status of a location
func (m *EdgeManager) SetLocationHealth(id string, healthy bool, latency time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if status, ok := m.status[id]; ok {
		status.Healthy = healthy
		status.Latency = latency
		status.LastCheck = time.Now()
		status.LastError = err
	}
}

// FindNearestLocation finds the nearest edge location to given coordinates
func (m *EdgeManager) FindNearestLocation(lat, lon float64) *EdgeLocation {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var nearest *EdgeLocation
	minDist := math.MaxFloat64

	for _, loc := range m.locations {
		if !loc.Enabled {
			continue
		}
		if status, ok := m.status[loc.ID]; ok && !status.Healthy {
			continue
		}

		dist := haversineDistance(lat, lon, loc.Latitude, loc.Longitude)
		if dist < minDist {
			minDist = dist
			nearest = loc
		}
	}

	return nearest
}

// FindNearestLocations finds the N nearest edge locations
func (m *EdgeManager) FindNearestLocations(lat, lon float64, n int) []*EdgeLocation {
	m.mu.RLock()
	defer m.mu.RUnlock()

	type locDist struct {
		loc  *EdgeLocation
		dist float64
	}

	var distances []locDist
	for _, loc := range m.locations {
		if !loc.Enabled {
			continue
		}
		if status, ok := m.status[loc.ID]; ok && !status.Healthy {
			continue
		}

		dist := haversineDistance(lat, lon, loc.Latitude, loc.Longitude)
		distances = append(distances, locDist{loc: loc, dist: dist})
	}

	sort.Slice(distances, func(i, j int) bool {
		return distances[i].dist < distances[j].dist
	})

	result := make([]*EdgeLocation, 0, n)
	for i := 0; i < len(distances) && i < n; i++ {
		result = append(result, distances[i].loc)
	}

	return result
}

// SelectLocation selects the best location based on multiple factors
func (m *EdgeManager) SelectLocation(lat, lon float64, opts *SelectionOptions) *EdgeLocation {
	if opts == nil {
		opts = DefaultSelectionOptions()
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	var best *EdgeLocation
	bestScore := math.MinInt64

	for _, loc := range m.locations {
		if !loc.Enabled {
			continue
		}

		status, ok := m.status[loc.ID]
		if !ok || !status.Healthy {
			continue
		}

		// Check region/country constraints
		if opts.RequiredRegion != "" && loc.Region != opts.RequiredRegion {
			continue
		}
		if opts.RequiredCountry != "" && loc.Country != opts.RequiredCountry {
			continue
		}

		// Calculate score
		score := m.calculateLocationScore(loc, status, lat, lon, opts)

		if score > bestScore {
			bestScore = score
			best = loc
		}
	}

	return best
}

// SelectionOptions configures location selection
type SelectionOptions struct {
	RequiredRegion   string
	RequiredCountry  string
	PreferLowLatency bool
	PreferCapacity   bool
	WeightDistance   float64 // 0-1
	WeightLatency    float64 // 0-1
	WeightCapacity   float64 // 0-1
	WeightLoad       float64 // 0-1
}

// DefaultSelectionOptions returns default selection options
func DefaultSelectionOptions() *SelectionOptions {
	return &SelectionOptions{
		PreferLowLatency: true,
		WeightDistance:   0.3,
		WeightLatency:    0.4,
		WeightCapacity:   0.15,
		WeightLoad:       0.15,
	}
}

func (m *EdgeManager) calculateLocationScore(loc *EdgeLocation, status *EdgeStatus, lat, lon float64, opts *SelectionOptions) int {
	score := 0

	// Distance score (closer is better)
	dist := haversineDistance(lat, lon, loc.Latitude, loc.Longitude)
	maxDist := 20000.0 // km (roughly half Earth circumference)
	distScore := int((1 - dist/maxDist) * 100 * opts.WeightDistance)
	score += distScore

	// Latency score (lower is better)
	if status.Latency > 0 {
		maxLatency := float64(m.config.MaxLatency)
		latencyScore := int((1 - float64(status.Latency)/maxLatency) * 100 * opts.WeightLatency)
		if latencyScore < 0 {
			latencyScore = 0
		}
		score += latencyScore
	}

	// Capacity score (more available is better)
	if loc.Capacity > 0 {
		availableRatio := float64(loc.Capacity-loc.UsedBytes) / float64(loc.Capacity)
		capacityScore := int(availableRatio * 100 * opts.WeightCapacity)
		score += capacityScore
	}

	// Load score (lower connections is better)
	if loc.MaxConns > 0 {
		loadRatio := 1 - float64(status.ActiveConns)/float64(loc.MaxConns)
		loadScore := int(loadRatio * 100 * opts.WeightLoad)
		score += loadScore
	}

	// Add base weight
	score += loc.Weight

	return score
}

// haversineDistance calculates distance between two points in km
func haversineDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371.0 // km

	lat1Rad := lat1 * math.Pi / 180
	lat2Rad := lat2 * math.Pi / 180
	deltaLat := (lat2 - lat1) * math.Pi / 180
	deltaLon := (lon2 - lon1) * math.Pi / 180

	a := math.Sin(deltaLat/2)*math.Sin(deltaLat/2) +
		math.Cos(lat1Rad)*math.Cos(lat2Rad)*
			math.Sin(deltaLon/2)*math.Sin(deltaLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return earthRadius * c
}

// EdgeHealthChecker performs health checks on edge locations
type EdgeHealthChecker struct {
	manager  *EdgeManager
	checker  func(ctx context.Context, loc *EdgeLocation) (time.Duration, error)
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewEdgeHealthChecker creates a new health checker
func NewEdgeHealthChecker(manager *EdgeManager, checker func(ctx context.Context, loc *EdgeLocation) (time.Duration, error)) *EdgeHealthChecker {
	return &EdgeHealthChecker{
		manager:  manager,
		checker:  checker,
		interval: manager.config.HealthCheckInterval,
		stopCh:   make(chan struct{}),
	}
}

// Start begins health checking
func (h *EdgeHealthChecker) Start() {
	h.wg.Add(1)
	go h.run()
}

// Stop stops health checking
func (h *EdgeHealthChecker) Stop() {
	close(h.stopCh)
	h.wg.Wait()
}

func (h *EdgeHealthChecker) run() {
	defer h.wg.Done()

	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	// Initial check
	h.checkAll()

	for {
		select {
		case <-ticker.C:
			h.checkAll()
		case <-h.stopCh:
			return
		}
	}
}

func (h *EdgeHealthChecker) checkAll() {
	locations := h.manager.GetEnabledLocations()

	var wg sync.WaitGroup
	for _, loc := range locations {
		wg.Add(1)
		go func(l *EdgeLocation) {
			defer wg.Done()
			h.checkLocation(l)
		}(loc)
	}
	wg.Wait()
}

func (h *EdgeHealthChecker) checkLocation(loc *EdgeLocation) {
	ctx, cancel := context.WithTimeout(context.Background(), h.manager.config.HealthCheckTimeout)
	defer cancel()

	latency, err := h.checker(ctx, loc)
	healthy := err == nil && latency < h.manager.config.MaxLatency

	h.manager.SetLocationHealth(loc.ID, healthy, latency, err)
}

// PredefinedEdgeLocations returns common edge location definitions
func PredefinedEdgeLocations() []*EdgeLocation {
	return []*EdgeLocation{
		// North America
		{ID: "us-east-1", Name: "US East (N. Virginia)", Region: "us-east", Country: "US", City: "Ashburn", Latitude: 39.0438, Longitude: -77.4874},
		{ID: "us-east-2", Name: "US East (Ohio)", Region: "us-east", Country: "US", City: "Columbus", Latitude: 39.9612, Longitude: -82.9988},
		{ID: "us-west-1", Name: "US West (N. California)", Region: "us-west", Country: "US", City: "San Jose", Latitude: 37.3382, Longitude: -121.8863},
		{ID: "us-west-2", Name: "US West (Oregon)", Region: "us-west", Country: "US", City: "Portland", Latitude: 45.5152, Longitude: -122.6784},
		{ID: "ca-central-1", Name: "Canada (Central)", Region: "ca", Country: "CA", City: "Montreal", Latitude: 45.5017, Longitude: -73.5673},

		// Europe
		{ID: "eu-west-1", Name: "Europe (Ireland)", Region: "eu-west", Country: "IE", City: "Dublin", Latitude: 53.3498, Longitude: -6.2603},
		{ID: "eu-west-2", Name: "Europe (London)", Region: "eu-west", Country: "GB", City: "London", Latitude: 51.5074, Longitude: -0.1278},
		{ID: "eu-west-3", Name: "Europe (Paris)", Region: "eu-west", Country: "FR", City: "Paris", Latitude: 48.8566, Longitude: 2.3522},
		{ID: "eu-central-1", Name: "Europe (Frankfurt)", Region: "eu-central", Country: "DE", City: "Frankfurt", Latitude: 50.1109, Longitude: 8.6821},
		{ID: "eu-north-1", Name: "Europe (Stockholm)", Region: "eu-north", Country: "SE", City: "Stockholm", Latitude: 59.3293, Longitude: 18.0686},

		// Asia Pacific
		{ID: "ap-northeast-1", Name: "Asia Pacific (Tokyo)", Region: "ap-northeast", Country: "JP", City: "Tokyo", Latitude: 35.6762, Longitude: 139.6503},
		{ID: "ap-northeast-2", Name: "Asia Pacific (Seoul)", Region: "ap-northeast", Country: "KR", City: "Seoul", Latitude: 37.5665, Longitude: 126.9780},
		{ID: "ap-southeast-1", Name: "Asia Pacific (Singapore)", Region: "ap-southeast", Country: "SG", City: "Singapore", Latitude: 1.3521, Longitude: 103.8198},
		{ID: "ap-southeast-2", Name: "Asia Pacific (Sydney)", Region: "ap-southeast", Country: "AU", City: "Sydney", Latitude: -33.8688, Longitude: 151.2093},
		{ID: "ap-south-1", Name: "Asia Pacific (Mumbai)", Region: "ap-south", Country: "IN", City: "Mumbai", Latitude: 19.0760, Longitude: 72.8777},

		// South America
		{ID: "sa-east-1", Name: "South America (São Paulo)", Region: "sa-east", Country: "BR", City: "São Paulo", Latitude: -23.5505, Longitude: -46.6333},

		// Middle East & Africa
		{ID: "me-south-1", Name: "Middle East (Bahrain)", Region: "me-south", Country: "BH", City: "Manama", Latitude: 26.2285, Longitude: 50.5860},
		{ID: "af-south-1", Name: "Africa (Cape Town)", Region: "af-south", Country: "ZA", City: "Cape Town", Latitude: -33.9249, Longitude: 18.4241},
	}
}

// RegionInfo provides information about a region
type RegionInfo struct {
	ID          string
	Name        string
	Continent   string
	Locations   []string
	PrimaryLoc  string
	Regulations []string // GDPR, etc.
}

// GetRegions returns all available regions
func GetRegions() map[string]*RegionInfo {
	return map[string]*RegionInfo{
		"us-east": {
			ID:         "us-east",
			Name:       "US East",
			Continent:  "North America",
			Locations:  []string{"us-east-1", "us-east-2"},
			PrimaryLoc: "us-east-1",
		},
		"us-west": {
			ID:         "us-west",
			Name:       "US West",
			Continent:  "North America",
			Locations:  []string{"us-west-1", "us-west-2"},
			PrimaryLoc: "us-west-2",
		},
		"eu-west": {
			ID:          "eu-west",
			Name:        "EU West",
			Continent:   "Europe",
			Locations:   []string{"eu-west-1", "eu-west-2", "eu-west-3"},
			PrimaryLoc:  "eu-west-1",
			Regulations: []string{"GDPR"},
		},
		"eu-central": {
			ID:          "eu-central",
			Name:        "EU Central",
			Continent:   "Europe",
			Locations:   []string{"eu-central-1"},
			PrimaryLoc:  "eu-central-1",
			Regulations: []string{"GDPR"},
		},
		"ap-northeast": {
			ID:         "ap-northeast",
			Name:       "Asia Pacific Northeast",
			Continent:  "Asia",
			Locations:  []string{"ap-northeast-1", "ap-northeast-2"},
			PrimaryLoc: "ap-northeast-1",
		},
		"ap-southeast": {
			ID:         "ap-southeast",
			Name:       "Asia Pacific Southeast",
			Continent:  "Asia",
			Locations:  []string{"ap-southeast-1", "ap-southeast-2"},
			PrimaryLoc: "ap-southeast-1",
		},
	}
}
