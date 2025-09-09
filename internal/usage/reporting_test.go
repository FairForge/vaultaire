// internal/usage/reporting_test.go
package usage

import (
	"context"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReporter_GenerateUsageReport(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	reporter := NewReporter(qm)
	require.NoError(t, reporter.InitializeSchema(context.Background()))
	ctx := context.Background()

	// Create test data with unique tenant ID
	tenantID := "tenant-report-unique-" + time.Now().Format("20060102150405")
	require.NoError(t, qm.CreateTenant(ctx, tenantID, "starter", 1073741824)) // 1GB limit

	// Reserve 500MB
	allowed, err := qm.CheckAndReserve(ctx, tenantID, 500000000)
	require.NoError(t, err)
	require.True(t, allowed)

	// Generate report
	report, err := reporter.GenerateUsageReport(ctx, tenantID, ReportPeriodDaily)
	require.NoError(t, err)

	assert.Equal(t, tenantID, report.TenantID)
	assert.Equal(t, ReportPeriodDaily, report.Period)
	assert.Equal(t, int64(500000000), report.StorageUsed)
	assert.Equal(t, int64(1073741824), report.StorageLimit)
	assert.InDelta(t, 46.57, report.UsagePercent, 0.1)
}

func TestReporter_GenerateTrendAnalysis(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	reporter := NewReporter(qm)
	require.NoError(t, reporter.InitializeSchema(context.Background()))
	ctx := context.Background()

	// Create historical data
	tenantID := "tenant-trend-1"
	require.NoError(t, qm.CreateTenant(ctx, tenantID, "professional", 107374182400)) // 100GB

	// Simulate usage over time - use specific dates
	now := time.Now()
	for i := 0; i < 7; i++ {
		date := now.AddDate(0, 0, -6+i)      // Start 6 days ago
		bytes := int64((i + 1) * 1073741824) // Increase by 1GB each day
		require.NoError(t, reporter.RecordDailyUsageForDate(ctx, tenantID, date, bytes, 0))
	}

	// Generate trend analysis
	trend, err := reporter.GenerateTrendAnalysis(ctx, tenantID, 7)
	require.NoError(t, err)

	assert.Equal(t, tenantID, trend.TenantID)
	assert.Greater(t, trend.GrowthRate, 0.0) // Positive growth
	assert.Equal(t, 7, len(trend.DailyUsage))
	assert.NotNil(t, trend.ProjectedExhaustion)
}

func TestReporter_ExportCSV(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	reporter := NewReporter(qm)
	ctx := context.Background()

	// Create test tenants
	require.NoError(t, qm.CreateTenant(ctx, "tenant-csv-1", "starter", 1073741824))
	require.NoError(t, qm.CreateTenant(ctx, "tenant-csv-2", "professional", 107374182400))

	// Generate CSV report
	csvData, err := reporter.ExportUsageCSV(ctx)
	require.NoError(t, err)

	// Parse CSV
	reader := csv.NewReader(strings.NewReader(string(csvData)))
	records, err := reader.ReadAll()
	require.NoError(t, err)

	// Check headers
	assert.Equal(t, []string{"tenant_id", "tier", "storage_used", "storage_limit", "usage_percent", "created_at"}, records[0])

	// Check we have data rows
	assert.GreaterOrEqual(t, len(records), 3) // Header + 2 tenants
}

func TestReporter_ScheduledReports(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	reporter := NewReporter(qm)
	require.NoError(t, reporter.InitializeSchema(context.Background()))
	ctx := context.Background()

	// Create the tenant first!
	require.NoError(t, qm.CreateTenant(ctx, "tenant-scheduled", "starter", 1073741824))

	// Schedule a report
	schedule := &ReportSchedule{
		TenantID:   "tenant-scheduled",
		Period:     ReportPeriodWeekly,
		Recipients: []string{"admin@example.com"},
		Format:     ReportFormatJSON,
		Enabled:    true,
	}

	err := reporter.CreateSchedule(ctx, schedule)
	require.NoError(t, err)
	assert.NotEmpty(t, schedule.ID)

	// Get scheduled reports
	schedules, err := reporter.GetSchedules(ctx, "tenant-scheduled")
	require.NoError(t, err)
	assert.Len(t, schedules, 1)
	assert.Equal(t, ReportPeriodWeekly, schedules[0].Period)
}
