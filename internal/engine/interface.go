package engine

import (
    "context"
    "io"
)

// Engine is the universal interface for storage, compute, and ML
type Engine interface {
    // Storage operations (visible to users)
    Get(ctx context.Context, container, artifact string) (io.ReadCloser, error)
    Put(ctx context.Context, container, artifact string, data io.Reader) error
    Delete(ctx context.Context, container, artifact string) error
    List(ctx context.Context, container string) ([]Artifact, error)

    // Hidden capabilities (implement as no-ops for now)
    Execute(ctx context.Context, container string, wasm []byte, input io.Reader) (io.Reader, error)
    Query(ctx context.Context, sql string) (ResultSet, error)
    Train(ctx context.Context, model string, data []byte) error
    Predict(ctx context.Context, model string, input []byte) ([]byte, error)

    // Metadata operations
    GetContainerMetadata(ctx context.Context, container string) (*Container, error)
    GetArtifactMetadata(ctx context.Context, container, artifact string) (*Artifact, error)

    // Health and metrics
    HealthCheck(ctx context.Context) error
    GetMetrics(ctx context.Context) (map[string]interface{}, error)
}

// Driver interface for storage backends - FIXED to use container/artifact
type Driver interface {
    Name() string
    Get(ctx context.Context, container, artifact string) (io.ReadCloser, error)
    Put(ctx context.Context, container, artifact string, data io.Reader) error
    Delete(ctx context.Context, container, artifact string) error
    List(ctx context.Context, container string) ([]string, error)
    HealthCheck(ctx context.Context) error
}

// ResultSet for query operations (future use)
type ResultSet interface {
    Next() bool
    Scan(dest ...interface{}) error
    Close() error
}

// ComputeEngine for WASM execution (future use)
type ComputeEngine interface {
    LoadModule(wasm []byte) error
    Execute(input []byte) ([]byte, error)
}

// MLEngine for machine learning operations (future use)
type MLEngine interface {
    LoadModel(path string) error
    Train(data [][]float64, labels []float64) error
    Predict(input []float64) (float64, error)
}
