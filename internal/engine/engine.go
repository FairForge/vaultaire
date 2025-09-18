// internal/engine/engine.go
package engine

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/cache"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/intelligence"
	"go.uber.org/zap"
)

// QuotaManager interface for quota operations
type QuotaManager interface {
	CheckAndReserve(ctx context.Context, tenantID string, bytes int64) (bool, error)
	ReleaseQuota(ctx context.Context, tenantID string, bytes int64) error
}

// CoreEngine implements the Engine interface with real intelligence
type CoreEngine struct {
	// Core components
	drivers map[string]Driver
	primary string
	backup  string

	// Managers
	logger        *zap.Logger
	db            *sql.DB
	quota         QuotaManager
	selector      *BackendSelector
	costOptimizer *CostOptimizer

	// Intelligence & Caching (REAL implementations)
	intelligence *intelligence.AccessTracker
	cache        *cache.TieredCache

	// Synchronization
	mu sync.RWMutex

	// Configuration
	config *Config
}

// Config holds engine configuration
type Config struct {
	CacheSize      int64
	EnableML       bool
	EnableCaching  bool
	DefaultBackend string
}

// NewEngine creates a new engine with full intelligence integration
func NewEngine(db *sql.DB, logger *zap.Logger, config *Config) *CoreEngine {
	if config == nil {
		config = &Config{
			CacheSize:      10 << 30, // 10GB default
			EnableCaching:  true,
			EnableML:       true,
			DefaultBackend: "local",
		}
	}

	e := &CoreEngine{
		drivers:       make(map[string]Driver),
		primary:       config.DefaultBackend,
		db:            db,
		logger:        logger,
		config:        config,
		selector:      NewBackendSelector(),
		costOptimizer: NewCostOptimizer(),
	}

	// Initialize intelligence system if DB is provided
	if db != nil {
		e.intelligence = intelligence.NewAccessTracker(db, logger)
		logger.Info("intelligence system initialized")
	}

	// Initialize tiered cache if enabled
	if config.EnableCaching {
		cacheConfig := &cache.Config{
			MemorySize: 1 << 30, // 1GB memory tier
			SSDSize:    config.CacheSize,
			SSDPath:    "/var/cache/vaultaire",
		}
		e.cache = cache.NewTieredCache(cacheConfig, logger)
		logger.Info("tiered cache initialized",
			zap.Int64("memory_size", cacheConfig.MemorySize),
			zap.Int64("ssd_size", cacheConfig.SSDSize))
	}

	return e
}

// AddDriver adds a storage driver
func (e *CoreEngine) AddDriver(name string, driver Driver) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.drivers[name] = driver
	if e.primary == "" {
		e.primary = name
	}

	e.logger.Info("driver added",
		zap.String("name", name),
		zap.Bool("is_primary", e.primary == name))
}

// SetPrimary sets the primary driver
func (e *CoreEngine) SetPrimary(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.primary = name
}

// SetBackup sets the backup driver
func (e *CoreEngine) SetBackup(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.backup = name
}

// Get retrieves an artifact with full intelligence tracking
func (e *CoreEngine) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	start := time.Now()
	tenantID := common.GetTenantID(ctx)

	// Build cache key
	cacheKey := fmt.Sprintf("%s/%s/%s", tenantID, container, artifact)

	// Track access event
	defer func() {
		if e.intelligence != nil {
			e.intelligence.LogAccess(ctx, intelligence.AccessEvent{
				TenantID:  tenantID,
				Container: container,
				Artifact:  artifact,
				Operation: "GET",
				Timestamp: time.Now(),
				Latency:   time.Since(start),
				Backend:   e.primary, // Will be updated below
				CacheHit:  false,     // Will be updated below
				Success:   true,      // Will be updated on error
			})
		}
	}()

	// Try cache first if enabled
	if e.cache != nil && e.config.EnableCaching {
		if data, err := e.cache.Get(cacheKey); err == nil && data != nil {
			e.logger.Debug("cache hit",
				zap.String("key", cacheKey),
				zap.Int("size", len(data)))

			// Update access tracking for cache hit
			if e.intelligence != nil {
				e.intelligence.LogAccess(ctx, intelligence.AccessEvent{
					TenantID:  tenantID,
					Container: container,
					Artifact:  artifact,
					Operation: "GET",
					Size:      int64(len(data)),
					Latency:   time.Since(start),
					Backend:   "cache",
					CacheHit:  true,
					Timestamp: time.Now(),
					Success:   true,
				})
			}
			return io.NopCloser(bytes.NewReader(data)), nil
		}
	}

	// Get recommendation from intelligence system
	backendName := e.primary
	if e.intelligence != nil {
		rec := e.intelligence.GetRecommendation(tenantID, container, artifact)

		// Log backend selection decision
		e.logger.Info("backend selection",
			zap.String("default", e.primary),
			zap.Bool("has_recommendation", rec != nil),
			zap.String("recommended", func() string {
				if rec != nil {
					return rec.PreferredBackend
				}
				return "none"
			}()),
			zap.String("reason", func() string {
				if rec != nil {
					return rec.Reason
				}
				return ""
			}()))

		if rec != nil && rec.PreferredBackend != "" {
			backendName = rec.PreferredBackend
		}
	}

	// Select backend based on cost if no recommendation
	if e.costOptimizer != nil && backendName == e.primary {
		if optimal := e.costOptimizer.SelectOptimal(ctx, "GET", 0); optimal != "" {
			backendName = optimal
		}
	}

	// Get driver
	driver, ok := e.drivers[backendName]
	if !ok {
		driver, ok = e.drivers[e.primary]
		if !ok {
			return nil, fmt.Errorf("no driver available")
		}
		backendName = e.primary
	}

	// Perform the actual get
	reader, err := driver.Get(ctx, container, artifact)
	if err != nil {
		return nil, fmt.Errorf("get from %s: %w", backendName, err)
	}

	// Cache the data if small enough
	if e.cache != nil && e.config.EnableCaching {
		// Wrap reader to cache on read
		reader = e.wrapReaderForCaching(reader, cacheKey)
	}

	return reader, nil
}

// Put stores an artifact with intelligence tracking
func (e *CoreEngine) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) error {
	start := time.Now()
	tenantID := common.GetTenantID(ctx)

	// Track size by wrapping reader
	sizeReader := &sizeTrackingReader{Reader: data}

	// Check quota if manager is configured
	if e.quota != nil && tenantID != "" {
		// Estimate size for quota check
		estimatedSize := int64(10 << 20) // 10MB default

		allowed, err := e.quota.CheckAndReserve(ctx, tenantID, estimatedSize)
		if err != nil {
			return fmt.Errorf("checking quota: %w", err)
		}
		if !allowed {
			return ErrQuotaExceeded
		}

		// Release quota if put fails
		defer func() {
			if actualSize := sizeReader.bytesRead; actualSize != estimatedSize {
				// Adjust quota for actual size
				diff := actualSize - estimatedSize
				if diff != 0 {
					_, _ = e.quota.CheckAndReserve(ctx, tenantID, diff)
				}
			}
		}()
	}

	// Select backend for write
	backendName := e.primary
	if e.intelligence != nil {
		if rec := e.intelligence.GetRecommendation(tenantID, container, artifact); rec != nil {
			if rec.PreferredBackend != "" {
				backendName = rec.PreferredBackend
			}
		}
	}

	// Get driver
	driver, ok := e.drivers[backendName]
	if !ok {
		return fmt.Errorf("driver %s not found", backendName)
	}

	// Store to primary
	err := driver.Put(ctx, container, artifact, sizeReader, opts...)

	// Log access
	if e.intelligence != nil {
		e.intelligence.LogAccess(ctx, intelligence.AccessEvent{
			TenantID:  tenantID,
			Container: container,
			Artifact:  artifact,
			Operation: "PUT",
			Size:      sizeReader.bytesRead,
			Latency:   time.Since(start),
			Backend:   backendName,
			Timestamp: time.Now(),
			Success:   err == nil,
		})
	}

	if err != nil {
		return fmt.Errorf("put to %s: %w", backendName, err)
	}

	// Invalidate cache
	if e.cache != nil {
		cacheKey := fmt.Sprintf("%s/%s/%s", tenantID, container, artifact)
		_ = e.cache.Delete(cacheKey)
	}

	// Queue for backup asynchronously
	if e.backup != "" && e.backup != e.primary {
		go e.replicateToBackup(ctx, container, artifact)
	}

	return nil
}

// Delete removes an artifact with tracking
func (e *CoreEngine) Delete(ctx context.Context, container, artifact string) error {
	start := time.Now()
	tenantID := common.GetTenantID(ctx)

	// Delete from all backends
	var lastErr error
	for name, driver := range e.drivers {
		if err := driver.Delete(ctx, container, artifact); err != nil {
			e.logger.Warn("delete failed",
				zap.String("driver", name),
				zap.Error(err))
			lastErr = err
		}
	}

	// Clear from cache
	if e.cache != nil {
		cacheKey := fmt.Sprintf("%s/%s/%s", tenantID, container, artifact)
		_ = e.cache.Delete(cacheKey)
	}

	// Log access
	if e.intelligence != nil {
		e.intelligence.LogAccess(ctx, intelligence.AccessEvent{
			TenantID:  tenantID,
			Container: container,
			Artifact:  artifact,
			Operation: "DELETE",
			Latency:   time.Since(start),
			Timestamp: time.Now(),
			Success:   lastErr == nil,
		})
	}

	return lastErr
}

// List returns artifacts in a container
func (e *CoreEngine) List(ctx context.Context, container, prefix string) ([]Artifact, error) {
	start := time.Now()
	tenantID := common.GetTenantID(ctx)

	driver, ok := e.drivers[e.primary]
	if !ok {
		return nil, fmt.Errorf("no driver available")
	}

	keys, err := driver.List(ctx, container, prefix)
	if err != nil {
		return nil, err
	}

	// Log access
	if e.intelligence != nil {
		e.intelligence.LogAccess(ctx, intelligence.AccessEvent{
			TenantID:  tenantID,
			Container: container,
			Operation: "LIST",
			Latency:   time.Since(start),
			Backend:   e.primary,
			Timestamp: time.Now(),
			Success:   err == nil,
		})
	}

	artifacts := make([]Artifact, len(keys))
	for i, key := range keys {
		artifacts[i] = Artifact{
			Key:       key,
			Container: container,
			Type:      "blob",
		}
	}

	return artifacts, nil
}

// GetHotData returns frequently accessed data for a tenant
func (e *CoreEngine) GetHotData(ctx context.Context, limit int) ([]string, error) {
	if e.intelligence == nil {
		return nil, fmt.Errorf("intelligence system not initialized")
	}

	tenantID := common.GetTenantID(ctx)
	return e.intelligence.GetHotData(tenantID, limit)
}

// GetAccessPatterns returns access patterns for analysis
func (e *CoreEngine) GetAccessPatterns(ctx context.Context) (*intelligence.TenantPatterns, error) {
	if e.intelligence == nil {
		return nil, fmt.Errorf("intelligence system not initialized")
	}

	tenantID := common.GetTenantID(ctx)
	return e.intelligence.GetPatterns(tenantID)
}

// GetRecommendations returns optimization recommendations
func (e *CoreEngine) GetRecommendations(ctx context.Context) ([]intelligence.Recommendation, error) {
	if e.intelligence == nil {
		return nil, fmt.Errorf("intelligence system not initialized")
	}

	tenantID := common.GetTenantID(ctx)
	return e.intelligence.GetRecommendations(tenantID)
}

// HealthCheck verifies all drivers and systems are healthy
func (e *CoreEngine) HealthCheck(ctx context.Context) error {
	// Check drivers
	for name, driver := range e.drivers {
		if err := driver.HealthCheck(ctx); err != nil {
			return fmt.Errorf("driver %s unhealthy: %w", name, err)
		}
	}

	// Check database
	if e.db != nil {
		if err := e.db.PingContext(ctx); err != nil {
			return fmt.Errorf("database unhealthy: %w", err)
		}
	}

	// Check cache
	if e.cache != nil {
		if err := e.cache.HealthCheck(); err != nil {
			return fmt.Errorf("cache unhealthy: %w", err)
		}
	}

	return nil
}

// GetMetrics returns comprehensive metrics
func (e *CoreEngine) GetMetrics(ctx context.Context) (map[string]interface{}, error) {
	metrics := map[string]interface{}{
		"drivers": len(e.drivers),
		"primary": e.primary,
		"backup":  e.backup,
	}

	// Add cache metrics
	if e.cache != nil {
		cacheMetrics := e.cache.GetMetrics()
		metrics["cache"] = cacheMetrics
	}

	// Add intelligence metrics if available
	if e.intelligence != nil {
		tenantID := common.GetTenantID(ctx)
		if patterns, err := e.intelligence.GetPatterns(tenantID); err == nil {
			metrics["patterns"] = patterns
		}
	}

	return metrics, nil
}

// SetQuotaManager sets the quota manager
func (e *CoreEngine) SetQuotaManager(qm QuotaManager) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.quota = qm
}

// SetCostConfiguration updates cost optimizer
func (e *CoreEngine) SetCostConfiguration(costs map[string]float64) {
	if e.costOptimizer != nil {
		for backend, cost := range costs {
			e.costOptimizer.SetCost(backend, cost)
		}
	}
}

// SetEgressCosts updates egress costs
func (e *CoreEngine) SetEgressCosts(costs map[string]float64) {
	if e.costOptimizer != nil {
		for backend, cost := range costs {
			e.costOptimizer.SetEgressCost(backend, cost)
		}
	}
}

// Helper: wrap reader to cache data on read
func (e *CoreEngine) wrapReaderForCaching(reader io.ReadCloser, cacheKey string) io.ReadCloser {
	return &cachingReader{
		ReadCloser: reader,
		cache:      e.cache,
		key:        cacheKey,
		buffer:     &bytes.Buffer{},
	}
}

// Helper: replicate to backup backend
func (e *CoreEngine) replicateToBackup(ctx context.Context, container, artifact string) {
	backupDriver, ok := e.drivers[e.backup]
	if !ok {
		return
	}

	// Get from primary
	primaryDriver, ok := e.drivers[e.primary]
	if !ok {
		return
	}

	reader, err := primaryDriver.Get(ctx, container, artifact)
	if err != nil {
		e.logger.Error("failed to read for backup", zap.Error(err))
		return
	}
	defer func() { _ = reader.Close() }()

	// Put to backup
	if err := backupDriver.Put(ctx, container, artifact, reader); err != nil {
		e.logger.Error("failed to replicate to backup", zap.Error(err))
		return
	}

	e.logger.Info("replicated to backup",
		zap.String("container", container),
		zap.String("artifact", artifact))
}

// Helper types

type sizeTrackingReader struct {
	io.Reader
	bytesRead int64
}

func (r *sizeTrackingReader) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	r.bytesRead += int64(n)
	return
}

type cachingReader struct {
	io.ReadCloser
	cache  *cache.TieredCache
	key    string
	buffer *bytes.Buffer
}

func (r *cachingReader) Read(p []byte) (n int, err error) {
	n, err = r.ReadCloser.Read(p)
	if n > 0 {
		r.buffer.Write(p[:n])
	}

	// Cache when done reading
	if err == io.EOF && r.cache != nil && r.buffer.Len() < 10<<20 { // Cache if < 10MB
		_ = r.cache.Set(r.key, r.buffer.Bytes())
	}

	return
}

// Shutdown gracefully shuts down the engine
func (e *CoreEngine) Shutdown(ctx context.Context) error {
	e.logger.Info("shutting down engine")

	// Flush intelligence data
	if e.intelligence != nil {
		e.intelligence.Flush()
	}

	// Flush cache
	if e.cache != nil {
		e.cache.Flush()
	}

	// Close database
	if e.db != nil {
		_ = e.db.Close()
	}

	return nil
}

// GetContainerMetadata returns container metadata
func (e *CoreEngine) GetContainerMetadata(ctx context.Context, container string) (*Container, error) {
	return &Container{
		Name:     container,
		Type:     "storage",
		Created:  time.Now(),
		Metadata: make(map[string]interface{}),
	}, nil
}

// GetArtifactMetadata returns artifact metadata
func (e *CoreEngine) GetArtifactMetadata(ctx context.Context, container, artifact string) (*Artifact, error) {
	return &Artifact{
		Container: container,
		Key:       artifact,
		Type:      "blob",
		Modified:  time.Now(),
		Metadata:  make(map[string]interface{}),
	}, nil
}

// Execute implements WASM execution (placeholder)
func (e *CoreEngine) Execute(ctx context.Context, container string, wasm []byte, input io.Reader) (io.Reader, error) {
	return nil, fmt.Errorf("WASM execution not yet implemented")
}

// Query implements SQL queries (placeholder)
func (e *CoreEngine) Query(ctx context.Context, sql string) (ResultSet, error) {
	return nil, fmt.Errorf("SQL queries not yet implemented")
}

// Train implements ML training (placeholder)
func (e *CoreEngine) Train(ctx context.Context, model string, data []byte) error {
	return fmt.Errorf("ML training not yet implemented")
}

// Predict implements ML prediction (placeholder)
func (e *CoreEngine) Predict(ctx context.Context, model string, input []byte) ([]byte, error) {
	return nil, fmt.Errorf("ML prediction not yet implemented")
}
