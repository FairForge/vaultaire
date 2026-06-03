package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

type TieringEngine struct {
	db             *sql.DB
	drivers        map[string]Driver
	locations      *LocationStore
	objectBackends *sync.Map
	logger         *zap.Logger
	interval       time.Duration
	stop           chan struct{}
}

func NewTieringEngine(db *sql.DB, drivers map[string]Driver, locations *LocationStore, objectBackends *sync.Map, logger *zap.Logger) *TieringEngine {
	return &TieringEngine{
		db:             db,
		drivers:        drivers,
		locations:      locations,
		objectBackends: objectBackends,
		logger:         logger,
		interval:       1 * time.Hour,
		stop:           make(chan struct{}),
	}
}

func (t *TieringEngine) Start(ctx context.Context) {
	if t.db == nil {
		return
	}

	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t.runScan(ctx)
		case <-t.stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (t *TieringEngine) Stop() {
	close(t.stop)
}

type tieringPolicy struct {
	id            int64
	tenantID      *string
	bucket        *string
	minAgeDays    int
	targetBackend string
	targetClass   string
}

func (t *TieringEngine) runScan(ctx context.Context) {
	policies, err := t.loadPolicies(ctx)
	if err != nil {
		t.logger.Error("tiering: load policies", zap.Error(err))
		return
	}

	if len(policies) == 0 {
		policies = t.defaultPolicy()
	}

	var migrated, skipped, failed int
	for _, p := range policies {
		if _, ok := t.drivers[p.targetBackend]; !ok {
			t.logger.Debug("tiering: target backend not registered, skipping policy",
				zap.String("target", p.targetBackend))
			skipped++
			continue
		}

		candidates, err := t.findCandidates(ctx, p)
		if err != nil {
			t.logger.Error("tiering: find candidates", zap.Error(err), zap.Int64("policy_id", p.id))
			failed++
			continue
		}

		for _, c := range candidates {
			if err := t.migrateObject(ctx, c.tenantID, c.bucket, c.objectKey, c.backendName, p.targetBackend, p.targetClass, c.sizeBytes); err != nil {
				t.logger.Warn("tiering: migrate failed",
					zap.String("key", c.bucket+"/"+c.objectKey),
					zap.Error(err))
				failed++
			} else {
				migrated++
			}
		}
	}

	if migrated > 0 || failed > 0 {
		t.logger.Info("tiering scan complete",
			zap.Int("migrated", migrated),
			zap.Int("skipped", skipped),
			zap.Int("failed", failed))
	}
}

func (t *TieringEngine) loadPolicies(ctx context.Context) ([]tieringPolicy, error) {
	rows, err := t.db.QueryContext(ctx, `
		SELECT id, tenant_id, bucket, min_age_days, target_backend, target_class
		FROM tiering_policies WHERE enabled = TRUE`)
	if err != nil {
		return nil, fmt.Errorf("query tiering_policies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var policies []tieringPolicy
	for rows.Next() {
		var p tieringPolicy
		if err := rows.Scan(&p.id, &p.tenantID, &p.bucket, &p.minAgeDays, &p.targetBackend, &p.targetClass); err != nil {
			return nil, fmt.Errorf("scan tiering_policies: %w", err)
		}
		policies = append(policies, p)
	}
	return policies, rows.Err()
}

func (t *TieringEngine) defaultPolicy() []tieringPolicy {
	if _, hasGeyser := t.drivers["geyser"]; !hasGeyser {
		return nil
	}
	return []tieringPolicy{{
		id:            0,
		minAgeDays:    90,
		targetBackend: "geyser",
		targetClass:   "GLACIER",
	}}
}

type migrationCandidate struct {
	tenantID    string
	bucket      string
	objectKey   string
	backendName string
	sizeBytes   int64
}

func (t *TieringEngine) findCandidates(ctx context.Context, p tieringPolicy) ([]migrationCandidate, error) {
	query := `
		SELECT tenant_id, bucket, object_key, backend_name, size_bytes
		FROM object_locations
		WHERE last_accessed < NOW() - INTERVAL '1 day' * $1
		  AND backend_name != $2
		  AND NOT EXISTS (
		      SELECT 1 FROM buckets
		      WHERE buckets.tenant_id = object_locations.tenant_id
		        AND buckets.name = object_locations.bucket
		        AND buckets.tier_preference != 'auto'
		  )`
	args := []any{p.minAgeDays, p.targetBackend}

	targetRegion := BackendRegion(p.targetBackend)
	if targetRegion != "eu" {
		query += ` AND NOT EXISTS (
			SELECT 1 FROM buckets
			WHERE buckets.tenant_id = object_locations.tenant_id
			  AND buckets.name = object_locations.bucket
			  AND buckets.data_residency = 'eu'
		)`
	}
	if targetRegion != "us" {
		query += ` AND NOT EXISTS (
			SELECT 1 FROM buckets
			WHERE buckets.tenant_id = object_locations.tenant_id
			  AND buckets.name = object_locations.bucket
			  AND buckets.data_residency = 'us'
		)`
	}

	argIdx := 3
	if p.tenantID != nil {
		query += fmt.Sprintf(" AND tenant_id = $%d", argIdx)
		args = append(args, *p.tenantID)
		argIdx++
	}
	if p.bucket != nil {
		query += fmt.Sprintf(" AND bucket = $%d", argIdx)
		args = append(args, *p.bucket)
	}
	query += " LIMIT 100"

	rows, err := t.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []migrationCandidate
	for rows.Next() {
		var c migrationCandidate
		if err := rows.Scan(&c.tenantID, &c.bucket, &c.objectKey, &c.backendName, &c.sizeBytes); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

func (t *TieringEngine) migrateObject(ctx context.Context, tenant, bucket, key, source, target, targetClass string, sizeBytes int64) error {
	srcDriver, ok := t.drivers[source]
	if !ok {
		return fmt.Errorf("source driver %q not registered", source)
	}
	dstDriver, ok := t.drivers[target]
	if !ok {
		return fmt.Errorf("target driver %q not registered", target)
	}

	reader, err := srcDriver.Get(ctx, bucket, key)
	if err != nil {
		return fmt.Errorf("get from %s: %w", source, err)
	}

	putErr := dstDriver.Put(ctx, bucket, key, reader)
	closeErr := reader.Close()
	if putErr != nil {
		return fmt.Errorf("put to %s: %w", target, putErr)
	}
	if closeErr != nil {
		t.logger.Warn("tiering: close source reader", zap.Error(closeErr))
	}

	if err := t.locations.RecordLocation(ctx, tenant, bucket, key, target, targetClass, sizeBytes); err != nil {
		t.logger.Error("tiering: update location failed, object exists in both backends",
			zap.String("key", bucket+"/"+key),
			zap.Error(err))
		return fmt.Errorf("update location: %w", err)
	}

	t.objectBackends.Store(objectKey(bucket, key), target)

	if err := srcDriver.Delete(ctx, bucket, key); err != nil {
		t.logger.Warn("tiering: delete from source failed, object duplicated (safe)",
			zap.String("source", source),
			zap.String("key", bucket+"/"+key),
			zap.Error(err))
	}

	t.logger.Info("tiering: migrated object",
		zap.String("tenant", tenant),
		zap.String("key", bucket+"/"+key),
		zap.String("from", source),
		zap.String("to", target))

	return nil
}

// MigrateObject is exported for testing.
func (t *TieringEngine) MigrateObject(ctx context.Context, tenant, bucket, key, source, target, targetClass string, sizeBytes int64) error {
	return t.migrateObject(ctx, tenant, bucket, key, source, target, targetClass, sizeBytes)
}

// RunScan is exported for testing.
func (t *TieringEngine) RunScan(ctx context.Context) {
	t.runScan(ctx)
}

// SetInterval overrides the default scan interval (for testing).
func (t *TieringEngine) SetInterval(d time.Duration) {
	t.interval = d
}
