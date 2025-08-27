package drivers

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

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

	defer func() {
		if err := file.Close(); err != nil {
			d.logger.Error("failed to close file", // Changed from l.logger to d.logger
				zap.String("path", fullPath), // Changed from path to fullPath
				zap.Error(err))
		}
	}()

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

	err := filepath.Walk(containerPath, func(path string, info os.FileInfo, err error) error {
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

	if err != nil {
		return nil, fmt.Errorf("failed to walk directory: %w", err)
	}

	return artifacts, nil
}

// HealthCheck verifies the driver is working
func (d *LocalDriver) HealthCheck(ctx context.Context) error {
	_, err := os.Stat(d.basePath)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	return nil // Return nil on success!

}

// GetOptions configures Get behavior
type GetOptions struct {
	FollowSymlinks bool
}

// FileInfo extends basic file information
type FileInfo struct {
	Name          string
	Size          int64
	IsDir         bool
	IsSymlink     bool
	SymlinkTarget string
	Mode          os.FileMode
	ModTime       time.Time
}

// SupportsSymlinks checks if the filesystem supports symlinks
func (d *LocalDriver) SupportsSymlinks() bool {
	testFile := filepath.Join(d.basePath, ".symlink-test")
	testLink := filepath.Join(d.basePath, ".symlink-test-link")

	// Clean up any previous test files
	os.Remove(testLink)
	os.Remove(testFile)
	defer os.Remove(testLink)
	defer os.Remove(testFile)

	// Create test file
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		return false
	}

	// Try to create symlink
	if err := os.Symlink(testFile, testLink); err != nil {
		return false
	}

	return true
}

// GetWithOptions retrieves an artifact with configurable options
func (d *LocalDriver) GetWithOptions(ctx context.Context, container, artifact string, opts GetOptions) (io.ReadCloser, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	// Check if it's a symlink
	info, err := os.Lstat(fullPath) // Lstat doesn't follow symlinks
	if err != nil {
		return nil, fmt.Errorf("lstat failed: %w", err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		if !opts.FollowSymlinks {
			return nil, fmt.Errorf("artifact is a symlink and FollowSymlinks is false")
		}
		// Use os.Open which follows symlinks
		return os.Open(fullPath)
	}

	return os.Open(fullPath)
}

// GetInfo returns detailed information about an artifact
func (d *LocalDriver) GetInfo(ctx context.Context, container, artifact string) (*FileInfo, error) {
	fullPath := filepath.Join(d.basePath, container, artifact)

	info, err := os.Lstat(fullPath)
	if err != nil {
		return nil, fmt.Errorf("lstat failed: %w", err)
	}

	fi := &FileInfo{
		Name:    info.Name(),
		Size:    info.Size(),
		IsDir:   info.IsDir(),
		Mode:    info.Mode(),
		ModTime: info.ModTime(),
	}

	// Check if it's a symlink
	if info.Mode()&os.ModeSymlink != 0 {
		fi.IsSymlink = true
		// Read symlink target
		if target, err := os.Readlink(fullPath); err == nil {
			fi.SymlinkTarget = target // Keep full path
		}
	}

	return fi, nil
}
