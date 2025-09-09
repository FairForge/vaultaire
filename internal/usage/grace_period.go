// internal/usage/grace_period.go
package usage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type GracePeriodStatus string

const (
	GracePeriodStatusActive   GracePeriodStatus = "active"
	GracePeriodStatusExpired  GracePeriodStatus = "expired"
	GracePeriodStatusConsumed GracePeriodStatus = "consumed"
)

type GracePeriod struct {
	TenantID          string            `json:"tenant_id"`
	Status            GracePeriodStatus `json:"status"`
	StartedAt         time.Time         `json:"started_at"`
	ExpiresAt         *time.Time        `json:"expires_at"`
	Duration          time.Duration     `json:"duration"`
	ExtensionCount    int               `json:"extension_count"`
	NotificationsSent []time.Time       `json:"notifications_sent,omitempty"`
}

type GracePeriodManager struct {
	quotaManager      *QuotaManager
	defaultDuration   time.Duration
	maxExtensions     int
	notificationHours []int // Hours before expiry to send notifications
}

func NewGracePeriodManager(qm *QuotaManager) *GracePeriodManager {
	return &GracePeriodManager{
		quotaManager:      qm,
		defaultDuration:   72 * time.Hour, // 3 days default
		maxExtensions:     2,
		notificationHours: []int{48, 24, 6, 1}, // Notify at these hours before expiry
	}
}

func (gpm *GracePeriodManager) InitializeSchema(ctx context.Context) error {
	schema := `
    CREATE TABLE IF NOT EXISTS grace_periods (
        tenant_id TEXT PRIMARY KEY REFERENCES tenant_quotas(tenant_id),
        status VARCHAR(20) NOT NULL DEFAULT 'active',
        started_at TIMESTAMP NOT NULL DEFAULT NOW(),
        expires_at TIMESTAMP NOT NULL,
        extension_count INT DEFAULT 0,
        last_notification TIMESTAMP,
        created_at TIMESTAMP DEFAULT NOW(),
        updated_at TIMESTAMP DEFAULT NOW()
    );

    CREATE INDEX IF NOT EXISTS idx_grace_periods_expires
        ON grace_periods(expires_at)
        WHERE status = 'active';
    `

	_, err := gpm.quotaManager.db.ExecContext(ctx, schema)
	return err
}

func (gpm *GracePeriodManager) StartGracePeriod(ctx context.Context, tenantID string, duration time.Duration) (*GracePeriod, error) {
	// Only use default if duration is exactly 0, not negative (for testing)
	if duration == 0 {
		duration = gpm.defaultDuration
	}

	expiresAt := time.Now().Add(duration)

	query := `
        INSERT INTO grace_periods (tenant_id, status, expires_at)
        VALUES ($1, $2, $3)
        ON CONFLICT (tenant_id)
        DO UPDATE SET
            status = $2,
            expires_at = $3,
            extension_count = 0,
            updated_at = NOW()
        RETURNING started_at, expires_at, extension_count`

	var grace GracePeriod
	err := gpm.quotaManager.db.QueryRowContext(ctx, query,
		tenantID, GracePeriodStatusActive, expiresAt).
		Scan(&grace.StartedAt, &grace.ExpiresAt, &grace.ExtensionCount)

	if err != nil {
		return nil, fmt.Errorf("starting grace period: %w", err)
	}

	grace.TenantID = tenantID
	grace.Status = GracePeriodStatusActive
	grace.Duration = duration

	return &grace, nil
}

func (gpm *GracePeriodManager) GetGracePeriod(ctx context.Context, tenantID string) (*GracePeriod, error) {
	query := `
        SELECT status, started_at, expires_at, extension_count
        FROM grace_periods
        WHERE tenant_id = $1`

	var grace GracePeriod
	err := gpm.quotaManager.db.QueryRowContext(ctx, query, tenantID).
		Scan(&grace.Status, &grace.StartedAt, &grace.ExpiresAt, &grace.ExtensionCount)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting grace period: %w", err)
	}

	grace.TenantID = tenantID
	if grace.ExpiresAt != nil {
		grace.Duration = grace.ExpiresAt.Sub(grace.StartedAt)
	}

	return &grace, nil
}

func (gpm *GracePeriodManager) ExtendGracePeriod(ctx context.Context, tenantID string, extension time.Duration) (*GracePeriod, error) {
	// Check current grace period
	current, err := gpm.GetGracePeriod(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, fmt.Errorf("no grace period exists for tenant %s", tenantID)
	}

	if current.ExtensionCount >= gpm.maxExtensions {
		return nil, fmt.Errorf("maximum extensions (%d) reached", gpm.maxExtensions)
	}

	newExpiry := current.ExpiresAt.Add(extension)

	query := `
        UPDATE grace_periods
        SET expires_at = $1,
            extension_count = extension_count + 1,
            updated_at = NOW()
        WHERE tenant_id = $2 AND status = $3
        RETURNING started_at, expires_at, extension_count`

	var grace GracePeriod
	err = gpm.quotaManager.db.QueryRowContext(ctx, query,
		newExpiry, tenantID, GracePeriodStatusActive).
		Scan(&grace.StartedAt, &grace.ExpiresAt, &grace.ExtensionCount)

	if err != nil {
		return nil, fmt.Errorf("extending grace period: %w", err)
	}

	grace.TenantID = tenantID
	grace.Status = GracePeriodStatusActive

	return &grace, nil
}

func (gpm *GracePeriodManager) ProcessExpiredGracePeriods(ctx context.Context) ([]string, error) {
	query := `
        UPDATE grace_periods
        SET status = $1,
            updated_at = NOW()
        WHERE status = $2 AND expires_at < NOW()
        RETURNING tenant_id`

	rows, err := gpm.quotaManager.db.QueryContext(ctx, query,
		GracePeriodStatusExpired, GracePeriodStatusActive)
	if err != nil {
		return nil, fmt.Errorf("processing expired grace periods: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var expired []string
	for rows.Next() {
		var tenantID string
		if err := rows.Scan(&tenantID); err != nil {
			continue
		}
		expired = append(expired, tenantID)
	}

	return expired, nil
}

func (gpm *GracePeriodManager) CancelGracePeriod(ctx context.Context, tenantID string) error {
	query := `
        UPDATE grace_periods
        SET status = $1,
            updated_at = NOW()
        WHERE tenant_id = $2 AND status = $3`

	_, err := gpm.quotaManager.db.ExecContext(ctx, query,
		GracePeriodStatusConsumed, tenantID, GracePeriodStatusActive)

	return err
}
