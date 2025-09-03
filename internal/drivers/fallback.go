// internal/drivers/fallback.go
package drivers

import (
	"context"
	"fmt"
	"io"

	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// FallbackDriver tries primary driver first, falls back to secondary on failure
type FallbackDriver struct {
	primary   Driver
	secondary Driver
	logger    *zap.Logger
}

// NewFallbackDriver creates a driver with fallback capability
func NewFallbackDriver(primary, secondary Driver, logger *zap.Logger) *FallbackDriver {
	return &FallbackDriver{
		primary:   primary,
		secondary: secondary,
		logger:    logger,
	}
}

// Get tries primary first, then secondary
func (f *FallbackDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	reader, err := f.primary.Get(ctx, container, artifact)
	if err == nil {
		return reader, nil
	}

	f.logger.Warn("primary backend failed, trying secondary",
		zap.String("container", container),
		zap.String("artifact", artifact),
		zap.Error(err))

	reader, err = f.secondary.Get(ctx, container, artifact)
	if err != nil {
		return nil, fmt.Errorf("all backends failed: primary: %v, secondary: %w", err, err)
	}

	return reader, nil
}

// Put writes to primary, optionally replicates to secondary
func (f *FallbackDriver) Put(ctx context.Context, container, artifact string,
	data io.Reader, opts ...engine.PutOption) error {

	err := f.primary.Put(ctx, container, artifact, data, opts...)
	if err == nil {
		// Optionally replicate to secondary asynchronously
		// This could be done in a goroutine for performance
		return nil
	}

	f.logger.Warn("primary backend failed for put, trying secondary",
		zap.String("container", container),
		zap.String("artifact", artifact),
		zap.Error(err))

	err = f.secondary.Put(ctx, container, artifact, data, opts...)
	if err != nil {
		return fmt.Errorf("all backends failed: %w", err)
	}

	return nil
}

// Delete from both backends
func (f *FallbackDriver) Delete(ctx context.Context, container, artifact string) error {
	primaryErr := f.primary.Delete(ctx, container, artifact)
	secondaryErr := f.secondary.Delete(ctx, container, artifact)

	// Success if at least one succeeded
	if primaryErr == nil || secondaryErr == nil {
		return nil
	}

	return fmt.Errorf("all backends failed: primary: %v, secondary: %v", primaryErr, secondaryErr)
}

// List from primary, fallback to secondary
func (f *FallbackDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	files, err := f.primary.List(ctx, container, prefix)
	if err == nil {
		return files, nil
	}

	f.logger.Warn("primary backend failed for list, trying secondary",
		zap.String("container", container),
		zap.String("prefix", prefix),
		zap.Error(err))

	files, err = f.secondary.List(ctx, container, prefix)
	if err != nil {
		return nil, fmt.Errorf("all backends failed: %w", err)
	}

	return files, nil
}

// Exists checks primary first, then secondary
func (f *FallbackDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	exists, err := f.primary.Exists(ctx, container, artifact)
	if err == nil {
		return exists, nil
	}

	exists, err = f.secondary.Exists(ctx, container, artifact)
	if err != nil {
		return false, fmt.Errorf("all backends failed: %w", err)
	}

	return exists, nil
}
