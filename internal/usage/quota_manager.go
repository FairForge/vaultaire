// internal/usage/quota_manager.go
package usage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type QuotaManager struct {
	db *sql.DB
}

func NewQuotaManager(db *sql.DB) *QuotaManager {
	return &QuotaManager{db: db}
}

func (m *QuotaManager) InitializeSchema(ctx context.Context) error {
	schema := `
    CREATE TABLE IF NOT EXISTS tenant_quotas (
        tenant_id TEXT PRIMARY KEY,
        storage_limit_bytes BIGINT NOT NULL DEFAULT 1099511627776,
        storage_used_bytes BIGINT NOT NULL DEFAULT 0,
        bandwidth_limit_bytes BIGINT DEFAULT NULL,
        bandwidth_used_bytes BIGINT NOT NULL DEFAULT 0,
        tier VARCHAR(50) NOT NULL DEFAULT 'starter',
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
        updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );

    CREATE TABLE IF NOT EXISTS quota_usage_events (
        id SERIAL PRIMARY KEY,
        tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id),
        operation VARCHAR(20) NOT NULL,
        bytes_delta BIGINT NOT NULL,
        object_key TEXT NOT NULL,
        timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );

    CREATE INDEX IF NOT EXISTS idx_usage_events_tenant_time
        ON quota_usage_events(tenant_id, timestamp);
    CREATE INDEX IF NOT EXISTS idx_usage_events_operation
        ON quota_usage_events(operation);
    `

	_, err := m.db.ExecContext(ctx, schema)
	if err != nil {
		return fmt.Errorf("initializing quota schema: %w", err)
	}
	return nil
}

func (m *QuotaManager) CreateTenant(ctx context.Context, tenantID, tier string, limitBytes int64) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO tenant_quotas (tenant_id, tier, storage_limit_bytes)
         VALUES ($1, $2, $3)
         ON CONFLICT (tenant_id) DO UPDATE
         SET tier = $2, storage_limit_bytes = $3, updated_at = NOW()`,
		tenantID, tier, limitBytes)

	if err != nil {
		return fmt.Errorf("creating tenant %s: %w", tenantID, err)
	}
	return nil
}

func (m *QuotaManager) CheckAndReserve(ctx context.Context, tenantID string, bytes int64) (bool, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Lock row for update
	var used, limit int64
	err = tx.QueryRowContext(ctx,
		`SELECT storage_used_bytes, storage_limit_bytes
         FROM tenant_quotas
         WHERE tenant_id = $1
         FOR UPDATE`,
		tenantID).Scan(&used, &limit)

	if err != nil {
		return false, fmt.Errorf("checking quota for tenant %s: %w", tenantID, err)
	}

	if used+bytes > limit {
		return false, nil // Quota exceeded but not an error
	}

	// Reserve the space
	_, err = tx.ExecContext(ctx,
		`UPDATE tenant_quotas
         SET storage_used_bytes = storage_used_bytes + $1,
             updated_at = NOW()
         WHERE tenant_id = $2`,
		bytes, tenantID)

	if err != nil {
		return false, fmt.Errorf("updating quota: %w", err)
	}

	// Record event
	_, err = tx.ExecContext(ctx,
		`INSERT INTO quota_usage_events (tenant_id, operation, bytes_delta, object_key)
         VALUES ($1, 'RESERVE', $2, '')`,
		tenantID, bytes)

	if err != nil {
		return false, fmt.Errorf("recording event: %w", err)
	}

	return true, tx.Commit()
}

func (m *QuotaManager) ReleaseQuota(ctx context.Context, tenantID string, bytes int64) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE tenant_quotas
         SET storage_used_bytes = GREATEST(0, storage_used_bytes - $1),
             updated_at = NOW()
         WHERE tenant_id = $2`,
		bytes, tenantID)

	if err != nil {
		return fmt.Errorf("releasing quota for tenant %s: %w", tenantID, err)
	}

	return nil
}

func (m *QuotaManager) GetUsage(ctx context.Context, tenantID string) (used, limit int64, err error) {
	err = m.db.QueryRowContext(ctx,
		`SELECT storage_used_bytes, storage_limit_bytes
         FROM tenant_quotas
         WHERE tenant_id = $1`,
		tenantID).Scan(&used, &limit)

	if err != nil {
		return 0, 0, fmt.Errorf("getting usage for tenant %s: %w", tenantID, err)
	}

	return used, limit, nil
}

func (qm *QuotaManager) UpdateQuota(ctx context.Context, tenantID string, newLimit int64) error {
	query := `
        UPDATE tenant_quotas
        SET storage_limit_bytes = $1, updated_at = NOW()
        WHERE tenant_id = $2`

	_, err := qm.db.ExecContext(ctx, query, newLimit, tenantID)
	return err
}

// Fix ListQuotas to use correct column names:
func (qm *QuotaManager) ListQuotas(ctx context.Context) ([]map[string]interface{}, error) {
	query := `
        SELECT tenant_id, tier, storage_limit_bytes, storage_used_bytes, created_at
        FROM tenant_quotas
        ORDER BY created_at DESC`

	rows, err := qm.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var quotas []map[string]interface{}
	for rows.Next() {
		q := make(map[string]interface{})
		var tenantID, tier string
		var storageLimit, storageUsed int64
		var createdAt time.Time

		err := rows.Scan(&tenantID, &tier, &storageLimit, &storageUsed, &createdAt)
		if err != nil {
			continue
		}

		q["tenant_id"] = tenantID
		q["plan"] = tier // Map tier to plan for API consistency
		q["storage_limit"] = storageLimit
		q["storage_used"] = storageUsed
		q["created_at"] = createdAt

		quotas = append(quotas, q)
	}

	return quotas, nil
}

func (qm *QuotaManager) DeleteQuota(ctx context.Context, tenantID string) error {
	query := `DELETE FROM tenant_quotas WHERE tenant_id = $1`
	_, err := qm.db.ExecContext(ctx, query, tenantID)
	return err
}

// GetTier returns the current tier for a tenant
func (m *QuotaManager) GetTier(ctx context.Context, tenantID string) (string, error) {
	var tier string
	err := m.db.QueryRowContext(ctx,
		"SELECT tier FROM tenant_quotas WHERE tenant_id = $1", tenantID).Scan(&tier)
	return tier, err
}

// UpdateTier updates the tier and associated limits
func (m *QuotaManager) UpdateTier(ctx context.Context, tenantID, newTier string) error {
	limits := map[string]int64{
		"starter":      1099511627776,   // 1TB
		"professional": 10995116277760,  // 10TB
		"enterprise":   109951162777600, // 100TB
	}

	limit, ok := limits[newTier]
	if !ok {
		return fmt.Errorf("invalid tier: %s", newTier)
	}

	_, err := m.db.ExecContext(ctx,
		`UPDATE tenant_quotas
		 SET tier = $1, storage_limit_bytes = $2, updated_at = CURRENT_TIMESTAMP
		 WHERE tenant_id = $3`,
		newTier, limit, tenantID)

	return err
}

// GetUsageHistory returns historical usage data
func (m *QuotaManager) GetUsageHistory(ctx context.Context, tenantID string, days int) ([]map[string]interface{}, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT DATE(timestamp) as date,
		        MAX(bytes_delta) as peak_usage,
		        SUM(CASE WHEN operation = 'PUT' THEN bytes_delta ELSE 0 END) as uploaded,
		        SUM(CASE WHEN operation = 'DELETE' THEN -bytes_delta ELSE 0 END) as deleted
		 FROM quota_usage_events
		 WHERE tenant_id = $1 AND timestamp > NOW() - INTERVAL '%d days'
		 GROUP BY DATE(timestamp)
		 ORDER BY date DESC`, tenantID, days)

	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var history []map[string]interface{}
	for rows.Next() {
		var date string
		var peak, uploaded, deleted int64

		if err := rows.Scan(&date, &peak, &uploaded, &deleted); err != nil {
			return nil, err
		}

		history = append(history, map[string]interface{}{
			"date":       date,
			"peak_usage": peak,
			"uploaded":   uploaded,
			"deleted":    deleted,
		})
	}

	return history, nil
}

// GetTier returns the current tier for a tenant
