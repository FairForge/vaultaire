// internal/engine/migrator.go
package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Migrator handles data migration between backends
type Migrator struct {
	logger *zap.Logger
}

// MigrationOptions controls migration behavior
type MigrationOptions struct {
	Workers       int
	BatchSize     int
	VerifyMode    bool
	DeleteSource  bool
	RetryAttempts int
}

// MigrationStats tracks migration progress
type MigrationStats struct {
	ObjectsProcessed int64
	BytesTransferred int64
	Failed           int64
	StartTime        time.Time
	EndTime          time.Time
}

// NewMigrator creates a migrator
func NewMigrator() *Migrator {
	logger, _ := zap.NewDevelopment()
	return &Migrator{
		logger: logger,
	}
}

// MigrateObject migrates a single object
func (m *Migrator) MigrateObject(ctx context.Context,
	source, dest Driver, container, key string) error {

	// Read from source
	reader, err := source.Get(ctx, container, key)
	if err != nil {
		return fmt.Errorf("reading from source: %w", err)
	}
	defer func() { _ = reader.Close() }()

	// Write to destination
	if err := dest.Put(ctx, container, key, reader); err != nil {
		return fmt.Errorf("writing to destination: %w", err)
	}

	return nil
}

// MigrateContainer migrates an entire container
func (m *Migrator) MigrateContainer(ctx context.Context,
	source, dest Driver, container string, opts MigrationOptions) (*MigrationStats, error) {

	stats := &MigrationStats{
		StartTime: time.Now(),
	}

	// List all objects
	objects, err := source.List(ctx, container, "")
	if err != nil {
		return stats, fmt.Errorf("listing objects: %w", err)
	}

	// Create worker pool
	jobs := make(chan string, len(objects))
	var wg sync.WaitGroup

	// Start workers
	for i := 0; i < opts.Workers; i++ {
		wg.Add(1)
		go m.worker(ctx, &wg, jobs, source, dest, container, stats)
	}

	// Queue jobs
	for _, obj := range objects {
		jobs <- obj
	}
	close(jobs)

	wg.Wait()
	stats.EndTime = time.Now()

	return stats, nil
}

func (m *Migrator) worker(ctx context.Context, wg *sync.WaitGroup,
	jobs <-chan string, source, dest Driver, container string, stats *MigrationStats) {
	defer wg.Done()

	for key := range jobs {
		if err := m.MigrateObject(ctx, source, dest, container, key); err != nil {
			atomic.AddInt64(&stats.Failed, 1)
			m.logger.Error("migration failed", zap.String("key", key), zap.Error(err))
		} else {
			atomic.AddInt64(&stats.ObjectsProcessed, 1)
		}
	}
}
