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
	drivers map[string]Driver
	primary string
	backup  string

	logger        *zap.Logger
	db            *sql.DB
	quota         QuotaManager
	selector      *BackendSelector
	costOptimizer *CostOptimizer

	intelligence *intelligence.AccessTracker
	cache        *cache.TieredCache

	// objectBackends records which backend each object was written to.
	// Key: "container/artifact"  Value: backend name string
	// This guarantees Get always reads from the same backend Put used,
	// even when the intelligence or cost-optimizer would suggest a
	// different one. In-memory only; objects written before a restart
	// fall back to e.primary (safe — they were written there too).
	objectBackends sync.Map

	mu     sync.RWMutex
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
			CacheSize:      10 << 30,
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

	if db != nil {
		e.intelligence = intelligence.NewAccessTracker(db, logger)
		logger.Info("intelligence system initialized")
	}

	if config.EnableCaching {
		cacheConfig := &cache.Config{
			MemorySize: 1 << 30,
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

// objectKey returns the sync.Map key for a container+artifact pair.
func objectKey(container, artifact string) string {
	return container + "/" + artifact
}

// Get retrieves an artifact.
//
// Backend selection consults objectBackends first — the map written by Put
// that records exactly which backend each object landed on. This is the
// source of truth. Intelligence and cost-optimizer recommendations are
// logged for future use but never override this lookup.
//
// Fall-through to e.primary only occurs for objects written before this
// engine version was deployed (map is empty on restart).
func (e *CoreEngine) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	start := time.Now()
	tenantID := common.GetTenantID(ctx)
	cacheKey := fmt.Sprintf("%s/%s/%s", tenantID, container, artifact)

	// L1: in-memory tiered cache
	if e.cache != nil && e.config.EnableCaching {
		if data, err := e.cache.Get(cacheKey); err == nil && data != nil {
			e.logger.Debug("cache hit", zap.String("key", cacheKey))
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

	// L2: look up which backend this object was written to.
	backendName := e.primary
	if v, ok := e.objectBackends.Load(objectKey(container, artifact)); ok {
		if name, ok := v.(string); ok && name != "" {
			backendName = name
		}
	}

	// Log what intelligence would have recommended (informational only).
	if e.intelligence != nil {
		if rec := e.intelligence.GetRecommendation(tenantID, container, artifact); rec != nil {
			e.logger.Debug("intelligence recommendation (not used for routing)",
				zap.String("actual_backend", backendName),
				zap.String("recommended", rec.PreferredBackend),
				zap.String("reason", rec.Reason))
		}
	}

	driver, ok := e.drivers[backendName]
	if !ok {
		// Backend from the map no longer exists — fall back to primary.
		e.logger.Warn("recorded backend not found, falling back to primary",
			zap.String("recorded", backendName),
			zap.String("primary", e.primary))
		driver, ok = e.drivers[e.primary]
		if !ok {
			return nil, fmt.Errorf("no driver available")
		}
		backendName = e.primary
	}

	reader, err := driver.Get(ctx, container, artifact)
	if e.intelligence != nil {
		e.intelligence.LogAccess(ctx, intelligence.AccessEvent{
			TenantID:  tenantID,
			Container: container,
			Artifact:  artifact,
			Operation: "GET",
			Latency:   time.Since(start),
			Backend:   backendName,
			CacheHit:  false,
			Timestamp: time.Now(),
			Success:   err == nil,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("get from %s: %w", backendName, err)
	}

	if e.cache != nil && e.config.EnableCaching {
		reader = e.wrapReaderForCaching(reader, cacheKey)
	}

	return reader, nil
}

// Put stores an artifact and returns the name of the backend it was written to.
//
// The returned backend name must be persisted by the caller (the S3 adapter
// writes it to object_head_cache.backend_name). It is also stored in
// objectBackends so that Get can route correctly within the same process
// lifetime without a DB round-trip.
func (e *CoreEngine) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) (string, error) {
	start := time.Now()
	tenantID := common.GetTenantID(ctx)
	sizeReader := &sizeTrackingReader{Reader: data}

	if e.quota != nil && tenantID != "" {
		estimatedSize := int64(10 << 20)
		allowed, err := e.quota.CheckAndReserve(ctx, tenantID, estimatedSize)
		if err != nil {
			return "", fmt.Errorf("checking quota: %w", err)
		}
		if !allowed {
			return "", ErrQuotaExceeded
		}
		defer func() {
			if actual := sizeReader.bytesRead; actual != estimatedSize {
				diff := actual - estimatedSize
				if diff != 0 {
					_, _ = e.quota.CheckAndReserve(ctx, tenantID, diff)
				}
			}
		}()
	}

	// Select the backend for this write.
	// Intelligence recommendations are accepted here — on PUT we want smart
	// routing. The key guarantee is that we record what we chose so Get
	// can honour it exactly.
	backendName := e.primary
	if e.intelligence != nil {
		if rec := e.intelligence.GetRecommendation(tenantID, container, artifact); rec != nil {
			if rec.PreferredBackend != "" {
				if _, exists := e.drivers[rec.PreferredBackend]; exists {
					backendName = rec.PreferredBackend
				} else {
					e.logger.Warn("intelligence recommended non-existent backend, using primary",
						zap.String("recommended", rec.PreferredBackend),
						zap.String("primary", e.primary))
				}
			}
		}
	}

	driver, ok := e.drivers[backendName]
	if !ok {
		return "", fmt.Errorf("driver %s not found", backendName)
	}

	err := driver.Put(ctx, container, artifact, sizeReader, opts...)

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
		return "", fmt.Errorf("put to %s: %w", backendName, err)
	}

	// Record backend for this object so Get always routes correctly.
	e.objectBackends.Store(objectKey(container, artifact), backendName)

	if e.cache != nil {
		cacheKey := fmt.Sprintf("%s/%s/%s", tenantID, container, artifact)
		_ = e.cache.Delete(cacheKey)
	}

	if e.backup != "" && e.backup != e.primary {
		go e.replicateToBackup(ctx, container, artifact)
	}

	return backendName, nil
}

// Delete removes an artifact from all backends
func (e *CoreEngine) Delete(ctx context.Context, container, artifact string) error {
	start := time.Now()
	tenantID := common.GetTenantID(ctx)

	var lastErr error
	for name, driver := range e.drivers {
		if err := driver.Delete(ctx, container, artifact); err != nil {
			e.logger.Warn("delete failed",
				zap.String("driver", name),
				zap.Error(err))
			lastErr = err
		}
	}

	// Remove routing record — object no longer exists.
	e.objectBackends.Delete(objectKey(container, artifact))

	if e.cache != nil {
		cacheKey := fmt.Sprintf("%s/%s/%s", tenantID, container, artifact)
		_ = e.cache.Delete(cacheKey)
	}

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

	if e.intelligence != nil {
		e.intelligence.LogAccess(ctx, intelligence.AccessEvent{
			TenantID:  tenantID,
			Container: container,
			Operation: "LIST",
			Latency:   time.Since(start),
			Backend:   e.primary,
			Timestamp: time.Now(),
			Success:   true,
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
	for name, driver := range e.drivers {
		if err := driver.HealthCheck(ctx); err != nil {
			return fmt.Errorf("driver %s unhealthy: %w", name, err)
		}
	}
	if e.db != nil {
		if err := e.db.PingContext(ctx); err != nil {
			return fmt.Errorf("database unhealthy: %w", err)
		}
	}
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
	if e.cache != nil {
		metrics["cache"] = e.cache.GetMetrics()
	}
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

func (e *CoreEngine) wrapReaderForCaching(reader io.ReadCloser, cacheKey string) io.ReadCloser {
	return &cachingReader{
		ReadCloser: reader,
		cache:      e.cache,
		key:        cacheKey,
		buffer:     &bytes.Buffer{},
	}
}

func (e *CoreEngine) replicateToBackup(ctx context.Context, container, artifact string) {
	backupDriver, ok := e.drivers[e.backup]
	if !ok {
		return
	}
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
	if err := backupDriver.Put(ctx, container, artifact, reader); err != nil {
		e.logger.Error("failed to replicate to backup", zap.Error(err))
		return
	}
	e.logger.Info("replicated to backup",
		zap.String("container", container),
		zap.String("artifact", artifact))
}

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
	if err == io.EOF && r.cache != nil && r.buffer.Len() < 10<<20 {
		_ = r.cache.Set(r.key, r.buffer.Bytes())
	}
	return
}

// Shutdown gracefully shuts down the engine
func (e *CoreEngine) Shutdown(ctx context.Context) error {
	e.logger.Info("shutting down engine")
	if e.intelligence != nil {
		e.intelligence.Flush()
	}
	if e.cache != nil {
		e.cache.Flush()
	}
	if e.db != nil {
		_ = e.db.Close()
	}
	return nil
}

func (e *CoreEngine) GetContainerMetadata(ctx context.Context, container string) (*Container, error) {
	return &Container{
		Name:     container,
		Type:     "storage",
		Created:  time.Now(),
		Metadata: make(map[string]interface{}),
	}, nil
}

func (e *CoreEngine) GetArtifactMetadata(ctx context.Context, container, artifact string) (*Artifact, error) {
	return &Artifact{
		Container: container,
		Key:       artifact,
		Type:      "blob",
		Modified:  time.Now(),
		Metadata:  make(map[string]interface{}),
	}, nil
}

func (e *CoreEngine) Execute(ctx context.Context, container string, wasm []byte, input io.Reader) (io.Reader, error) {
	return nil, fmt.Errorf("WASM execution not yet implemented")
}

func (e *CoreEngine) Query(ctx context.Context, sql string) (ResultSet, error) {
	return nil, fmt.Errorf("SQL queries not yet implemented")
}

func (e *CoreEngine) Train(ctx context.Context, model string, data []byte) error {
	return fmt.Errorf("ML training not yet implemented")
}

func (e *CoreEngine) Predict(ctx context.Context, model string, input []byte) ([]byte, error) {
	return nil, fmt.Errorf("ML prediction not yet implemented")
}
