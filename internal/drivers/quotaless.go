package drivers

import (
	"context"
	"fmt"
	"go.uber.org/zap"
	"io"
)

// QuotalessDriver wraps S3Driver with Quotaless-specific configuration
type QuotalessDriver struct {
	*S3Driver
	rootPath string // "/data/personal-files"
}

// NewQuotalessDriver creates a Quotaless-specific driver
func NewQuotalessDriver(accessKey, secretKey string, logger *zap.Logger) (*QuotalessDriver, error) {
	s3Driver, err := NewS3Driver(
		"https://io.quotaless.cloud:8000",
		accessKey,
		secretKey,
		"us-east-1", // Quotaless doesn't care about region
		logger,
	)
	if err != nil {
		return nil, fmt.Errorf("create S3 driver: %w", err)
	}

	return &QuotalessDriver{
		S3Driver: s3Driver,
		rootPath: "/data/personal-files",
	}, nil
}

// Put overrides to add root path prefix
func (d *QuotalessDriver) Put(ctx context.Context, container, artifact string, data io.Reader) error {
	fullPath := fmt.Sprintf("%s/%s/%s", d.rootPath, container, artifact)
	return d.S3Driver.Put(ctx, "data", fullPath, data)
}
