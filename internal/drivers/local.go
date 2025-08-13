package drivers

import (
	"context"
	"io"
	"os"
	"path/filepath"
)

// LocalDriver implements the Driver interface for local filesystem
type LocalDriver struct {
	basePath string
}

// NewLocalDriver creates a new local filesystem driver
func NewLocalDriver(basePath string) *LocalDriver {
	return &LocalDriver{basePath: basePath}
}

// Name returns the driver name
func (d *LocalDriver) Name() string {
	return "local"
}

// Get retrieves a file
func (d *LocalDriver) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	path := filepath.Join(d.basePath, key)
	return os.Open(path)
}

// Put stores a file
func (d *LocalDriver) Put(ctx context.Context, key string, data io.Reader) error {
	path := filepath.Join(d.basePath, key)

	// Create directory if needed
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Create file
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	// Copy data
	_, err = io.Copy(file, data)
	return err
}

// Delete removes a file
func (d *LocalDriver) Delete(ctx context.Context, key string) error {
	path := filepath.Join(d.basePath, key)
	return os.Remove(path)
}

// List returns files with prefix
func (d *LocalDriver) List(ctx context.Context, prefix string) ([]string, error) {
	var files []string

	root := filepath.Join(d.basePath, prefix)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			relPath, _ := filepath.Rel(d.basePath, path)
			files = append(files, relPath)
		}
		return nil
	})

	return files, err
}

// HealthCheck verifies the driver is working
func (d *LocalDriver) HealthCheck(ctx context.Context) error {
	// Check if base path exists and is writable
	testFile := filepath.Join(d.basePath, ".health")
	file, err := os.Create(testFile)
	if err != nil {
		return err
	}
	file.Close()
	return os.Remove(testFile)
}
