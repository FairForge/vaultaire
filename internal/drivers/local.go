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
    if err := os.MkdirAll(containerPath, 0755); err != nil {
        return fmt.Errorf("create container: %w", err)
    }
    
    fullPath := filepath.Join(d.basePath, container, artifact)
    file, err := os.Create(fullPath)
    if err != nil {
        return fmt.Errorf("create file: %w", err)
    }
    defer file.Close()
    
    _, err = io.Copy(file, data)
    return err
}

// Delete removes an artifact from a container
func (d *LocalDriver) Delete(ctx context.Context, container, artifact string) error {
    fullPath := filepath.Join(d.basePath, container, artifact)
    return os.Remove(fullPath)
}

// List lists artifacts in a container
func (d *LocalDriver) List(ctx context.Context, container string) ([]string, error) {
    containerPath := filepath.Join(d.basePath, container)
    
    entries, err := os.ReadDir(containerPath)
    if err != nil {
        return nil, err
    }
    
    var artifacts []string
    for _, entry := range entries {
        if !entry.IsDir() {
            artifacts = append(artifacts, entry.Name())
        }
    }
    
    return artifacts, nil
}

// HealthCheck verifies the driver is working
func (d *LocalDriver) HealthCheck(ctx context.Context) error {
    _, err := os.Stat(d.basePath)
    return err
}
