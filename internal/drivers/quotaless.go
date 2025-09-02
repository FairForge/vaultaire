package drivers

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// QuotalessDriver wraps S3Driver with Quotaless-specific configuration
type QuotalessDriver struct {
	*S3Driver
	rootPath string // "personal-files" not "/data/personal-files"
}

// NewQuotalessDriver creates a Quotaless-specific driver
func NewQuotalessDriver(accessKey, secretKey string, logger *zap.Logger) (*QuotalessDriver, error) {
	s3Driver, err := NewS3Driver(
		"https://io.quotaless.cloud:8000",
		accessKey,
		secretKey,
		"us-east-1",
		logger,
	)
	if err != nil {
		return nil, fmt.Errorf("create S3 driver: %w", err)
	}

	return &QuotalessDriver{
		S3Driver: s3Driver,
		rootPath: "personal-files",
	}, nil
}

// Put overrides to add root path prefix
func (d *QuotalessDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	fullPath := fmt.Sprintf("%s/%s/%s", d.rootPath, container, artifact)
	return d.S3Driver.Put(ctx, "data", fullPath, data)
}

// Get overrides to add root path prefix
func (d *QuotalessDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	fullPath := fmt.Sprintf("%s/%s/%s", d.rootPath, container, artifact)
	return d.S3Driver.Get(ctx, "data", fullPath)
}

// Delete overrides to add root path prefix
func (d *QuotalessDriver) Delete(ctx context.Context, container, artifact string) error {
	fullPath := fmt.Sprintf("%s/%s/%s", d.rootPath, container, artifact)
	return d.S3Driver.Delete(ctx, "data", fullPath)
}

// List overrides to handle Quotaless-specific listing
func (d *QuotalessDriver) List(ctx context.Context, container string, prefix string) ([]string, error) {
	fullPrefix := fmt.Sprintf("%s/%s/", d.rootPath, container)
	if prefix != "" {
		fullPrefix = fmt.Sprintf("%s%s", fullPrefix, prefix)
	}

	results, err := d.S3Driver.List(ctx, "data", fullPrefix)
	if err != nil {
		return nil, err
	}

	// Strip the prefix from results to return relative paths
	var cleaned []string
	for _, result := range results {
		cleaned = append(cleaned, strings.TrimPrefix(result, fullPrefix))
	}

	return cleaned, nil
}

// HealthCheck verifies the backend is accessible
func (d *QuotalessDriver) HealthCheck(ctx context.Context) error {
	// Just check if we can list the root - this verifies connectivity
	_, err := d.S3Driver.List(ctx, "data", d.rootPath)
	if err != nil {
		return fmt.Errorf("quotaless health check failed: %w", err)
	}
	return nil
}

// Name returns the driver name
func (d *QuotalessDriver) Name() string {
	return "quotaless"
}
