package events

import (
    "context"
    "encoding/json"
    "sync"
    "time"
)

// EventBus collects everything for future ML training
type EventBus interface {
    Publish(ctx context.Context, event Event) error
    Subscribe(pattern string, handler Handler) error
    
    // Future: replay for ML training
    Replay(from, to time.Time) ([]Event, error)
}

// Event represents something that happened
type Event struct {
    ID        string          `json:"id"`
    Type      EventType       `json:"type"`
    TenantID  string          `json:"tenant_id"`
    Timestamp time.Time       `json:"timestamp"`
    
    // The gold for ML training
    Operation   OperationData   `json:"operation,omitempty"`
    Performance PerformanceData `json:"performance,omitempty"`
    Decision    RoutingDecision `json:"decision,omitempty"`
    
    // Extensible
    Metadata json.RawMessage `json:"metadata,omitempty"`
}

// EventType categorizes events
type EventType string

const (
    OperationStarted  EventType = "operation.started"
    OperationComplete EventType = "operation.complete"
    OperationFailed   EventType = "operation.failed"
    CacheHit          EventType = "cache.hit"
    CacheMiss         EventType = "cache.miss"
    BackendSelected   EventType = "backend.selected"
    BackendFailed     EventType = "backend.failed"
)

// OperationData describes what happened
type OperationData struct {
    Type      string    `json:"type"`
    Container string    `json:"container"`
    Key       string    `json:"key"`
    Size      int64     `json:"size"`
    
    // Access patterns for ML
    AccessCount  int       `json:"access_count"`
    LastAccessed time.Time `json:"last_accessed"`
    HotScore     float64   `json:"hot_score"`
}

// PerformanceData for optimization
type PerformanceData struct {
    Duration    time.Duration `json:"duration"`
    BytesRead   int64        `json:"bytes_read"`
    BytesWritten int64       `json:"bytes_written"`
    CacheHit    bool         `json:"cache_hit"`
}

// RoutingDecision for ML training
type RoutingDecision struct {
    SelectedBackend string   `json:"selected_backend"`
    Reason          string   `json:"reason"`
    Score           float64  `json:"score"`
    Alternatives    []string `json:"alternatives"`
}

// Handler processes events
type Handler func(ctx context.Context, event Event) error

// SimpleEventBus is a basic in-memory implementation
type SimpleEventBus struct {
    mu        sync.RWMutex
    handlers  map[string][]Handler
    events    []Event
    maxEvents int
}

// NewSimpleEventBus creates a basic event bus
func NewSimpleEventBus() *SimpleEventBus {
    return &SimpleEventBus{
        handlers:  make(map[string][]Handler),
        events:    make([]Event, 0, 10000),
        maxEvents: 10000, // Keep last 10k events in memory
    }
}

// Publish sends an event
func (eb *SimpleEventBus) Publish(ctx context.Context, event Event) error {
    eb.mu.Lock()
    defer eb.mu.Unlock()
    
    // Store for replay
    eb.events = append(eb.events, event)
    if len(eb.events) > eb.maxEvents {
        eb.events = eb.events[1:] // Remove oldest
    }
    
    // Notify handlers
    for pattern, handlers := range eb.handlers {
        if matchesPattern(string(event.Type), pattern) {
            for _, handler := range handlers {
                go handler(ctx, event) // Async processing
            }
        }
    }
    
    return nil
}

// Subscribe registers a handler
func (eb *SimpleEventBus) Subscribe(pattern string, handler Handler) error {
    eb.mu.Lock()
    defer eb.mu.Unlock()
    
    eb.handlers[pattern] = append(eb.handlers[pattern], handler)
    return nil
}

// Replay returns historical events
func (eb *SimpleEventBus) Replay(from, to time.Time) ([]Event, error) {
    eb.mu.RLock()
    defer eb.mu.RUnlock()
    
    var result []Event
    for _, event := range eb.events {
        if event.Timestamp.After(from) && event.Timestamp.Before(to) {
            result = append(result, event)
        }
    }
    
    return result, nil
}

// matchesPattern checks if event type matches pattern
func matchesPattern(eventType, pattern string) bool {
    // Simple prefix matching for now
    // Future: support wildcards like "operation.*"
    return eventType == pattern || pattern == "*"
}
