// internal/intelligence/access_tracker.go
package intelligence

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// AccessTracker handles all pattern learning
type AccessTracker struct {
	db       *sql.DB
	logger   *zap.Logger
	buffer   chan AccessEvent
	patterns *PatternAnalyzer
	anomaly  *AnomalyDetector
	ml       *MLPipeline

	hotCache map[string]*HotData
}

// NewAccessTracker creates the intelligence system
func NewAccessTracker(db *sql.DB, logger *zap.Logger) *AccessTracker {
	at := &AccessTracker{
		db:       db,
		logger:   logger,
		buffer:   make(chan AccessEvent, 10000),
		patterns: NewPatternAnalyzer(db, logger),
		anomaly:  NewAnomalyDetector(db, logger),
		ml:       NewMLPipeline(db, logger),
		hotCache: make(map[string]*HotData),
	}

	go at.processEvents()
	return at
}

// LogAccess logs an access event
func (at *AccessTracker) LogAccess(ctx context.Context, event AccessEvent) {
	select {
	case at.buffer <- event:
	default:
		at.logger.Warn("access buffer full")
	}
}

// GetHotData returns hot data for a tenant
func (at *AccessTracker) GetHotData(tenantID string, limit int) ([]string, error) {
	query := `
		SELECT container || '/' || artifact_key
		FROM access_patterns
		WHERE tenant_id = $1 AND temperature = 'hot'
		ORDER BY access_frequency DESC
		LIMIT $2
	`

	rows, err := at.db.Query(query, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}

	return keys, nil
}

// GetPatterns returns patterns for a tenant
func (at *AccessTracker) GetPatterns(tenantID string) (*TenantPatterns, error) {
	return at.patterns.AnalyzePatterns(tenantID)
}

// GetRecommendations returns optimization recommendations
func (at *AccessTracker) GetRecommendations(tenantID string) ([]Recommendation, error) {
	optimizer := NewOptimizer(at.db, at.patterns)
	return optimizer.GetRecommendations(tenantID)
}

// GetRecommendation for a specific artifact
func (at *AccessTracker) GetRecommendation(tenantID, container, artifact string) *Recommendation {
	// Simple recommendation based on access patterns
	query := `
		SELECT access_count, temperature
		FROM access_patterns
		WHERE tenant_id = $1 AND container = $2 AND artifact_key = $3
	`

	var count int
	var temp string
	err := at.db.QueryRow(query, tenantID, container, artifact).Scan(&count, &temp)
	if err != nil {
		return nil
	}

	if temp == "hot" && count > 10 {
		return &Recommendation{
			Type:             "use_cache",
			PreferredBackend: "cache",
			Reason:           fmt.Sprintf("High access count (%d)", count),
		}
	}

	return nil
}

// Flush forces processing of buffered events
func (at *AccessTracker) Flush() {
	// Process any remaining events
	for len(at.buffer) > 0 {
		select {
		case event := <-at.buffer:
			at.processSingleEvent(event)
		default:
			return
		}
	}
}

func (at *AccessTracker) processEvents() {
	ticker := time.NewTicker(5 * time.Second)
	batch := make([]AccessEvent, 0, 1000)

	for {
		select {
		case event := <-at.buffer:
			batch = append(batch, event)
			if len(batch) >= 1000 {
				at.flushBatch(batch)
				batch = batch[:0]
			}

		case <-ticker.C:
			if len(batch) > 0 {
				at.flushBatch(batch)
				batch = batch[:0]
			}
		}
	}
}

func (at *AccessTracker) processSingleEvent(event AccessEvent) {
	at.flushBatch([]AccessEvent{event})
}

func (at *AccessTracker) flushBatch(events []AccessEvent) {
	tx, err := at.db.Begin()
	if err != nil {
		at.logger.Error("failed to begin transaction", zap.Error(err))
		return
	}
	defer func() { _ = tx.Rollback() }()

	query := `
		INSERT INTO access_patterns (
			tenant_id, container, artifact_key, operation,
			size_bytes, latency_ms, backend_used, cache_hit,
			access_time, success
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (tenant_id, container, artifact_key)
		DO UPDATE SET
			last_seen = EXCLUDED.access_time,
			access_count = access_patterns.access_count + 1,
			total_bytes = access_patterns.total_bytes + EXCLUDED.size_bytes
	`

	for _, e := range events {
		_, err := tx.Exec(query,
			e.TenantID, e.Container, e.Artifact, e.Operation,
			e.Size, e.Latency.Milliseconds(), e.Backend, e.CacheHit,
			e.Timestamp, e.Success,
		)
		if err != nil {
			at.logger.Error("failed to log access", zap.Error(err))
		}
	}

	_ = tx.Commit()
}

// Pattern Analyzer
type PatternAnalyzer struct {
	db     *sql.DB
	logger *zap.Logger
}

func NewPatternAnalyzer(db *sql.DB, logger *zap.Logger) *PatternAnalyzer {
	return &PatternAnalyzer{db: db, logger: logger}
}

func (pa *PatternAnalyzer) AnalyzePatterns(tenantID string) (*TenantPatterns, error) {
	return &TenantPatterns{
		TenantID:     tenantID,
		Temporal:     TemporalPatterns{PeakHours: []int{9, 10, 11}, PeakDays: []int{1, 2, 3, 4, 5}},
		Spatial:      SpatialPatterns{HotDirectories: []string{"/images", "/docs"}},
		UserBehavior: UserBehavior{AvgFileSize: 1024000, AccessVelocity: 10.5, BurstThreshold: 100},
	}, nil
}
