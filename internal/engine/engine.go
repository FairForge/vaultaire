package engine

import (
    "context"
    "io"
    "time"
)

// Engine is the universal execution interface that secretly enables EVERYTHING
type Engine interface {
    // MVP: Basic operations that look simple
    Execute(ctx context.Context, op Operation) (Result, error)
    
    // Hidden future capabilities (return ErrNotImplemented for now)
    Query(ctx context.Context, query Query) (ResultSet, error)
    BeginTx(ctx context.Context) (Transaction, error)
    Compute(ctx context.Context, wasm []byte, data io.Reader) (io.Reader, error)
}

// Operation represents any action in the system
type Operation struct {
    ID        string
    Type      OpType        // GET, PUT, DELETE now → COMPUTE later
    Container string        // NOT bucket! Using DAOS terminology
    Key       string
    Stream    io.Reader     // Stream-based from day 1
    Metadata  Metadata      // Extensible metadata
    Policy    PolicyHints   // For future ML routing
    
    // Hidden fields (set internally)
    TenantID  string
    AppID     string
    EventID   string
    Pipeline  []Stage      // Processing pipeline
}

// OpType defines operation types
type OpType string

const (
    OpGet    OpType = "GET"
    OpPut    OpType = "PUT"
    OpDelete OpType = "DELETE"
    OpList   OpType = "LIST"
    OpQuery  OpType = "QUERY"   // Future
    OpCompute OpType = "COMPUTE" // Future
)

// Result represents operation results
type Result interface {
    StatusCode() int
    Headers() map[string]string
    Body() io.ReadCloser
}

// Metadata for extensibility
type Metadata map[string]interface{}

// PolicyHints guide intelligent routing
type PolicyHints struct {
    PreferredBackends []string
    CostLimit         float64
    LatencyTarget     time.Duration
    DurabilityLevel   int
}

// Query for future SQL-like operations
type Query struct {
    SQL        string
    Parameters []interface{}
}

// ResultSet for query results
type ResultSet interface {
    Next() bool
    Scan(dest ...interface{}) error
    Close() error
}

// Transaction for future ACID operations
type Transaction interface {
    Commit() error
    Rollback() error
}

// Stage in the processing pipeline
type Stage interface {
    Name() string
    Process(ctx context.Context, data io.Reader) (io.Reader, error)
}

// Backend is just one implementation of engine
type Backend interface {
    Execute(ctx context.Context, op Operation) (Result, error)
    Capabilities() Capabilities
    HealthCheck(ctx context.Context) error
}

// Capabilities describe what a backend can do
type Capabilities struct {
    SupportsCompute    bool     // WASM ready?
    SupportsSQL        bool     // Query ready?
    SupportsVersioning bool     // Epochs ready?
    SupportsErasure    bool     // RaptorQ ready?
    CostPerGB          float64
    Bandwidth          int64
    MaxObjectSize      int64
    MinObjectSize      int64
}
