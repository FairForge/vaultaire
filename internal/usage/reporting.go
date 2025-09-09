// internal/usage/reporting.go
package usage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"time"

	"github.com/lib/pq"
)

type ReportPeriod string

const (
	ReportPeriodDaily   ReportPeriod = "daily"
	ReportPeriodWeekly  ReportPeriod = "weekly"
	ReportPeriodMonthly ReportPeriod = "monthly"
)

type ReportFormat string

const (
	ReportFormatJSON ReportFormat = "json"
	ReportFormatCSV  ReportFormat = "csv"
	ReportFormatPDF  ReportFormat = "pdf"
)

type UsageReport struct {
	TenantID      string       `json:"tenant_id"`
	Period        ReportPeriod `json:"period"`
	StartDate     time.Time    `json:"start_date"`
	EndDate       time.Time    `json:"end_date"`
	StorageUsed   int64        `json:"storage_used"`
	StorageLimit  int64        `json:"storage_limit"`
	UsagePercent  float64      `json:"usage_percent"`
	BandwidthUsed int64        `json:"bandwidth_used,omitempty"`
	ObjectCount   int64        `json:"object_count,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
}

type TrendAnalysis struct {
	TenantID            string     `json:"tenant_id"`
	Period              int        `json:"period_days"`
	GrowthRate          float64    `json:"growth_rate_percent"`
	DailyUsage          []int64    `json:"daily_usage"`
	PeakUsage           int64      `json:"peak_usage"`
	AverageUsage        int64      `json:"average_usage"`
	ProjectedExhaustion *time.Time `json:"projected_exhaustion,omitempty"`
}

type ReportSchedule struct {
	ID         string       `json:"id"`
	TenantID   string       `json:"tenant_id"`
	Period     ReportPeriod `json:"period"`
	Recipients []string     `json:"recipients"`
	Format     ReportFormat `json:"format"`
	NextRun    time.Time    `json:"next_run"`
	Enabled    bool         `json:"enabled"`
	CreatedAt  time.Time    `json:"created_at"`
}

type Reporter struct {
	quotaManager *QuotaManager
	db           *sql.DB
}

func NewReporter(qm *QuotaManager) *Reporter {
	return &Reporter{
		quotaManager: qm,
		db:           qm.db,
	}
}

func (r *Reporter) InitializeSchema(ctx context.Context) error {
	schema := `
    CREATE TABLE IF NOT EXISTS usage_reports (
        id SERIAL PRIMARY KEY,
        tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id),
        period VARCHAR(20) NOT NULL,
        start_date DATE NOT NULL,
        end_date DATE NOT NULL,
        storage_used BIGINT,
        storage_limit BIGINT,
        bandwidth_used BIGINT,
        object_count BIGINT,
        created_at TIMESTAMP DEFAULT NOW()
    );

    CREATE TABLE IF NOT EXISTS usage_daily_snapshots (
        id SERIAL PRIMARY KEY,
        tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id),
        snapshot_date DATE NOT NULL,
        storage_used BIGINT,
        bandwidth_used BIGINT,
        object_count BIGINT,
        created_at TIMESTAMP DEFAULT NOW(),
        UNIQUE(tenant_id, snapshot_date)
    );

    CREATE TABLE IF NOT EXISTS report_schedules (
        id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        tenant_id TEXT NOT NULL REFERENCES tenant_quotas(tenant_id),
        period VARCHAR(20) NOT NULL,
        recipients TEXT[],
        format VARCHAR(10) NOT NULL,
        next_run TIMESTAMP NOT NULL,
        enabled BOOLEAN DEFAULT true,
        created_at TIMESTAMP DEFAULT NOW()
    );

    CREATE INDEX IF NOT EXISTS idx_daily_snapshots_tenant_date
        ON usage_daily_snapshots(tenant_id, snapshot_date DESC);
    `

	_, err := r.db.ExecContext(ctx, schema)
	return err
}

func (r *Reporter) GenerateUsageReport(ctx context.Context, tenantID string, period ReportPeriod) (*UsageReport, error) {
	// Get current usage
	used, limit, err := r.quotaManager.GetUsage(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("getting usage: %w", err)
	}

	now := time.Now()
	var startDate time.Time

	switch period {
	case ReportPeriodDaily:
		startDate = now.AddDate(0, 0, -1)
	case ReportPeriodWeekly:
		startDate = now.AddDate(0, 0, -7)
	case ReportPeriodMonthly:
		startDate = now.AddDate(0, -1, 0)
	default:
		startDate = now.AddDate(0, 0, -1)
	}

	report := &UsageReport{
		TenantID:     tenantID,
		Period:       period,
		StartDate:    startDate,
		EndDate:      now,
		StorageUsed:  used,
		StorageLimit: limit,
		UsagePercent: float64(used) / float64(limit) * 100,
		CreatedAt:    now,
	}

	// Store report
	_, err = r.db.ExecContext(ctx, `
        INSERT INTO usage_reports (tenant_id, period, start_date, end_date,
                                   storage_used, storage_limit, created_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		tenantID, period, startDate, now, used, limit, now)

	return report, err
}

func (r *Reporter) RecordDailyUsageForDate(ctx context.Context, tenantID string, date time.Time, storageUsed, bandwidthUsed int64) error {
	query := `
        INSERT INTO usage_daily_snapshots (tenant_id, snapshot_date, storage_used, bandwidth_used)
        VALUES ($1, $2, $3, $4)
        ON CONFLICT (tenant_id, snapshot_date)
        DO UPDATE SET storage_used = $3, bandwidth_used = $4`

	_, err := r.db.ExecContext(ctx, query, tenantID, date.Format("2006-01-02"), storageUsed, bandwidthUsed)
	return err
}

func (r *Reporter) GenerateTrendAnalysis(ctx context.Context, tenantID string, days int) (*TrendAnalysis, error) {
	// Get historical snapshots
	query := `
        SELECT snapshot_date, storage_used
        FROM usage_daily_snapshots
        WHERE tenant_id = $1
        AND snapshot_date >= CURRENT_DATE - INTERVAL '%d days'
        ORDER BY snapshot_date ASC`

	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(query, days), tenantID)
	if err != nil {
		return nil, fmt.Errorf("querying snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var dailyUsage []int64
	var total, peak int64

	for rows.Next() {
		var date time.Time
		var usage int64
		if err := rows.Scan(&date, &usage); err != nil {
			continue
		}
		dailyUsage = append(dailyUsage, usage)
		total += usage
		if usage > peak {
			peak = usage
		}
	}

	if len(dailyUsage) == 0 {
		return nil, fmt.Errorf("no usage data available")
	}

	// Calculate growth rate
	growthRate := 0.0
	if len(dailyUsage) > 1 {
		first := dailyUsage[0]
		last := dailyUsage[len(dailyUsage)-1]
		if first > 0 {
			growthRate = float64(last-first) / float64(first) * 100
		}
	}

	// Calculate projected exhaustion
	var projectedExhaustion *time.Time
	if growthRate > 0 {
		_, limit, _ := r.quotaManager.GetUsage(ctx, tenantID)
		currentUsage := dailyUsage[len(dailyUsage)-1]
		dailyGrowth := float64(currentUsage) * (growthRate / 100) / float64(days)

		if dailyGrowth > 0 {
			daysToExhaustion := float64(limit-currentUsage) / dailyGrowth
			if daysToExhaustion > 0 && daysToExhaustion < 365 {
				exhaustion := time.Now().AddDate(0, 0, int(daysToExhaustion))
				projectedExhaustion = &exhaustion
			}
		}
	}

	trend := &TrendAnalysis{
		TenantID:            tenantID,
		Period:              days,
		GrowthRate:          growthRate,
		DailyUsage:          dailyUsage,
		PeakUsage:           peak,
		AverageUsage:        total / int64(len(dailyUsage)),
		ProjectedExhaustion: projectedExhaustion,
	}

	return trend, nil
}

func (r *Reporter) ExportUsageCSV(ctx context.Context) ([]byte, error) {
	query := `
        SELECT t.tenant_id, t.tier, t.storage_used_bytes, t.storage_limit_bytes, t.created_at
        FROM tenant_quotas t
        ORDER BY t.created_at DESC`

	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("querying tenants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)

	// Write headers
	_ = writer.Write([]string{"tenant_id", "tier", "storage_used", "storage_limit", "usage_percent", "created_at"})

	for rows.Next() {
		var tenantID, tier string
		var used, limit int64
		var createdAt time.Time

		if err := rows.Scan(&tenantID, &tier, &used, &limit, &createdAt); err != nil {
			continue
		}

		usagePercent := fmt.Sprintf("%.2f", float64(used)/float64(limit)*100)

		_ = writer.Write([]string{
			tenantID,
			tier,
			fmt.Sprintf("%d", used),
			fmt.Sprintf("%d", limit),
			usagePercent,
			createdAt.Format(time.RFC3339),
		})
	}

	writer.Flush()
	return buf.Bytes(), nil
}

func (r *Reporter) CreateSchedule(ctx context.Context, schedule *ReportSchedule) error {
	// Calculate next run time
	now := time.Now()
	switch schedule.Period {
	case ReportPeriodDaily:
		schedule.NextRun = now.AddDate(0, 0, 1)
	case ReportPeriodWeekly:
		schedule.NextRun = now.AddDate(0, 0, 7)
	case ReportPeriodMonthly:
		schedule.NextRun = now.AddDate(0, 1, 0)
	}

	query := `
        INSERT INTO report_schedules (tenant_id, period, recipients, format, next_run, enabled)
        VALUES ($1, $2, $3, $4, $5, $6)
        RETURNING id`

	err := r.db.QueryRowContext(ctx, query,
		schedule.TenantID, schedule.Period, pq.Array(schedule.Recipients), // USE pq.Array
		schedule.Format, schedule.NextRun, schedule.Enabled).Scan(&schedule.ID)

	return err
}

func (r *Reporter) GetSchedules(ctx context.Context, tenantID string) ([]*ReportSchedule, error) {
	query := `
        SELECT id, period, recipients, format, next_run, enabled, created_at
        FROM report_schedules
        WHERE tenant_id = $1 AND enabled = true`

	rows, err := r.db.QueryContext(ctx, query, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var schedules []*ReportSchedule
	for rows.Next() {
		s := &ReportSchedule{TenantID: tenantID}
		err := rows.Scan(&s.ID, &s.Period, pq.Array(&s.Recipients), // USE pq.Array
			&s.Format, &s.NextRun, &s.Enabled, &s.CreatedAt)
		if err != nil {
			continue
		}
		schedules = append(schedules, s)
	}

	return schedules, nil
}
