// internal/metrics/custom.go
package metrics

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// Units
const (
	UnitBytes    = "bytes"
	UnitSeconds  = "seconds"
	UnitCount    = "count"
	UnitPercent  = "percent"
	UnitRequests = "requests"
)

// Aggregations
const (
	AggregationSum   = "sum"
	AggregationAvg   = "avg"
	AggregationMin   = "min"
	AggregationMax   = "max"
	AggregationCount = "count"
)

// Dimensions represents metric dimensions/tags
type Dimensions map[string]string

// Key returns a sorted key for dimensions
func (d Dimensions) Key() string {
	if len(d) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+d[k])
	}
	return strings.Join(parts, ",")
}

// CustomMetricConfig configures a custom metric
type CustomMetricConfig struct {
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	Aggregation string `json:"aggregation"`
	Description string `json:"description"`
}

// Validate checks configuration
func (c *CustomMetricConfig) Validate() error {
	if c.Name == "" {
		return errors.New("custom metrics: name is required")
	}
	return nil
}

// CustomMetricsConfig configures the custom metrics system
type CustomMetricsConfig struct {
	RetentionPeriod time.Duration `json:"retention_period"`
	Resolution      time.Duration `json:"resolution"`
}

// MetricValue represents a metric value with timestamp
type MetricValue struct {
	Value     float64   `json:"value"`
	Timestamp time.Time `json:"timestamp"`
}

// MetricData stores metric data
type MetricData struct {
	Current float64
	Values  []MetricValue
	mu      sync.Mutex
}

// APICallStats represents API call statistics
type APICallStats struct {
	TotalCalls   int64
	SuccessCalls int64
	FailedCalls  int64
	AvgLatency   time.Duration
}

// MetricSnapshot represents a point-in-time snapshot
type MetricSnapshot struct {
	Timestamp time.Time          `json:"timestamp"`
	Metrics   map[string]float64 `json:"metrics"`
}

// CustomMetrics manages custom metrics
type CustomMetrics struct {
	config    *CustomMetricsConfig
	metrics   map[string]*MetricData
	storage   map[string]int64
	bandwidth map[string]map[string]int64
	apiCalls  map[string]*APICallStats
	mu        sync.RWMutex
}

// NewCustomMetrics creates a custom metrics manager
func NewCustomMetrics(config *CustomMetricsConfig) *CustomMetrics {
	if config == nil {
		config = &CustomMetricsConfig{
			RetentionPeriod: 24 * time.Hour,
			Resolution:      time.Minute,
		}
	}

	return &CustomMetrics{
		config:    config,
		metrics:   make(map[string]*MetricData),
		storage:   make(map[string]int64),
		bandwidth: make(map[string]map[string]int64),
		apiCalls:  make(map[string]*APICallStats),
	}
}

func (cm *CustomMetrics) getOrCreate(name string, dims Dimensions) *MetricData {
	key := name
	if dims != nil {
		key = name + ":" + dims.Key()
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	if data, ok := cm.metrics[key]; ok {
		return data
	}

	data := &MetricData{
		Values: make([]MetricValue, 0),
	}
	cm.metrics[key] = data
	return data
}

// Record records a metric value (adds to current)
func (cm *CustomMetrics) Record(name string, value float64, dims Dimensions) {
	data := cm.getOrCreate(name, dims)
	data.mu.Lock()
	defer data.mu.Unlock()
	data.Current += value
	data.Values = append(data.Values, MetricValue{
		Value:     value,
		Timestamp: time.Now(),
	})
}

// RecordValue records a single value (for aggregations)
func (cm *CustomMetrics) RecordValue(name string, value float64, dims Dimensions) {
	data := cm.getOrCreate(name, dims)
	data.mu.Lock()
	defer data.mu.Unlock()
	data.Values = append(data.Values, MetricValue{
		Value:     value,
		Timestamp: time.Now(),
	})
}

// RecordWithTime records a value with timestamp
func (cm *CustomMetrics) RecordWithTime(name string, value float64, dims Dimensions, ts time.Time) {
	data := cm.getOrCreate(name, dims)
	data.mu.Lock()
	defer data.mu.Unlock()
	data.Current = value
	data.Values = append(data.Values, MetricValue{
		Value:     value,
		Timestamp: ts,
	})
}

// Get returns the current value
func (cm *CustomMetrics) Get(name string, dims Dimensions) float64 {
	data := cm.getOrCreate(name, dims)
	data.mu.Lock()
	defer data.mu.Unlock()
	return data.Current
}

// Aggregate calculates an aggregation
func (cm *CustomMetrics) Aggregate(name string, agg string, dims Dimensions) float64 {
	data := cm.getOrCreate(name, dims)
	data.mu.Lock()
	defer data.mu.Unlock()

	if len(data.Values) == 0 {
		return 0
	}

	switch agg {
	case AggregationSum:
		var sum float64
		for _, v := range data.Values {
			sum += v.Value
		}
		return sum

	case AggregationAvg:
		var sum float64
		for _, v := range data.Values {
			sum += v.Value
		}
		return sum / float64(len(data.Values))

	case AggregationMin:
		min := data.Values[0].Value
		for _, v := range data.Values[1:] {
			if v.Value < min {
				min = v.Value
			}
		}
		return min

	case AggregationMax:
		max := data.Values[0].Value
		for _, v := range data.Values[1:] {
			if v.Value > max {
				max = v.Value
			}
		}
		return max

	case AggregationCount:
		return float64(len(data.Values))

	default:
		return 0
	}
}

// Percentile calculates a percentile
func (cm *CustomMetrics) Percentile(name string, p float64, dims Dimensions) float64 {
	data := cm.getOrCreate(name, dims)
	data.mu.Lock()
	defer data.mu.Unlock()

	if len(data.Values) == 0 {
		return 0
	}

	// Copy and sort values
	values := make([]float64, len(data.Values))
	for i, v := range data.Values {
		values[i] = v.Value
	}
	sort.Float64s(values)

	// Calculate percentile index
	idx := int(float64(len(values)-1) * p / 100)
	return values[idx]
}

// TimeSeries returns time series data
func (cm *CustomMetrics) TimeSeries(name string, dims Dimensions, start, end time.Time) []MetricValue {
	data := cm.getOrCreate(name, dims)
	data.mu.Lock()
	defer data.mu.Unlock()

	var result []MetricValue
	for _, v := range data.Values {
		if v.Timestamp.After(start) && v.Timestamp.Before(end) {
			result = append(result, v)
		}
	}
	return result
}

// TimeSeriesBuckets returns bucketed time series
func (cm *CustomMetrics) TimeSeriesBuckets(name string, dims Dimensions, start, end time.Time, bucket time.Duration) map[time.Time]float64 {
	data := cm.getOrCreate(name, dims)
	data.mu.Lock()
	defer data.mu.Unlock()

	buckets := make(map[time.Time]float64)
	for _, v := range data.Values {
		if v.Timestamp.After(start) && v.Timestamp.Before(end) {
			bucketTime := v.Timestamp.Truncate(bucket)
			buckets[bucketTime] = v.Value
		}
	}
	return buckets
}

// Rate calculates rate of change per duration
func (cm *CustomMetrics) Rate(name string, dims Dimensions, per time.Duration) float64 {
	data := cm.getOrCreate(name, dims)
	data.mu.Lock()
	defer data.mu.Unlock()

	if len(data.Values) < 2 {
		return 0
	}

	first := data.Values[0]
	last := data.Values[len(data.Values)-1]

	duration := last.Timestamp.Sub(first.Timestamp)
	if duration == 0 {
		return 0
	}

	delta := last.Value - first.Value
	return delta * float64(per) / float64(duration)
}

// Delta returns the change between last two values
func (cm *CustomMetrics) Delta(name string, dims Dimensions) float64 {
	data := cm.getOrCreate(name, dims)
	data.mu.Lock()
	defer data.mu.Unlock()

	if len(data.Values) < 2 {
		return 0
	}

	prev := data.Values[len(data.Values)-2]
	curr := data.Values[len(data.Values)-1]
	return curr.Value - prev.Value
}

// SumByTag sums values matching a tag
func (cm *CustomMetrics) SumByTag(name, tag, value string) float64 {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	var sum float64
	for key, data := range cm.metrics {
		if !strings.HasPrefix(key, name+":") && key != name {
			continue
		}
		if strings.Contains(key, tag+"="+value) {
			data.mu.Lock()
			sum += data.Current
			data.mu.Unlock()
		}
	}
	return sum
}

// Reset resets a metric
func (cm *CustomMetrics) Reset(name string, dims Dimensions) {
	data := cm.getOrCreate(name, dims)
	data.mu.Lock()
	defer data.mu.Unlock()
	data.Current = 0
	data.Values = data.Values[:0]
}

// List returns all metric names
func (cm *CustomMetrics) List() []string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	names := make(map[string]bool)
	for key := range cm.metrics {
		name := key
		if idx := strings.Index(key, ":"); idx > 0 {
			name = key[:idx]
		}
		names[name] = true
	}

	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// ListByPrefix returns metrics matching prefix
func (cm *CustomMetrics) ListByPrefix(prefix string) []string {
	all := cm.List()
	var result []string
	for _, name := range all {
		if strings.HasPrefix(name, prefix) {
			result = append(result, name)
		}
	}
	return result
}

// RecordStorage records storage usage
func (cm *CustomMetrics) RecordStorage(tenantID string, bytes int64) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.storage[tenantID] = bytes
}

// TotalStorage returns total storage
func (cm *CustomMetrics) TotalStorage() int64 {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	var total int64
	for _, bytes := range cm.storage {
		total += bytes
	}
	return total
}

// RecordBandwidth records bandwidth usage
func (cm *CustomMetrics) RecordBandwidth(tenantID string, bytes int64, direction string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.bandwidth[tenantID] == nil {
		cm.bandwidth[tenantID] = make(map[string]int64)
	}
	cm.bandwidth[tenantID][direction] += bytes
}

// Bandwidth returns bandwidth for tenant/direction
func (cm *CustomMetrics) Bandwidth(tenantID, direction string) int64 {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	if bw, ok := cm.bandwidth[tenantID]; ok {
		return bw[direction]
	}
	return 0
}

// RecordAPICall records an API call
func (cm *CustomMetrics) RecordAPICall(operation string, latency time.Duration, success bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.apiCalls[operation] == nil {
		cm.apiCalls[operation] = &APICallStats{}
	}

	stats := cm.apiCalls[operation]
	stats.TotalCalls++
	if success {
		stats.SuccessCalls++
	} else {
		stats.FailedCalls++
	}
}

// APIStats returns API call stats
func (cm *CustomMetrics) APIStats(operation string) *APICallStats {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	if stats, ok := cm.apiCalls[operation]; ok {
		return stats
	}
	return &APICallStats{}
}

// Snapshot returns a point-in-time snapshot
func (cm *CustomMetrics) Snapshot() *MetricSnapshot {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	metrics := make(map[string]float64)
	for key, data := range cm.metrics {
		data.mu.Lock()
		metrics[key] = data.Current
		data.mu.Unlock()
	}

	return &MetricSnapshot{
		Timestamp: time.Now(),
		Metrics:   metrics,
	}
}
