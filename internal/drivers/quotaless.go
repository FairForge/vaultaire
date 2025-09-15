package drivers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// QuotalessDriver wraps S3Driver with Quotaless-specific optimizations
type QuotalessDriver struct {
	*S3Driver
	rootPath     string
	endpoint     string
	useMultipart bool
	maxRetries   int
	chunkSize    int64
	uploadCutoff int64
	logger       *zap.Logger
}

// NewQuotalessDriver creates a Quotaless-optimized driver
func NewQuotalessDriver(accessKey, secretKey, endpoint string, logger *zap.Logger) (*QuotalessDriver, error) {
	// Default to US endpoint if not specified
	if endpoint == "" {
		endpoint = "https://us.quotaless.cloud:8000"
	}

	// Determine if this is a static (single-server) or dynamic (multi-server) endpoint
	isStaticEndpoint := strings.Contains(endpoint, "srv") ||
		strings.Contains(endpoint, "nl.") ||
		strings.Contains(endpoint, "us.") ||
		strings.Contains(endpoint, "sg.")

	// Create base S3 driver with Quotaless-specific settings
	s3Driver, err := NewS3Driver(
		endpoint,
		accessKey,
		secretKey,
		"us-east-1", // Quotaless doesn't use regions
		logger,
	)
	if err != nil {
		return nil, fmt.Errorf("create S3 driver: %w", err)
	}

	// Configure based on endpoint type (per Quotaless documentation)
	driver := &QuotalessDriver{
		S3Driver:     s3Driver,
		rootPath:     "personal-files",
		endpoint:     endpoint,
		maxRetries:   3,
		chunkSize:    50 * 1024 * 1024,  // 50MB chunks as recommended
		uploadCutoff: 100 * 1024 * 1024, // 100MB cutoff as recommended
		logger:       logger,
	}

	// Static endpoints support multipart, dynamic endpoints don't
	driver.useMultipart = isStaticEndpoint

	if isStaticEndpoint {
		logger.Info("Quotaless using static endpoint with multipart",
			zap.String("endpoint", endpoint))
	} else {
		logger.Info("Quotaless using dynamic endpoint without multipart",
			zap.String("endpoint", endpoint))
	}

	return driver, nil
}

// Put implements optimized upload for Quotaless with retry logic
func (d *QuotalessDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	fullPath := fmt.Sprintf("%s/%s/%s", d.rootPath, container, artifact)

	// Buffer the data for retries if it's not already seekable
	var buffer *bytes.Buffer
	if _, ok := data.(io.Seeker); !ok {
		// Read into buffer for retry capability
		buf, err := io.ReadAll(data)
		if err != nil {
			return fmt.Errorf("buffer data: %w", err)
		}
		buffer = bytes.NewBuffer(buf)
		data = buffer
	}

	// Retry logic for resilience
	var lastErr error
	for attempt := 0; attempt < d.maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff
			backoff := time.Duration(attempt*attempt) * time.Second
			d.logger.Warn("retrying upload",
				zap.Int("attempt", attempt+1),
				zap.String("path", fullPath),
				zap.Duration("backoff", backoff),
				zap.Error(lastErr))
			time.Sleep(backoff)

			// Reset reader if we buffered it
			if buffer != nil {
				data = bytes.NewReader(buffer.Bytes())
			} else if seeker, ok := data.(io.Seeker); ok {
				_, _ = seeker.Seek(0, io.SeekStart)
			}
		}

		// Use the S3Driver's Put method directly
		// Note: S3Driver.Put doesn't take variadic options, just the reader
		lastErr = d.S3Driver.Put(ctx, "data", fullPath, data)

		if lastErr == nil {
			d.logger.Debug("upload successful",
				zap.String("container", container),
				zap.String("artifact", artifact),
				zap.Int("attempts", attempt+1))
			return nil
		}
	}

	return fmt.Errorf("upload failed after %d attempts: %w", d.maxRetries, lastErr)
}

// Get retrieves data with retry logic
func (d *QuotalessDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	fullPath := fmt.Sprintf("%s/%s/%s", d.rootPath, container, artifact)

	var lastErr error
	for attempt := 0; attempt < d.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * time.Second
			d.logger.Warn("retrying get",
				zap.Int("attempt", attempt+1),
				zap.String("path", fullPath),
				zap.Duration("backoff", backoff),
				zap.Error(lastErr))
			time.Sleep(backoff)
		}

		reader, err := d.S3Driver.Get(ctx, "data", fullPath)
		if err == nil {
			return reader, nil
		}
		lastErr = err
	}

	return nil, fmt.Errorf("get failed after %d attempts: %w", d.maxRetries, lastErr)
}

// Delete with retry logic
func (d *QuotalessDriver) Delete(ctx context.Context, container, artifact string) error {
	fullPath := fmt.Sprintf("%s/%s/%s", d.rootPath, container, artifact)

	var lastErr error
	for attempt := 0; attempt < d.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * time.Second
			d.logger.Warn("retrying delete",
				zap.Int("attempt", attempt+1),
				zap.String("path", fullPath),
				zap.Duration("backoff", backoff),
				zap.Error(lastErr))
			time.Sleep(backoff)
		}

		err := d.S3Driver.Delete(ctx, "data", fullPath)
		if err == nil {
			return nil
		}
		lastErr = err
	}

	return fmt.Errorf("delete failed after %d attempts: %w", d.maxRetries, lastErr)
}

// List with proper prefix handling for Quotaless structure
func (d *QuotalessDriver) List(ctx context.Context, container string, prefix string) ([]string, error) {
	fullPrefix := fmt.Sprintf("%s/%s/", d.rootPath, container)
	if prefix != "" {
		fullPrefix = fmt.Sprintf("%s%s", fullPrefix, prefix)
	}

	var results []string
	var lastErr error

	for attempt := 0; attempt < d.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * time.Second
			d.logger.Warn("retrying list",
				zap.Int("attempt", attempt+1),
				zap.String("prefix", fullPrefix),
				zap.Duration("backoff", backoff),
				zap.Error(lastErr))
			time.Sleep(backoff)
		}

		results, lastErr = d.S3Driver.List(ctx, "data", fullPrefix)
		if lastErr == nil {
			break
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("list failed after %d attempts: %w", d.maxRetries, lastErr)
	}

	// Strip the prefix to return relative paths
	prefixLen := len(fullPrefix)
	cleaned := make([]string, 0, len(results))
	for _, result := range results {
		if len(result) > prefixLen {
			cleaned = append(cleaned, result[prefixLen:])
		}
	}

	return cleaned, nil
}

// HealthCheck with timeout and retry
func (d *QuotalessDriver) HealthCheck(ctx context.Context) error {
	// Create a timeout context for health check
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Try a simple list operation with minimal results
	_, err := d.S3Driver.List(checkCtx, "data", d.rootPath+"/")
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}

	return nil
}

// GetMetrics returns performance metrics
func (d *QuotalessDriver) GetMetrics() map[string]interface{} {
	return map[string]interface{}{
		"endpoint":         d.endpoint,
		"multipart":        d.useMultipart,
		"chunk_size_mb":    d.chunkSize / (1024 * 1024),
		"upload_cutoff_mb": d.uploadCutoff / (1024 * 1024),
		"max_retries":      d.maxRetries,
		"root_path":        d.rootPath,
	}
}

// Name returns the driver name
func (d *QuotalessDriver) Name() string {
	return "quotaless"
}
