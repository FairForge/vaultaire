package usage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// BackendCostPerTBCents maps backend names to their per-TB storage cost in cents.
var BackendCostPerTBCents = map[string]int64{
	"geyser":   155,
	"idrive":   330,
	"hetzner":  381,
	"onedrive": 0,
	"gorilla":  0,
	"local":    0,
	"edge":     0,
}

const bytesPerTB = 1024.0 * 1024 * 1024 * 1024

type CostTracker struct {
	db     *sql.DB
	logger *zap.Logger
	stop   chan struct{}
}

func NewCostTracker(db *sql.DB, logger *zap.Logger) *CostTracker {
	return &CostTracker{
		db:     db,
		logger: logger,
		stop:   make(chan struct{}),
	}
}

func (c *CostTracker) Start(ctx context.Context) {
	if c.db == nil {
		return
	}

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	// Run once on start.
	c.aggregate(ctx)

	for {
		select {
		case <-ticker.C:
			c.aggregate(ctx)
		case <-c.stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (c *CostTracker) Stop() {
	close(c.stop)
}

func (c *CostTracker) aggregate(ctx context.Context) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT tenant_id, backend_name, SUM(size_bytes)
		FROM object_locations
		GROUP BY tenant_id, backend_name`)
	if err != nil {
		c.logger.Error("cost_tracker: query object_locations", zap.Error(err))
		return
	}
	defer func() { _ = rows.Close() }()

	var upserted int
	for rows.Next() {
		var tenantID, backendName string
		var storageBytes int64
		if err := rows.Scan(&tenantID, &backendName, &storageBytes); err != nil {
			c.logger.Error("cost_tracker: scan row", zap.Error(err))
			continue
		}

		costMicrocents := computeCostMicrocents(backendName, storageBytes)

		_, err := c.db.ExecContext(ctx, `
			INSERT INTO tenant_cost_daily (tenant_id, date, backend_name, storage_bytes, cost_microcents)
			VALUES ($1, CURRENT_DATE, $2, $3, $4)
			ON CONFLICT (tenant_id, date, backend_name) DO UPDATE SET
				storage_bytes = EXCLUDED.storage_bytes,
				cost_microcents = EXCLUDED.cost_microcents`,
			tenantID, backendName, storageBytes, costMicrocents)
		if err != nil {
			c.logger.Error("cost_tracker: upsert tenant_cost_daily", zap.Error(err),
				zap.String("tenant", tenantID),
				zap.String("backend", backendName))
			continue
		}
		upserted++
	}

	if err := rows.Err(); err != nil {
		c.logger.Error("cost_tracker: rows iteration", zap.Error(err))
	}

	if upserted > 0 {
		c.logger.Info("cost_tracker: aggregation complete", zap.Int("upserted", upserted))
	}
}

func computeCostMicrocents(backend string, storageBytes int64) int64 {
	perTBCents, ok := BackendCostPerTBCents[backend]
	if !ok || perTBCents == 0 {
		return 0
	}
	storageTB := float64(storageBytes) / bytesPerTB
	centsF := storageTB * float64(perTBCents)
	return int64(centsF * 10000)
}

// Aggregate is exported for testing.
func (c *CostTracker) Aggregate(ctx context.Context) {
	c.aggregate(ctx)
}

// ComputeCostMicrocents is exported for use by other packages.
func ComputeCostMicrocents(backend string, storageBytes int64) int64 {
	return computeCostMicrocents(backend, storageBytes)
}

// FormatMicrocents formats microcents as a dollar string.
func FormatMicrocents(mc int64) string {
	cents := float64(mc) / 10000
	return fmt.Sprintf("$%.4f", cents/100)
}
