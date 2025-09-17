package drivers

import (
	"context"

	"io"
	"sync"

	"go.uber.org/zap"
)

// ParallelDriver wraps any driver with parallel execution
type ParallelDriver struct {
	backend Driver
	workers int
	logger  *zap.Logger
}

// NewParallelDriver creates a driver that executes operations in parallel
func NewParallelDriver(backend Driver, workers int, logger *zap.Logger) *ParallelDriver {
	if workers <= 0 {
		workers = 4 // Default worker count
	}
	return &ParallelDriver{
		backend: backend,
		workers: workers,
		logger:  logger,
	}
}

// ParallelPut writes multiple files concurrently
func (d *ParallelDriver) ParallelPut(ctx context.Context, operations []PutOperation) []error {
	sem := make(chan struct{}, d.workers) // Limit concurrency
	errors := make([]error, len(operations))
	var wg sync.WaitGroup

	for i, op := range operations {
		wg.Add(1)
		go func(idx int, operation PutOperation) {
			defer wg.Done()
			sem <- struct{}{}        // Acquire
			defer func() { <-sem }() // Release

			errors[idx] = d.backend.Put(ctx, operation.Container, operation.Artifact, operation.Data)
		}(i, op)
	}

	wg.Wait()
	return errors
}

// ParallelGet reads multiple files concurrently
func (d *ParallelDriver) ParallelGet(ctx context.Context, operations []GetOperation) []GetResult {
	sem := make(chan struct{}, d.workers)
	results := make([]GetResult, len(operations))
	var wg sync.WaitGroup

	for i, op := range operations {
		wg.Add(1)
		go func(idx int, operation GetOperation) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			reader, err := d.backend.Get(ctx, operation.Container, operation.Artifact)
			results[idx] = GetResult{Reader: reader, Error: err}
		}(i, op)
	}

	wg.Wait()
	return results
}

// PutOperation describes a parallel put
type PutOperation struct {
	Container string
	Artifact  string
	Data      io.Reader
}

// GetOperation describes a parallel get
type GetOperation struct {
	Container string
	Artifact  string
}

// GetResult contains the result of a parallel get
type GetResult struct {
	Reader io.ReadCloser
	Error  error
}
