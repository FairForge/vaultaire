// internal/tracing/tracer.go
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Status codes
const (
	StatusUnset = 0
	StatusOK    = 1
	StatusError = 2
)

// Span kinds
const (
	SpanKindInternal = 0
	SpanKindServer   = 1
	SpanKindClient   = 2
	SpanKindProducer = 3
	SpanKindConsumer = 4
)

type contextKey string

var spanContextKey = contextKey("span")

// TracerConfig configures a tracer
type TracerConfig struct {
	ServiceName string       `json:"service_name"`
	Endpoint    string       `json:"endpoint"`
	SampleRate  float64      `json:"sample_rate"`
	Exporter    SpanExporter `json:"-"`
}

// Validate checks configuration
func (c *TracerConfig) Validate() error {
	if c.ServiceName == "" {
		return errors.New("tracing: service name is required")
	}
	return nil
}

// ApplyDefaults fills in default values
func (c *TracerConfig) ApplyDefaults() {
	if c.SampleRate == 0 {
		c.SampleRate = 1.0
	}
}

// SpanOption configures span creation
type SpanOption func(*spanOptions)

type spanOptions struct {
	kind int
}

// WithSpanKind sets the span kind
func WithSpanKind(kind int) SpanOption {
	return func(o *spanOptions) {
		o.kind = kind
	}
}

// SpanEvent represents an event within a span
type SpanEvent struct {
	Name       string                 `json:"name"`
	Timestamp  time.Time              `json:"timestamp"`
	Attributes map[string]interface{} `json:"attributes,omitempty"`
}

// SpanLink represents a link to another span
type SpanLink struct {
	TraceID    string                 `json:"trace_id"`
	SpanID     string                 `json:"span_id"`
	Attributes map[string]interface{} `json:"attributes,omitempty"`
}

// Span represents a trace span
type Span struct {
	traceID       string
	spanID        string
	parentID      string
	name          string
	kind          int
	startTime     time.Time
	endTime       time.Time
	attributes    map[string]interface{}
	events        []*SpanEvent
	links         []*SpanLink
	status        int
	statusMessage string
	sampled       bool
	exporter      SpanExporter
	mu            sync.Mutex
}

// TraceID returns the trace ID
func (s *Span) TraceID() string {
	return s.traceID
}

// SpanID returns the span ID
func (s *Span) SpanID() string {
	return s.spanID
}

// ParentID returns the parent span ID
func (s *Span) ParentID() string {
	return s.parentID
}

// Name returns the span name
func (s *Span) Name() string {
	return s.name
}

// Kind returns the span kind
func (s *Span) Kind() int {
	return s.kind
}

// IsSampled returns whether the span is sampled
func (s *Span) IsSampled() bool {
	return s.sampled
}

// SetAttribute sets an attribute
func (s *Span) SetAttribute(key string, value interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attributes[key] = value
}

// SetAttributes sets multiple attributes
func (s *Span) SetAttributes(attrs map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range attrs {
		s.attributes[k] = v
	}
}

// Attributes returns all attributes
func (s *Span) Attributes() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]interface{})
	for k, v := range s.attributes {
		result[k] = v
	}
	return result
}

// AddEvent adds an event to the span
func (s *Span) AddEvent(name string, attrs map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, &SpanEvent{
		Name:       name,
		Timestamp:  time.Now().UTC(),
		Attributes: attrs,
	})
}

// Events returns all events
func (s *Span) Events() []*SpanEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}

// AddLink adds a link to another span
func (s *Span) AddLink(traceID, spanID string, attrs map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.links = append(s.links, &SpanLink{
		TraceID:    traceID,
		SpanID:     spanID,
		Attributes: attrs,
	})
}

// Links returns all links
func (s *Span) Links() []*SpanLink {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.links
}

// SetStatus sets the span status
func (s *Span) SetStatus(code int, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = code
	s.statusMessage = message
}

// Status returns the status code
func (s *Span) Status() int {
	return s.status
}

// StatusMessage returns the status message
func (s *Span) StatusMessage() string {
	return s.statusMessage
}

// RecordError records an error
func (s *Span) RecordError(err error) {
	s.SetStatus(StatusError, err.Error())
	s.AddEvent("exception", map[string]interface{}{
		"exception.message": err.Error(),
	})
}

// Duration returns the span duration
func (s *Span) Duration() time.Duration {
	if s.endTime.IsZero() {
		return time.Since(s.startTime)
	}
	return s.endTime.Sub(s.startTime)
}

// End ends the span
func (s *Span) End() {
	s.mu.Lock()
	s.endTime = time.Now().UTC()
	exporter := s.exporter
	s.mu.Unlock()

	if exporter != nil && s.sampled {
		exporter.Export(s)
	}
}

// SpanExporter exports spans
type SpanExporter interface {
	Export(span *Span)
}

// MemoryExporter stores spans in memory
type MemoryExporter struct {
	spans []*SpanData
	mu    sync.Mutex
}

// SpanData represents exported span data
type SpanData struct {
	TraceID    string                 `json:"trace_id"`
	SpanID     string                 `json:"span_id"`
	ParentID   string                 `json:"parent_id,omitempty"`
	Name       string                 `json:"name"`
	Kind       int                    `json:"kind"`
	StartTime  time.Time              `json:"start_time"`
	EndTime    time.Time              `json:"end_time"`
	Attributes map[string]interface{} `json:"attributes,omitempty"`
	Events     []*SpanEvent           `json:"events,omitempty"`
	Status     int                    `json:"status"`
}

// NewMemoryExporter creates a memory exporter
func NewMemoryExporter() *MemoryExporter {
	return &MemoryExporter{
		spans: make([]*SpanData, 0),
	}
}

// Export exports a span
func (e *MemoryExporter) Export(span *Span) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.spans = append(e.spans, &SpanData{
		TraceID:    span.traceID,
		SpanID:     span.spanID,
		ParentID:   span.parentID,
		Name:       span.name,
		Kind:       span.kind,
		StartTime:  span.startTime,
		EndTime:    span.endTime,
		Attributes: span.Attributes(),
		Events:     span.events,
		Status:     span.status,
	})
}

// Spans returns all exported spans
func (e *MemoryExporter) Spans() []*SpanData {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.spans
}

// Clear clears all spans
func (e *MemoryExporter) Clear() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = e.spans[:0]
}

// Tracer creates and manages spans
type Tracer struct {
	config *TracerConfig
}

// NewTracer creates a tracer
func NewTracer(config *TracerConfig) *Tracer {
	if config == nil {
		config = &TracerConfig{ServiceName: "unknown"}
	}
	config.ApplyDefaults()

	return &Tracer{
		config: config,
	}
}

// StartSpan starts a new span
func (t *Tracer) StartSpan(ctx context.Context, name string, opts ...SpanOption) (context.Context, *Span) {
	options := &spanOptions{}
	for _, opt := range opts {
		opt(options)
	}

	// Check for parent span
	var traceID, parentID string
	if parent := SpanFromContext(ctx); parent != nil {
		traceID = parent.TraceID()
		parentID = parent.SpanID()
	} else {
		traceID = generateTraceID()
	}

	// Determine if sampled
	sampled := mrand.Float64() < t.config.SampleRate

	span := &Span{
		traceID:    traceID,
		spanID:     generateSpanID(),
		parentID:   parentID,
		name:       name,
		kind:       options.kind,
		startTime:  time.Now().UTC(),
		attributes: make(map[string]interface{}),
		events:     make([]*SpanEvent, 0),
		links:      make([]*SpanLink, 0),
		sampled:    sampled,
		exporter:   t.config.Exporter,
	}

	return context.WithValue(ctx, spanContextKey, span), span
}

// SpanFromContext returns the span from context
func SpanFromContext(ctx context.Context) *Span {
	if span, ok := ctx.Value(spanContextKey).(*Span); ok {
		return span
	}
	return nil
}

// HTTPMiddleware returns tracing middleware
func (t *Tracer) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract trace context from headers
		ctx := t.Extract(r.Context(), r.Header)

		// Start span
		spanName := fmt.Sprintf("%s %s", r.Method, r.URL.Path)
		ctx, span := t.StartSpan(ctx, spanName, WithSpanKind(SpanKindServer))
		defer span.End()

		span.SetAttributes(map[string]interface{}{
			"http.method":      r.Method,
			"http.url":         r.URL.String(),
			"http.user_agent":  r.UserAgent(),
			"http.remote_addr": r.RemoteAddr,
		})

		// Wrap response writer to capture status
		wrapped := &statusResponseWriter{ResponseWriter: w, status: 200}

		next.ServeHTTP(wrapped, r.WithContext(ctx))

		span.SetAttribute("http.status_code", wrapped.status)
		if wrapped.status >= 400 {
			span.SetStatus(StatusError, fmt.Sprintf("HTTP %d", wrapped.status))
		} else {
			span.SetStatus(StatusOK, "")
		}
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Inject injects trace context into headers
func (t *Tracer) Inject(ctx context.Context, headers http.Header) {
	span := SpanFromContext(ctx)
	if span == nil {
		return
	}

	// W3C Trace Context format
	flags := "00"
	if span.sampled {
		flags = "01"
	}
	traceparent := fmt.Sprintf("00-%s-%s-%s", span.traceID, span.spanID, flags)
	headers.Set("traceparent", traceparent)
}

// Extract extracts trace context from headers
func (t *Tracer) Extract(ctx context.Context, headers http.Header) context.Context {
	traceparent := headers.Get("traceparent")
	if traceparent == "" {
		return ctx
	}

	// Parse W3C Trace Context
	parts := strings.Split(traceparent, "-")
	if len(parts) != 4 {
		return ctx
	}

	traceID := parts[1]
	parentID := parts[2]
	sampled := parts[3] == "01"

	// Create a span context (not a full span)
	span := &Span{
		traceID:  traceID,
		spanID:   parentID,
		sampled:  sampled,
		exporter: t.config.Exporter,
	}

	return context.WithValue(ctx, spanContextKey, span)
}

func generateTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func generateSpanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
