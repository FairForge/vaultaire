// internal/metrics/collector.go
package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metric types
const (
	MetricTypeCounter   = "counter"
	MetricTypeGauge     = "gauge"
	MetricTypeHistogram = "histogram"
	MetricTypeSummary   = "summary"
)

// Export formats
const (
	FormatPrometheus = "prometheus"
	FormatJSON       = "json"
	FormatStatsD     = "statsd"
)

// Labels represents metric labels
type Labels map[string]string

// Key returns a sorted key for the labels
func (l Labels) Key() string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, l[k]))
	}
	return strings.Join(parts, ",")
}

// MetricConfig configures a metric
type MetricConfig struct {
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Description string    `json:"description"`
	Labels      []string  `json:"labels"`
	Buckets     []float64 `json:"buckets,omitempty"`
	Quantiles   []float64 `json:"quantiles,omitempty"`
}

// Validate checks configuration
func (c *MetricConfig) Validate() error {
	if c.Name == "" {
		return errors.New("metrics: name is required")
	}
	validTypes := map[string]bool{
		MetricTypeCounter:   true,
		MetricTypeGauge:     true,
		MetricTypeHistogram: true,
		MetricTypeSummary:   true,
		"":                  true, // Allow empty for auto-detection
	}
	if !validTypes[c.Type] {
		return fmt.Errorf("metrics: invalid type: %s", c.Type)
	}
	return nil
}

// Counter is a monotonically increasing metric
type Counter struct {
	name   string
	values map[string]*atomic.Int64
	mu     sync.RWMutex
}

// Inc increments the counter by 1
func (c *Counter) Inc(labels Labels) {
	c.Add(1, labels)
}

// Add adds a value to the counter
func (c *Counter) Add(v float64, labels Labels) {
	key := labels.Key()
	c.mu.Lock()
	if _, ok := c.values[key]; !ok {
		c.values[key] = &atomic.Int64{}
	}
	c.mu.Unlock()

	c.mu.RLock()
	c.values[key].Add(int64(v))
	c.mu.RUnlock()
}

// Value returns the current value
func (c *Counter) Value(labels Labels) float64 {
	key := labels.Key()
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.values[key]; ok {
		return float64(v.Load())
	}
	return 0
}

// Reset resets the counter
func (c *Counter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.values {
		c.values[k].Store(0)
	}
}

// Gauge is a metric that can go up and down
type Gauge struct {
	name   string
	values map[string]*atomic.Int64
	mu     sync.RWMutex
}

// Set sets the gauge value
func (g *Gauge) Set(v float64, labels Labels) {
	key := labels.Key()
	g.mu.Lock()
	if _, ok := g.values[key]; !ok {
		g.values[key] = &atomic.Int64{}
	}
	g.mu.Unlock()

	g.mu.RLock()
	g.values[key].Store(int64(v * 1000)) // Store as millis for precision
	g.mu.RUnlock()
}

// Inc increments the gauge by 1
func (g *Gauge) Inc(labels Labels) {
	g.Add(1, labels)
}

// Dec decrements the gauge by 1
func (g *Gauge) Dec(labels Labels) {
	g.Add(-1, labels)
}

// Add adds to the gauge
func (g *Gauge) Add(v float64, labels Labels) {
	key := labels.Key()
	g.mu.Lock()
	if _, ok := g.values[key]; !ok {
		g.values[key] = &atomic.Int64{}
	}
	g.mu.Unlock()

	g.mu.RLock()
	g.values[key].Add(int64(v * 1000))
	g.mu.RUnlock()
}

// Value returns the current value
func (g *Gauge) Value(labels Labels) float64 {
	key := labels.Key()
	g.mu.RLock()
	defer g.mu.RUnlock()
	if v, ok := g.values[key]; ok {
		return float64(v.Load()) / 1000
	}
	return 0
}

// Reset resets the gauge
func (g *Gauge) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k := range g.values {
		g.values[k].Store(0)
	}
}

// HistogramStats contains histogram statistics
type HistogramStats struct {
	Count int64
	Sum   float64
	Min   float64
	Max   float64
}

// Histogram tracks value distributions
type Histogram struct {
	name    string
	buckets []float64
	counts  map[string]*histogramData
	mu      sync.RWMutex
}

type histogramData struct {
	count   int64
	sum     float64
	min     float64
	max     float64
	buckets []int64
}

// Observe records a value
func (h *Histogram) Observe(v float64, labels Labels) {
	key := labels.Key()
	h.mu.Lock()
	if _, ok := h.counts[key]; !ok {
		h.counts[key] = &histogramData{
			min:     v,
			max:     v,
			buckets: make([]int64, len(h.buckets)),
		}
	}
	data := h.counts[key]
	data.count++
	data.sum += v
	if v < data.min {
		data.min = v
	}
	if v > data.max {
		data.max = v
	}
	for i, b := range h.buckets {
		if v <= b {
			data.buckets[i]++
		}
	}
	h.mu.Unlock()
}

// Stats returns histogram statistics
func (h *Histogram) Stats(labels Labels) HistogramStats {
	key := labels.Key()
	h.mu.RLock()
	defer h.mu.RUnlock()
	if data, ok := h.counts[key]; ok {
		return HistogramStats{
			Count: data.count,
			Sum:   data.sum,
			Min:   data.min,
			Max:   data.max,
		}
	}
	return HistogramStats{}
}

// Buckets returns bucket counts
func (h *Histogram) Buckets(labels Labels) map[float64]int64 {
	key := labels.Key()
	h.mu.RLock()
	defer h.mu.RUnlock()

	result := make(map[float64]int64)
	if data, ok := h.counts[key]; ok {
		for i, b := range h.buckets {
			result[b] = data.buckets[i]
		}
	}
	return result
}

// Reset resets the histogram
func (h *Histogram) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counts = make(map[string]*histogramData)
}

// Summary tracks quantiles
type Summary struct {
	name      string
	quantiles []float64
	values    map[string]*summaryData
	mu        sync.RWMutex
}

type summaryData struct {
	values []float64
}

// Observe records a value
func (s *Summary) Observe(v float64, labels Labels) {
	key := labels.Key()
	s.mu.Lock()
	if _, ok := s.values[key]; !ok {
		s.values[key] = &summaryData{values: make([]float64, 0, 1000)}
	}
	s.values[key].values = append(s.values[key].values, v)
	s.mu.Unlock()
}

// Quantiles returns quantile values
func (s *Summary) Quantiles(labels Labels) map[float64]float64 {
	key := labels.Key()
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[float64]float64)
	data, ok := s.values[key]
	if !ok || len(data.values) == 0 {
		return result
	}

	sorted := make([]float64, len(data.values))
	copy(sorted, data.values)
	sort.Float64s(sorted)

	for _, q := range s.quantiles {
		idx := int(float64(len(sorted)-1) * q)
		result[q] = sorted[idx]
	}

	return result
}

// Reset resets the summary
func (s *Summary) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = make(map[string]*summaryData)
}

// Timer measures duration
type Timer struct {
	histogram *Histogram
	labels    Labels
	start     time.Time
}

// Stop stops the timer and records duration
func (t *Timer) Stop() time.Duration {
	duration := time.Since(t.start)
	t.histogram.Observe(duration.Seconds(), t.labels)
	return duration
}

// Snapshot contains all metric values
type Snapshot struct {
	Timestamp time.Time              `json:"timestamp"`
	Metrics   map[string]interface{} `json:"metrics"`
}

// CollectorConfig configures the collector
type CollectorConfig struct {
	Namespace string
	Subsystem string
}

// Collector collects metrics
type Collector struct {
	config     *CollectorConfig
	counters   map[string]*Counter
	gauges     map[string]*Gauge
	histograms map[string]*Histogram
	summaries  map[string]*Summary
	configs    map[string]*MetricConfig
	mu         sync.RWMutex
}

// NewCollector creates a collector
func NewCollector(config *CollectorConfig) *Collector {
	if config == nil {
		config = &CollectorConfig{}
	}
	return &Collector{
		config:     config,
		counters:   make(map[string]*Counter),
		gauges:     make(map[string]*Gauge),
		histograms: make(map[string]*Histogram),
		summaries:  make(map[string]*Summary),
		configs:    make(map[string]*MetricConfig),
	}
}

func (c *Collector) fullName(name string) string {
	var parts []string
	if c.config.Namespace != "" {
		parts = append(parts, c.config.Namespace)
	}
	if c.config.Subsystem != "" {
		parts = append(parts, c.config.Subsystem)
	}
	parts = append(parts, name)
	return strings.Join(parts, "_")
}

// RegisterCounter registers a counter
func (c *Collector) RegisterCounter(config *MetricConfig) (*Counter, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	name := c.fullName(config.Name)
	c.mu.Lock()
	defer c.mu.Unlock()

	counter := &Counter{
		name:   name,
		values: make(map[string]*atomic.Int64),
	}
	c.counters[name] = counter
	c.configs[name] = config
	return counter, nil
}

// GetCounter returns a counter by name
func (c *Collector) GetCounter(name string) (*Counter, bool) {
	name = c.fullName(name)
	c.mu.RLock()
	defer c.mu.RUnlock()
	counter, ok := c.counters[name]
	return counter, ok
}

// RegisterGauge registers a gauge
func (c *Collector) RegisterGauge(config *MetricConfig) (*Gauge, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	name := c.fullName(config.Name)
	c.mu.Lock()
	defer c.mu.Unlock()

	gauge := &Gauge{
		name:   name,
		values: make(map[string]*atomic.Int64),
	}
	c.gauges[name] = gauge
	c.configs[name] = config
	return gauge, nil
}

// GetGauge returns a gauge by name
func (c *Collector) GetGauge(name string) (*Gauge, bool) {
	name = c.fullName(name)
	c.mu.RLock()
	defer c.mu.RUnlock()
	gauge, ok := c.gauges[name]
	return gauge, ok
}

// RegisterHistogram registers a histogram
func (c *Collector) RegisterHistogram(config *MetricConfig) (*Histogram, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	name := c.fullName(config.Name)
	buckets := config.Buckets
	if len(buckets) == 0 {
		buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	histogram := &Histogram{
		name:    name,
		buckets: buckets,
		counts:  make(map[string]*histogramData),
	}
	c.histograms[name] = histogram
	c.configs[name] = config
	return histogram, nil
}

// GetHistogram returns a histogram by name
func (c *Collector) GetHistogram(name string) (*Histogram, bool) {
	name = c.fullName(name)
	c.mu.RLock()
	defer c.mu.RUnlock()
	histogram, ok := c.histograms[name]
	return histogram, ok
}

// RegisterSummary registers a summary
func (c *Collector) RegisterSummary(config *MetricConfig) (*Summary, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	name := c.fullName(config.Name)
	quantiles := config.Quantiles
	if len(quantiles) == 0 {
		quantiles = []float64{0.5, 0.9, 0.99}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	summary := &Summary{
		name:      name,
		quantiles: quantiles,
		values:    make(map[string]*summaryData),
	}
	c.summaries[name] = summary
	c.configs[name] = config
	return summary, nil
}

// GetSummary returns a summary by name
func (c *Collector) GetSummary(name string) (*Summary, bool) {
	name = c.fullName(name)
	c.mu.RLock()
	defer c.mu.RUnlock()
	summary, ok := c.summaries[name]
	return summary, ok
}

// StartTimer starts a timer for a histogram
func (c *Collector) StartTimer(name string, labels Labels) *Timer {
	name = c.fullName(name)
	c.mu.RLock()
	histogram := c.histograms[name]
	c.mu.RUnlock()

	return &Timer{
		histogram: histogram,
		labels:    labels,
		start:     time.Now(),
	}
}

// Snapshot returns all metric values
func (c *Collector) Snapshot() *Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	metrics := make(map[string]interface{})

	for name, counter := range c.counters {
		metrics[name] = counter.Value(nil)
	}
	for name, gauge := range c.gauges {
		metrics[name] = gauge.Value(nil)
	}

	return &Snapshot{
		Timestamp: time.Now().UTC(),
		Metrics:   metrics,
	}
}

// Export exports metrics in the specified format
func (c *Collector) Export(format string) ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	switch format {
	case FormatPrometheus:
		return c.exportPrometheus()
	case FormatJSON:
		return c.exportJSON()
	default:
		return c.exportPrometheus()
	}
}

func (c *Collector) exportPrometheus() ([]byte, error) {
	var buf bytes.Buffer

	for name, counter := range c.counters {
		config := c.configs[name]
		if config != nil && config.Description != "" {
			fmt.Fprintf(&buf, "# HELP %s %s\n", name, config.Description)
		}
		fmt.Fprintf(&buf, "# TYPE %s counter\n", name)
		fmt.Fprintf(&buf, "%s %v\n", name, counter.Value(nil))
	}

	for name, gauge := range c.gauges {
		config := c.configs[name]
		if config != nil && config.Description != "" {
			fmt.Fprintf(&buf, "# HELP %s %s\n", name, config.Description)
		}
		fmt.Fprintf(&buf, "# TYPE %s gauge\n", name)
		fmt.Fprintf(&buf, "%s %v\n", name, gauge.Value(nil))
	}

	return buf.Bytes(), nil
}

func (c *Collector) exportJSON() ([]byte, error) {
	snapshot := c.Snapshot()
	return json.MarshalIndent(snapshot, "", "  ")
}

// List returns all registered metric names
func (c *Collector) List() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var names []string
	for name := range c.configs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Unregister removes a metric
func (c *Collector) Unregister(name string) error {
	name = c.fullName(name)
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.counters, name)
	delete(c.gauges, name)
	delete(c.histograms, name)
	delete(c.summaries, name)
	delete(c.configs, name)
	return nil
}

// Reset resets all metrics
func (c *Collector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, counter := range c.counters {
		counter.Reset()
	}
	for _, gauge := range c.gauges {
		gauge.Reset()
	}
	for _, histogram := range c.histograms {
		histogram.Reset()
	}
	for _, summary := range c.summaries {
		summary.Reset()
	}
}

// HTTPHandler returns an HTTP handler for metrics
func (c *Collector) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		format := r.URL.Query().Get("format")
		if format == "" {
			format = FormatPrometheus
		}

		data, err := c.Export(format)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if format == FormatJSON {
			w.Header().Set("Content-Type", "application/json")
		} else {
			w.Header().Set("Content-Type", "text/plain")
		}
		_, _ = w.Write(data)
	})
}

// Push pushes metrics to a Prometheus Pushgateway
func (c *Collector) Push(ctx context.Context, gateway, job string) error {
	data, err := c.Export(FormatPrometheus)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/metrics/job/%s", gateway, job)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("push failed: status %d", resp.StatusCode)
	}

	return nil
}
