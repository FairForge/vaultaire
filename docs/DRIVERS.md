# Driver Development Guide

## Overview

Drivers are the bridge between Vaultaire's engine and storage backends. This guide explains how to create custom drivers.

## Driver Interface

Every driver must implement this interface:

```go
type Driver interface {
    // Identification
    Name() string

    // Core operations
    Get(ctx context.Context, container, artifact string) (io.ReadCloser, error)
    Put(ctx context.Context, container, artifact string, data io.Reader) error
    Delete(ctx context.Context, container, artifact string) error
    List(ctx context.Context, container string) ([]string, error)

    // Health and metrics
    HealthCheck(ctx context.Context) error
    GetMetrics() map[string]interface{}
}
Creating a Custom Driver
Step 1: Define Your Driver Structure
gopackage drivers

import (
    "context"
    "io"
    "github.com/fairforge/vaultaire/internal/engine"
)

type MyCustomDriver struct {
    config   Config
    client   *MyClient
    logger   *zap.Logger
    metrics  *Metrics
}

type Config struct {
    Endpoint string
    APIKey   string
    Timeout  time.Duration
}
Step 2: Implement the Interface
gofunc NewMyCustomDriver(config Config, logger *zap.Logger) engine.Driver {
    return &MyCustomDriver{
        config: config,
        logger: logger,
        client: newClient(config),
    }
}

func (d *MyCustomDriver) Name() string {
    return "mycustom"
}

func (d *MyCustomDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
    key := d.buildKey(container, artifact)

    // Add metrics
    start := time.Now()
    defer func() {
        d.metrics.RecordLatency("get", time.Since(start))
    }()

    // Implement your get logic
    reader, err := d.client.Download(ctx, key)
    if err != nil {
        return nil, fmt.Errorf("failed to get %s/%s: %w", container, artifact, err)
    }

    return reader, nil
}

func (d *MyCustomDriver) Put(ctx context.Context, container, artifact string, data io.Reader) error {
    key := d.buildKey(container, artifact)

    // Add retry logic
    return retry.Do(func() error {
        return d.client.Upload(ctx, key, data)
    }, retry.Attempts(3))
}

func (d *MyCustomDriver) Delete(ctx context.Context, container, artifact string) error {
    key := d.buildKey(container, artifact)
    return d.client.Delete(ctx, key)
}

func (d *MyCustomDriver) List(ctx context.Context, container string) ([]string, error) {
    prefix := container + "/"
    return d.client.List(ctx, prefix)
}

func (d *MyCustomDriver) HealthCheck(ctx context.Context) error {
    ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()

    return d.client.Ping(ctx)
}

func (d *MyCustomDriver) buildKey(container, artifact string) string {
    return fmt.Sprintf("%s/%s", container, artifact)
}
Step 3: Add Error Handling
gofunc (d *MyCustomDriver) handleError(err error) error {
    if err == nil {
        return nil
    }

    // Map backend-specific errors to standard errors
    switch {
    case isNotFound(err):
        return engine.ErrNotFound
    case isPermissionDenied(err):
        return engine.ErrAccessDenied
    case isQuotaExceeded(err):
        return engine.ErrQuotaExceeded
    default:
        return fmt.Errorf("backend error: %w", err)
    }
}
Step 4: Add Metrics and Logging
gofunc (d *MyCustomDriver) recordMetric(operation string, success bool, latency time.Duration) {
    d.metrics.Operations.WithLabelValues(operation, fmt.Sprintf("%t", success)).Inc()
    d.metrics.Latency.WithLabelValues(operation).Observe(latency.Seconds())

    d.logger.Debug("operation completed",
        zap.String("operation", operation),
        zap.Bool("success", success),
        zap.Duration("latency", latency),
    )
}
Testing Your Driver
Unit Tests
gofunc TestMyCustomDriver_Get(t *testing.T) {
    driver := NewMyCustomDriver(testConfig, logger)

    // Test successful get
    reader, err := driver.Get(context.Background(), "test-container", "test-artifact")
    assert.NoError(t, err)
    assert.NotNil(t, reader)

    // Test not found
    _, err = driver.Get(context.Background(), "missing", "artifact")
    assert.ErrorIs(t, err, engine.ErrNotFound)
}
Integration Tests
gofunc TestMyCustomDriver_Integration(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test")
    }

    driver := NewMyCustomDriver(realConfig, logger)

    // Full cycle test
    data := bytes.NewReader([]byte("test data"))
    err := driver.Put(ctx, "test", "file.txt", data)
    require.NoError(t, err)

    reader, err := driver.Get(ctx, "test", "file.txt")
    require.NoError(t, err)

    content, err := io.ReadAll(reader)
    require.NoError(t, err)
    assert.Equal(t, "test data", string(content))

    err = driver.Delete(ctx, "test", "file.txt")
    require.NoError(t, err)
}
Best Practices
1. Connection Pooling
gotype MyCustomDriver struct {
    pool *ConnectionPool
}

func (d *MyCustomDriver) getConnection() (*Connection, error) {
    return d.pool.Get()
}
2. Retry Logic
gofunc (d *MyCustomDriver) Put(ctx context.Context, container, artifact string, data io.Reader) error {
    return retry.Do(
        func() error {
            return d.putInternal(ctx, container, artifact, data)
        },
        retry.Attempts(3),
        retry.Delay(time.Second),
        retry.OnRetry(func(n uint, err error) {
            d.logger.Warn("retry attempt",
                zap.Uint("attempt", n),
                zap.Error(err),
            )
        }),
    )
}
3. Context Handling
gofunc (d *MyCustomDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
    // Check context before expensive operations
    select {
    case <-ctx.Done():
        return nil, ctx.Err()
    default:
    }

    // Pass context through
    return d.client.GetWithContext(ctx, key)
}
4. Resource Cleanup
gotype readerWithCleanup struct {
    io.ReadCloser
    cleanup func()
}

func (r *readerWithCleanup) Close() error {
    err := r.ReadCloser.Close()
    if r.cleanup != nil {
        r.cleanup()
    }
    return err
}
Registering Your Driver
In the Engine
go// internal/engine/registry.go
func RegisterDrivers(engine *CoreEngine) {
    // Built-in drivers
    engine.RegisterDriver("s3", drivers.NewS3Driver)
    engine.RegisterDriver("local", drivers.NewLocalDriver)

    // Your custom driver
    engine.RegisterDriver("mycustom", drivers.NewMyCustomDriver)
}
In Configuration
yamlbackends:
  - name: custom-backend
    driver: mycustom
    config:
      endpoint: https://api.example.com
      api_key: ${CUSTOM_API_KEY}
Performance Optimization
1. Parallel Operations
gofunc (d *MyCustomDriver) List(ctx context.Context, container string) ([]string, error) {
    // Use goroutines for parallel listing
    results := make(chan []string)
    errors := make(chan error)

    go d.listPartition(ctx, container, 0, results, errors)
    go d.listPartition(ctx, container, 1, results, errors)

    // Collect results...
}
2. Caching
gotype MyCustomDriver struct {
    cache *lru.Cache
}

func (d *MyCustomDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
    key := d.buildKey(container, artifact)

    // Check cache first
    if cached, ok := d.cache.Get(key); ok {
        return cached.(io.ReadCloser), nil
    }

    // Fetch and cache
    reader, err := d.client.Get(ctx, key)
    if err == nil {
        d.cache.Add(key, reader)
    }
    return reader, err
}
Example Drivers
Check our existing drivers for reference:

internal/drivers/local.go - Simple filesystem driver
internal/drivers/s3.go - AWS S3 driver
internal/drivers/lyve.go - Seagate Lyve Cloud driver

Contributing
If you've built a useful driver, please consider contributing it back:

Fork the repository
Add your driver to internal/drivers/
Add tests
Submit a pull request

See CONTRIBUTING.md for details.
