package engine

import (
	"context"
	"io"
	"time"
)

// Container replaces Bucket - ready for compute/ML workloads
type Container struct {
	ID       string                 `json:"id"`
	Name     string                 `json:"name"`
	Type     string                 `json:"type"` // "storage", "compute", "ml-model", "vector-db"
	Created  time.Time              `json:"created"`
	Metadata map[string]interface{} `json:"metadata"`

	// Hidden ML fields (not exposed in S3 API)
	Temperature float64 `json:"-"` // Access frequency score
	Tier        string  `json:"-"` // "hot", "warm", "cold", "archive"
}

// Artifact replaces Object - ready for any data type
type Artifact struct {
	ID        string                 `json:"id"`
	Container string                 `json:"container"`
	Key       string                 `json:"key"`
	Type      string                 `json:"type"` // "blob", "wasm", "model", "vector", "dataset"
	Size      int64                  `json:"size"`
	Modified  time.Time              `json:"modified"`
	ETag      string                 `json:"etag"`
	Metadata  map[string]interface{} `json:"metadata"`

	// Hidden ML/AI fields
	Features    map[string]float64 `json:"-"` // For ML training
	Vector      []float64          `json:"-"` // For similarity search
	AccessCount int64              `json:"-"` // For smart caching
	LastAccess  time.Time          `json:"-"` // For tiering decisions
}

// Operation represents any engine operation (not just storage)
type Operation struct {
	Type      string // "get", "put", "delete", "compute", "query"
	Container string
	Artifact  string
	Context   context.Context
	Metadata  map[string]interface{}
}

// Result can hold any operation result
type Result struct {
	Success  bool
	Data     io.ReadCloser
	Metadata map[string]interface{}
	Error    error
}
