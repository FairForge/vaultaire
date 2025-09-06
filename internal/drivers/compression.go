// internal/drivers/compression.go
package drivers

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"

	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

type CompressionDriver struct {
	backend   engine.Driver // or just Driver if using the local interface
	algorithm string
	logger    *zap.Logger
}

func NewCompressionDriver(backend engine.Driver, algorithm string, logger *zap.Logger) *CompressionDriver {
	return &CompressionDriver{
		backend:   backend,
		algorithm: algorithm,
		logger:    logger,
	}
}

func (c *CompressionDriver) Put(ctx context.Context, container, artifact string,
	data io.Reader, opts ...engine.PutOption) error {
	// Compress data before storing
	var compressed bytes.Buffer

	switch c.algorithm {
	case "gzip":
		gw := gzip.NewWriter(&compressed)
		if _, err := io.Copy(gw, data); err != nil {
			return fmt.Errorf("compress data: %w", err)
		}
		if err := gw.Close(); err != nil {
			return fmt.Errorf("close gzip writer: %w", err)
		}
	default:
		return fmt.Errorf("unsupported algorithm: %s", c.algorithm)
	}

	// Store with .gz extension
	compressedName := artifact + ".gz"
	return c.backend.Put(ctx, container, compressedName, &compressed, opts...)
}

func (c *CompressionDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	// Get compressed data
	compressedName := artifact + ".gz"
	reader, err := c.backend.Get(ctx, container, compressedName)
	if err != nil {
		return nil, err
	}

	// Decompress based on algorithm
	switch c.algorithm {
	case "gzip":
		gr, err := gzip.NewReader(reader)
		if err != nil {
			_ = reader.Close()
			return nil, fmt.Errorf("create gzip reader: %w", err)
		}

		// Return wrapper that closes both readers
		return &compressedReader{
			gzipReader: gr,
			underlying: reader,
		}, nil
	default:
		_ = reader.Close()
		return nil, fmt.Errorf("unsupported algorithm: %s", c.algorithm)
	}
}

type compressedReader struct {
	gzipReader *gzip.Reader
	underlying io.ReadCloser
}

func (r *compressedReader) Read(p []byte) (int, error) {
	return r.gzipReader.Read(p)
}

func (r *compressedReader) Close() error {
	_ = r.gzipReader.Close()
	return r.underlying.Close()
}

// Delegate other methods
func (c *CompressionDriver) Delete(ctx context.Context, container, artifact string) error {
	return c.backend.Delete(ctx, container, artifact+".gz")
}

func (c *CompressionDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	return c.backend.List(ctx, container, prefix)
}

func (c *CompressionDriver) HealthCheck(ctx context.Context) error {
	return c.backend.HealthCheck(ctx)
}

func (c *CompressionDriver) Name() string {
	return fmt.Sprintf("compressed-%s", c.algorithm)
}
