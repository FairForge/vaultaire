// internal/intelligence/types.go
package intelligence

import (
	"time"
)

// AccessEvent represents a single access to storage
type AccessEvent struct {
	TenantID  string
	Container string
	Artifact  string
	Operation string
	Size      int64
	Latency   time.Duration
	Backend   string
	CacheHit  bool
	Timestamp time.Time
	IP        string
	UserAgent string
	Success   bool
	ErrorCode string
}

// TenantPatterns contains all pattern analysis for a tenant
type TenantPatterns struct {
	TenantID     string
	Temporal     TemporalPatterns
	Spatial      SpatialPatterns
	UserBehavior UserBehavior
}

// TemporalPatterns represents time-based access patterns
type TemporalPatterns struct {
	PeakHours []int
	PeakDays  []int
}

// SpatialPatterns represents location-based patterns
type SpatialPatterns struct {
	HotDirectories []string
	AccessClusters map[string][]string
}

// UserBehavior models user access behavior
type UserBehavior struct {
	AvgFileSize    int64
	AccessVelocity float64
	BurstThreshold int
}

// Recommendation for optimization
type Recommendation struct {
	Type             string
	Target           string
	Reason           string
	Impact           string
	PreferredBackend string
}

// Prediction from ML model
type Prediction struct {
	NextAccess  time.Time
	Temperature string
	ShouldCache bool
}

// HotData represents frequently accessed items
type HotData struct {
	Key         string
	AccessCount int
	LastAccess  time.Time
	Temperature string
}

// Rule for heuristic model
type Rule struct {
	Name      string
	Condition func([]float64) bool
	Action    Prediction
}
