package drivers

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.uber.org/zap"
)

// LocalDriver implements the Driver interface for local filesystem
type LocalDriver struct {
	basePath string
	logger   *zap.Logger
}

// NewLocalDriver creates a new local filesystem driver
func NewLocalDriver(basePath string, logger *zap.Logger) *LocalDriver {
	return &LocalDriver{
		basePath: basePath,
		logger:   logger,
	}
}

// Name returns the driver name
func (d *LocalDriver) Name() string {
	return "local"
}

// Get retrieves an artifact from a container
func (d *LocalDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	d.logger.Debug("LocalDriver.Get",
		zap.String("container", container),
		zap.String("artifact", artifact),
		zap.String("fullPath", fullPath))

	return os.Open(fullPath)
}

// Put stores an artifact in a container
func (d *LocalDriver) Put(ctx context.Context, container, artifact string, data io.Reader) error {
	containerPath := filepath.Join(d.basePath, container)
	if err := os.MkdirAll(containerPath, 0750); err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	
	fullPath := filepath.Join(d.basePath, container, artifact)
	
	// Create parent directory if artifact has subdirectories
	parentDir := filepath.Dir(fullPath)
	if err := os.MkdirAll(parentDir, 0750); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	
	file, err := os.Create(fullPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer file.Close()
	
	_, err = io.Copy(file, data)
	if err != nil {
		return fmt.Errorf("failed to copy data: %w", err)
	}
	
	return nil
}

// Delete removes an artifact from a container
func (d *LocalDriver) Delete(ctx context.Context, container, artifact string) error {
	fullPath := filepath.Join(d.basePath, container, artifact)
	return os.Remove(fullPath)
}

// List lists artifacts in a container
func (d *LocalDriver) List(ctx context.Context, container string) ([]string, error) {
	containerPath := filepath.Join(d.basePath, container)
	var artifacts []string
	
	filepath.Walk(containerPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors, continue walking
		}
		if !info.IsDir() {
			if rel, err := filepath.Rel(containerPath, path); err == nil {
				artifacts = append(artifacts, rel)
			}
		}
		return nil
	})
	
	return artifacts, nil
}

// HealthCheck verifies the driver is working
func (d *LocalDriver) HealthCheck(ctx context.Context) error {
	_, err := os.Stat(d.basePath)
	return fmt.Errorf("health check failed: %w", err)
}
