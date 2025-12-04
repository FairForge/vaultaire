// internal/apm/apm.go
package apm

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Transaction types
const (
	TransactionTypeWeb        = "web"
	TransactionTypeBackground = "background"
	TransactionTypeMessage    = "message"
)

// Segment types
const (
	SegmentTypeCustom   = "custom"
	SegmentTypeDatabase = "database"
	SegmentTypeExternal = "external"
)

type contextKey string

var transactionContextKey = contextKey("transaction")

// APMConfig configures the APM agent
type APMConfig struct {
	ServiceName string  `json:"service_name"`
	Environment string  `json:"environment"`
	Enabled     bool    `json:"enabled"`
	SampleRate  float64 `json:"sample_rate"`
}

// Validate checks configuration
func (c *APMConfig) Validate() error {
	if c.ServiceName == "" {
		return errors.New("apm: service name is required")
	}
	return nil
}

// ApplyDefaults fills in default values
func (c *APMConfig) ApplyDefaults() {
	if c.Environment == "" {
		c.Environment = "development"
	}
	if c.SampleRate == 0 {
		c.SampleRate = 1.0
	}
	c.Enabled = true
}

// SegmentData represents segment data
type SegmentData struct {
	Name      string                 `json:"name"`
	Type      string                 `json:"type"`
	StartTime time.Time              `json:"start_time"`
	EndTime   time.Time              `json:"end_time"`
	Duration  time.Duration          `json:"duration"`
	Attrs     map[string]interface{} `json:"attributes,omitempty"`
}

// Segment represents a segment within a transaction
type Segment struct {
	name      string
	segType   string
	startTime time.Time
	endTime   time.Time
	attrs     map[string]interface{}
	tx        *Transaction
}

// End ends the segment
func (s *Segment) End() {
	s.endTime = time.Now()
	s.tx.addSegment(s)
}

// DatabaseSegmentConfig configures a database segment
type DatabaseSegmentConfig struct {
	Operation  string
	Collection string
	Product    string
}

// ExternalSegmentConfig configures an external segment
type ExternalSegmentConfig struct {
	URL    string
	Method string
}

// Transaction represents an APM transaction
type Transaction struct {
	id         string
	name       string
	txType     string
	startTime  time.Time
	endTime    time.Time
	attributes map[string]interface{}
	segments   []*SegmentData
	hasError   bool
	ctx        context.Context
	mu         sync.Mutex
}

// ID returns the transaction ID
func (t *Transaction) ID() string {
	return t.id
}

// Name returns the transaction name
func (t *Transaction) Name() string {
	return t.name
}

// Type returns the transaction type
func (t *Transaction) Type() string {
	return t.txType
}

// SetType sets the transaction type
func (t *Transaction) SetType(txType string) {
	t.txType = txType
}

// Context returns the transaction context
func (t *Transaction) Context() context.Context {
	return t.ctx
}

// Duration returns the transaction duration
func (t *Transaction) Duration() time.Duration {
	if t.endTime.IsZero() {
		return time.Since(t.startTime)
	}
	return t.endTime.Sub(t.startTime)
}

// SetAttribute sets an attribute
func (t *Transaction) SetAttribute(key string, value interface{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.attributes[key] = value
}

// AddCustomAttribute adds a custom attribute
func (t *Transaction) AddCustomAttribute(key string, value interface{}) {
	t.SetAttribute(key, value)
}

// Attributes returns all attributes
func (t *Transaction) Attributes() map[string]interface{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make(map[string]interface{})
	for k, v := range t.attributes {
		result[k] = v
	}
	return result
}

// StartSegment starts a new segment
func (t *Transaction) StartSegment(name string) *Segment {
	return &Segment{
		name:      name,
		segType:   SegmentTypeCustom,
		startTime: time.Now(),
		attrs:     make(map[string]interface{}),
		tx:        t,
	}
}

// StartDatabaseSegment starts a database segment
func (t *Transaction) StartDatabaseSegment(config *DatabaseSegmentConfig) *Segment {
	seg := &Segment{
		name:      config.Operation + " " + config.Collection,
		segType:   SegmentTypeDatabase,
		startTime: time.Now(),
		attrs: map[string]interface{}{
			"db.operation":  config.Operation,
			"db.collection": config.Collection,
			"db.product":    config.Product,
		},
		tx: t,
	}
	return seg
}

// StartExternalSegment starts an external segment
func (t *Transaction) StartExternalSegment(config *ExternalSegmentConfig) *Segment {
	seg := &Segment{
		name:      config.Method + " " + config.URL,
		segType:   SegmentTypeExternal,
		startTime: time.Now(),
		attrs: map[string]interface{}{
			"http.url":    config.URL,
			"http.method": config.Method,
		},
		tx: t,
	}
	return seg
}

func (t *Transaction) addSegment(seg *Segment) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.segments = append(t.segments, &SegmentData{
		Name:      seg.name,
		Type:      seg.segType,
		StartTime: seg.startTime,
		EndTime:   seg.endTime,
		Duration:  seg.endTime.Sub(seg.startTime),
		Attrs:     seg.attrs,
	})
}

// Segments returns all segments
func (t *Transaction) Segments() []*SegmentData {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.segments
}

// NoticeError records an error
func (t *Transaction) NoticeError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hasError = true
	t.attributes["error"] = err.Error()
}

// HasError returns whether an error was recorded
func (t *Transaction) HasError() bool {
	return t.hasError
}

// End ends the transaction
func (t *Transaction) End() {
	t.endTime = time.Now()
}

// TransactionFromContext retrieves transaction from context
func TransactionFromContext(ctx context.Context) *Transaction {
	if tx, ok := ctx.Value(transactionContextKey).(*Transaction); ok {
		return tx
	}
	return nil
}

// CustomEvent represents a custom event
type CustomEvent struct {
	Type       string                 `json:"type"`
	Timestamp  time.Time              `json:"timestamp"`
	Attributes map[string]interface{} `json:"attributes"`
}

// HealthStatus represents agent health
type HealthStatus struct {
	Status     string        `json:"status"`
	Goroutines int           `json:"goroutines"`
	HeapAlloc  uint64        `json:"heap_alloc"`
	HeapSys    uint64        `json:"heap_sys"`
	NumGC      uint32        `json:"num_gc"`
	Uptime     time.Duration `json:"uptime"`
}

// Agent is the APM agent
type Agent struct {
	config    *APMConfig
	metrics   map[string]float64
	events    []*CustomEvent
	startTime time.Time
	mu        sync.Mutex
}

// NewAgent creates an APM agent
func NewAgent(config *APMConfig) *Agent {
	if config == nil {
		config = &APMConfig{ServiceName: "unknown"}
	}
	config.ApplyDefaults()

	return &Agent{
		config:    config,
		metrics:   make(map[string]float64),
		events:    make([]*CustomEvent, 0),
		startTime: time.Now(),
	}
}

// StartTransaction starts a new transaction
func (a *Agent) StartTransaction(name string) *Transaction {
	tx := &Transaction{
		id:         uuid.New().String(),
		name:       name,
		txType:     TransactionTypeWeb,
		startTime:  time.Now(),
		attributes: make(map[string]interface{}),
		segments:   make([]*SegmentData, 0),
	}
	tx.ctx = context.WithValue(context.Background(), transactionContextKey, tx)
	return tx
}

// WrapHandler wraps an HTTP handler with APM instrumentation
func (a *Agent) WrapHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tx := a.StartTransaction(r.Method + " " + r.URL.Path)
		defer tx.End()

		tx.SetAttribute("http.method", r.Method)
		tx.SetAttribute("http.url", r.URL.String())
		tx.SetAttribute("http.user_agent", r.UserAgent())

		ctx := context.WithValue(r.Context(), transactionContextKey, tx)
		handler.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RecordMetric records a custom metric
func (a *Agent) RecordMetric(name string, value float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.metrics[name] = value
}

// IncrementCounter increments a counter metric
func (a *Agent) IncrementCounter(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.metrics[name]++
}

// Metrics returns all metrics
func (a *Agent) Metrics() map[string]float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make(map[string]float64)
	for k, v := range a.metrics {
		result[k] = v
	}
	return result
}

// RecordEvent records a custom event
func (a *Agent) RecordEvent(eventType string, attrs map[string]interface{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, &CustomEvent{
		Type:       eventType,
		Timestamp:  time.Now(),
		Attributes: attrs,
	})
}

// Events returns all events
func (a *Agent) Events() []*CustomEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.events
}

// Health returns agent health status
func (a *Agent) Health() *HealthStatus {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return &HealthStatus{
		Status:     "healthy",
		Goroutines: runtime.NumGoroutine(),
		HeapAlloc:  m.HeapAlloc,
		HeapSys:    m.HeapSys,
		NumGC:      m.NumGC,
		Uptime:     time.Since(a.startTime),
	}
}

// Shutdown shuts down the agent
func (a *Agent) Shutdown(ctx context.Context) error {
	// Flush any pending data
	return nil
}
