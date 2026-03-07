package engine

import (
	"context"
	"io"
)

// Engine is the universal interface for storage, compute, and ML
type Engine interface {
	// Storage operations (visible to users)
	Get(ctx context.Context, container, artifact string) (io.ReadCloser, error)

	// Put stores an artifact and returns the name of the backend it was
	// written to. Callers must persist this value alongside object metadata
	// so that Get can route the read to the same backend, regardless of
	// what the intelligence / cost-optimizer would otherwise select.
	Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) (string, error)

	Delete(ctx context.Context, container, artifact string) error
	List(ctx context.Context, container, prefix string) ([]Artifact, error)

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

// Driver interface for storage backends
type Driver interface {
	Name() string
	Get(ctx context.Context, container, artifact string) (io.ReadCloser, error)
	Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) error
	Delete(ctx context.Context, container, artifact string) error
	List(ctx context.Context, container, prefix string) ([]string, error)
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

// PutOption is a function that configures Put operations
type PutOption func(*PutOptions)

// PutOptions holds options for Put operations
type PutOptions struct {
	ContentType     string
	CacheControl    string
	ContentEncoding string
	ContentLanguage string
	UserMetadata    map[string]string
}

// WithContentType sets the content type
func WithContentType(ct string) PutOption {
	return func(o *PutOptions) {
		o.ContentType = ct
	}
}

// WithUserMetadata sets user metadata
func WithUserMetadata(meta map[string]string) PutOption {
	return func(o *PutOptions) {
		o.UserMetadata = meta
	}
}
