package drivers

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// RegionalFailover implements automatic failover between regions
type RegionalFailover struct {
	primary   Driver
	secondary Driver
	logger    *zap.Logger

	primaryHealthy   atomic.Bool
	secondaryHealthy atomic.Bool

	recoveryInterval time.Duration
	stopRecovery     chan struct{}
	mu               sync.RWMutex
}

// RegionHealthStatus represents the health of regions
type RegionHealthStatus struct {
	PrimaryHealthy   bool
	SecondaryHealthy bool
	LastCheck        time.Time
}

// RegionDriver interface for regional drivers
type RegionDriver interface {
	Get(ctx context.Context, container, artifact string) (io.ReadCloser, error)
	Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error
	Delete(ctx context.Context, container, artifact string) error
	List(ctx context.Context, container string, prefix string) ([]string, error)
	Exists(ctx context.Context, container, artifact string) (bool, error)
}

// NewRegionalFailover creates a new regional failover manager
func NewRegionalFailover(primary, secondary RegionDriver, logger *zap.Logger) *RegionalFailover {
	rf := &RegionalFailover{
		primary:          primary,
		secondary:        secondary,
		logger:           logger,
		recoveryInterval: 30 * time.Second,
		stopRecovery:     make(chan struct{}),
	}

	rf.primaryHealthy.Store(true)
	rf.secondaryHealthy.Store(true)

	// Start health monitoring
	go rf.monitorHealth()

	return rf
}

// Get retrieves an object with failover
func (rf *RegionalFailover) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	if rf.primaryHealthy.Load() {
		reader, err := rf.primary.Get(ctx, container, artifact)
		if err == nil {
			return reader, nil
		}
		rf.handlePrimaryError(err)
	}

	if rf.secondaryHealthy.Load() {
		reader, err := rf.secondary.Get(ctx, container, artifact)
		if err == nil {
			return reader, nil
		}
		rf.handleSecondaryError(err)
		return nil, err
	}

	return nil, fmt.Errorf("all regions unavailable")
}

// Put stores an object with failover
func (rf *RegionalFailover) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	if rf.primaryHealthy.Load() {
		err := rf.primary.Put(ctx, container, artifact, data, opts...)
		if err == nil {
			// Async replicate to secondary
			go rf.replicateToSecondary(ctx, container, artifact, data)
			return nil
		}
		rf.handlePrimaryError(err)
	}

	if rf.secondaryHealthy.Load() {
		err := rf.secondary.Put(ctx, container, artifact, data, opts...)
		if err == nil {
			return nil
		}
		rf.handleSecondaryError(err)
		return err
	}

	return fmt.Errorf("all regions unavailable")
}

// Delete removes an object with failover
func (rf *RegionalFailover) Delete(ctx context.Context, container, artifact string) error {
	var lastErr error

	if rf.primaryHealthy.Load() {
		err := rf.primary.Delete(ctx, container, artifact)
		if err == nil {
			// Also delete from secondary
			_ = rf.secondary.Delete(ctx, container, artifact)
			return nil
		}
		lastErr = err
		rf.handlePrimaryError(err)
	}

	if rf.secondaryHealthy.Load() {
		err := rf.secondary.Delete(ctx, container, artifact)
		if err == nil {
			return nil
		}
		lastErr = err
		rf.handleSecondaryError(err)
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("all regions unavailable")
}

// List returns objects with failover
func (rf *RegionalFailover) List(ctx context.Context, container string, prefix string) ([]string, error) {
	if rf.primaryHealthy.Load() {
		results, err := rf.primary.List(ctx, container, prefix)
		if err == nil {
			return results, nil
		}
		rf.handlePrimaryError(err)
	}

	if rf.secondaryHealthy.Load() {
		results, err := rf.secondary.List(ctx, container, prefix)
		if err == nil {
			return results, nil
		}
		rf.handleSecondaryError(err)
		return nil, err
	}

	return nil, fmt.Errorf("all regions unavailable")
}

// Exists checks if object exists with failover
func (rf *RegionalFailover) Exists(ctx context.Context, container, artifact string) (bool, error) {
	if rf.primaryHealthy.Load() {
		exists, err := rf.primary.Exists(ctx, container, artifact)
		if err == nil {
			return exists, nil
		}
		rf.handlePrimaryError(err)
	}

	if rf.secondaryHealthy.Load() {
		exists, err := rf.secondary.Exists(ctx, container, artifact)
		if err == nil {
			return exists, nil
		}
		rf.handleSecondaryError(err)
		return false, err
	}

	return false, fmt.Errorf("all regions unavailable")
}

// MarkUnhealthy manually marks a region as unhealthy
func (rf *RegionalFailover) MarkUnhealthy(region string) {
	switch region {
	case "primary":
		rf.primaryHealthy.Store(false)
		rf.logger.Warn("primary region marked unhealthy")
	case "secondary":
		rf.secondaryHealthy.Store(false)
		rf.logger.Warn("secondary region marked unhealthy")
	}
}

// GetHealthStatus returns current health status
func (rf *RegionalFailover) GetHealthStatus() RegionHealthStatus {
	return RegionHealthStatus{
		PrimaryHealthy:   rf.primaryHealthy.Load(),
		SecondaryHealthy: rf.secondaryHealthy.Load(),
		LastCheck:        time.Now(),
	}
}

// SetRecoveryInterval sets the health check interval
func (rf *RegionalFailover) SetRecoveryInterval(interval time.Duration) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	rf.recoveryInterval = interval
}

// Internal methods

func (rf *RegionalFailover) handlePrimaryError(err error) {
	rf.primaryHealthy.Store(false)
	rf.logger.Error("primary region error", zap.Error(err))
}

func (rf *RegionalFailover) handleSecondaryError(err error) {
	rf.secondaryHealthy.Store(false)
	rf.logger.Error("secondary region error", zap.Error(err))
}

func (rf *RegionalFailover) monitorHealth() {
	rf.mu.RLock()
	interval := rf.recoveryInterval
	rf.mu.RUnlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rf.probeHealth()
		case <-rf.stopRecovery:
			return
		}
	}
}

func (rf *RegionalFailover) probeHealth() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Probe primary
	if !rf.primaryHealthy.Load() {
		_, err := rf.primary.List(ctx, "health-check", "")
		if err == nil {
			rf.primaryHealthy.Store(true)
			rf.logger.Info("primary region recovered")
		}
	}

	// Probe secondary
	if !rf.secondaryHealthy.Load() {
		_, err := rf.secondary.List(ctx, "health-check", "")
		if err == nil {
			rf.secondaryHealthy.Store(true)
			rf.logger.Info("secondary region recovered")
		}
	}
}

func (rf *RegionalFailover) replicateToSecondary(ctx context.Context, container, artifact string, data io.Reader) {
	// In production, this would properly buffer and replicate the data
	rf.logger.Debug("replicating to secondary",
		zap.String("container", container),
		zap.String("artifact", artifact),
	)
}

// Stop stops the health monitoring
func (rf *RegionalFailover) Stop() {
	close(rf.stopRecovery)
}
