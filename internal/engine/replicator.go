// internal/engine/replicator.go
package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
)

// ReplicationMode defines how replication happens
type ReplicationMode int

const (
	SyncReplication   ReplicationMode = iota // Wait for all
	AsyncReplication                         // Primary only, queue others
	QuorumReplication                        // Wait for majority
)

// Replicator handles cross-backend replication
type Replicator struct {
	mode  ReplicationMode
	queue chan ReplicationJob
}

// ReplicationJob represents a pending replication
type ReplicationJob struct {
	Driver    Driver
	Container string
	Artifact  string
	Data      []byte
}

// NewReplicator creates a replicator
func NewReplicator(mode ReplicationMode) *Replicator {
	r := &Replicator{
		mode:  mode,
		queue: make(chan ReplicationJob, 1000),
	}

	// Start async workers
	for i := 0; i < 5; i++ {
		go r.worker()
	}

	return r
}

// Replicate copies data to multiple backends
func (r *Replicator) Replicate(ctx context.Context, drivers []Driver,
	container, artifact string, data io.Reader) error {

	if len(drivers) == 0 {
		return fmt.Errorf("no drivers provided")
	}

	// Read data once into memory (TODO: optimize for large files)
	buf, err := io.ReadAll(data)
	if err != nil {
		return fmt.Errorf("reading data: %w", err)
	}

	switch r.mode {
	case SyncReplication:
		return r.replicateSync(ctx, drivers, container, artifact, buf)
	case AsyncReplication:
		return r.replicateAsync(ctx, drivers, container, artifact, buf)
	case QuorumReplication:
		return r.replicateQuorum(ctx, drivers, container, artifact, buf)
	default:
		return r.replicateSync(ctx, drivers, container, artifact, buf)
	}
}

func (r *Replicator) replicateSync(ctx context.Context, drivers []Driver,
	container, artifact string, data []byte) error {

	var wg sync.WaitGroup
	errChan := make(chan error, len(drivers))

	for _, driver := range drivers {
		wg.Add(1)
		go func(d Driver) {
			defer wg.Done()
			reader := bytes.NewReader(data)
			if err := d.Put(ctx, container, artifact, reader); err != nil {
				errChan <- fmt.Errorf("driver %T: %w", d, err)
			}
		}(driver)
	}

	wg.Wait()
	close(errChan)

	// Collect errors
	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("replication failed: %v", errs)
	}

	return nil
}

func (r *Replicator) replicateAsync(ctx context.Context, drivers []Driver,
	container, artifact string, data []byte) error {

	// Write to primary synchronously
	if len(drivers) > 0 {
		reader := bytes.NewReader(data)
		if err := drivers[0].Put(ctx, container, artifact, reader); err != nil {
			return fmt.Errorf("primary write failed: %w", err)
		}
	}

	// Queue secondary writes
	for i := 1; i < len(drivers); i++ {
		job := ReplicationJob{
			Driver:    drivers[i],
			Container: container,
			Artifact:  artifact,
			Data:      data,
		}

		select {
		case r.queue <- job:
			// Queued successfully
		default:
			// Queue full, log and continue
		}
	}

	return nil
}

func (r *Replicator) replicateQuorum(ctx context.Context, drivers []Driver,
	container, artifact string, data []byte) error {

	required := (len(drivers) / 2) + 1 // Majority
	successChan := make(chan bool, len(drivers))

	var wg sync.WaitGroup
	for _, driver := range drivers {
		wg.Add(1)
		go func(d Driver) {
			defer wg.Done()
			reader := bytes.NewReader(data)
			if err := d.Put(ctx, container, artifact, reader); err == nil {
				successChan <- true
			}
		}(driver)
	}

	// Wait for all to complete
	go func() {
		wg.Wait()
		close(successChan)
	}()

	// Count successes
	successes := 0
	for range successChan {
		successes++
	}

	if successes < required {
		return fmt.Errorf("quorum not reached: %d/%d succeeded", successes, required)
	}

	return nil
}

// worker processes async replication jobs
func (r *Replicator) worker() {
	for job := range r.queue {
		reader := bytes.NewReader(job.Data)
		_ = job.Driver.Put(context.Background(), job.Container, job.Artifact, reader)
		// TODO: Add retry logic and error handling
	}
}

// Shutdown stops the replicator
func (r *Replicator) Shutdown() {
	close(r.queue)
}
