// internal/reporting/report_test.go
package reporting

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReportConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &ReportConfig{
			Name:      "monthly-usage",
			Type:      ReportTypeUsage,
			Format:    FormatJSON,
			StartDate: time.Now().AddDate(0, -1, 0),
			EndDate:   time.Now(),
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &ReportConfig{Type: ReportTypeUsage}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})

	t.Run("rejects invalid date range", func(t *testing.T) {
		config := &ReportConfig{
			Name:      "invalid-range",
			Type:      ReportTypeUsage,
			StartDate: time.Now(),
			EndDate:   time.Now().AddDate(0, -1, 0),
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "date")
	})
}

func TestNewReportGenerator(t *testing.T) {
	t.Run("creates generator", func(t *testing.T) {
		gen := NewReportGenerator(nil)
		assert.NotNil(t, gen)
	})
}

func TestReportGenerator_UsageReport(t *testing.T) {
	gen := NewReportGenerator(nil)

	// Add sample data
	gen.RecordUsage(&UsageRecord{
		TenantID:  "tenant-1",
		Operation: "GetObject",
		Bytes:     1024 * 1024,
		Timestamp: time.Now(),
	})
	gen.RecordUsage(&UsageRecord{
		TenantID:  "tenant-1",
		Operation: "PutObject",
		Bytes:     2048 * 1024,
		Timestamp: time.Now(),
	})

	t.Run("generates usage report", func(t *testing.T) {
		config := &ReportConfig{
			Name:      "usage-report",
			Type:      ReportTypeUsage,
			TenantID:  "tenant-1",
			StartDate: time.Now().Add(-24 * time.Hour),
			EndDate:   time.Now().Add(time.Hour),
		}

		report, err := gen.Generate(context.Background(), config)
		require.NoError(t, err)
		assert.Equal(t, "usage-report", report.Name)
		assert.Equal(t, ReportTypeUsage, report.Type)
		assert.NotEmpty(t, report.Data)
	})

	t.Run("aggregates by operation", func(t *testing.T) {
		config := &ReportConfig{
			Name:      "aggregated",
			Type:      ReportTypeUsage,
			TenantID:  "tenant-1",
			GroupBy:   "operation",
			StartDate: time.Now().Add(-24 * time.Hour),
			EndDate:   time.Now().Add(time.Hour),
		}

		report, err := gen.Generate(context.Background(), config)
		require.NoError(t, err)

		data := report.Data.(map[string]interface{})
		assert.Contains(t, data, "by_operation")
	})
}

func TestReportGenerator_BillingReport(t *testing.T) {
	gen := NewReportGenerator(nil)

	gen.RecordUsage(&UsageRecord{
		TenantID:  "tenant-1",
		Operation: "GetObject",
		Bytes:     1024 * 1024 * 1024, // 1GB
		Timestamp: time.Now(),
	})

	t.Run("generates billing report", func(t *testing.T) {
		config := &ReportConfig{
			Name:      "billing-report",
			Type:      ReportTypeBilling,
			TenantID:  "tenant-1",
			StartDate: time.Now().Add(-24 * time.Hour),
			EndDate:   time.Now().Add(time.Hour),
		}

		report, err := gen.Generate(context.Background(), config)
		require.NoError(t, err)

		data := report.Data.(map[string]interface{})
		assert.Contains(t, data, "total_storage_gb")
		assert.Contains(t, data, "total_bandwidth_gb")
		assert.Contains(t, data, "estimated_cost")
	})
}

func TestReportGenerator_ComplianceReport(t *testing.T) {
	gen := NewReportGenerator(nil)

	gen.RecordAuditEvent(&AuditEvent{
		TenantID:  "tenant-1",
		Action:    "DataAccess",
		Resource:  "bucket/object",
		UserID:    "user-1",
		Timestamp: time.Now(),
		Success:   true,
	})

	t.Run("generates compliance report", func(t *testing.T) {
		config := &ReportConfig{
			Name:      "compliance-report",
			Type:      ReportTypeCompliance,
			TenantID:  "tenant-1",
			StartDate: time.Now().Add(-24 * time.Hour),
			EndDate:   time.Now().Add(time.Hour),
		}

		report, err := gen.Generate(context.Background(), config)
		require.NoError(t, err)

		data := report.Data.(map[string]interface{})
		assert.Contains(t, data, "audit_events")
		assert.Contains(t, data, "compliance_status")
	})
}

func TestReportGenerator_SecurityReport(t *testing.T) {
	gen := NewReportGenerator(nil)

	gen.RecordSecurityEvent(&SecurityEvent{
		TenantID:  "tenant-1",
		EventType: "FailedLogin",
		Source:    "192.168.1.100",
		Severity:  SeverityMedium,
		Timestamp: time.Now(),
	})

	t.Run("generates security report", func(t *testing.T) {
		config := &ReportConfig{
			Name:      "security-report",
			Type:      ReportTypeSecurity,
			TenantID:  "tenant-1",
			StartDate: time.Now().Add(-24 * time.Hour),
			EndDate:   time.Now().Add(time.Hour),
		}

		report, err := gen.Generate(context.Background(), config)
		require.NoError(t, err)

		data := report.Data.(map[string]interface{})
		assert.Contains(t, data, "security_events")
		assert.Contains(t, data, "threat_summary")
	})
}

func TestReportGenerator_Export(t *testing.T) {
	gen := NewReportGenerator(nil)

	report := &Report{
		ID:        "report-123",
		Name:      "test-report",
		Type:      ReportTypeUsage,
		CreatedAt: time.Now(),
		Data: map[string]interface{}{
			"total_requests": 1000,
			"total_bytes":    1024 * 1024,
		},
	}

	t.Run("exports to JSON", func(t *testing.T) {
		data, err := gen.Export(report, FormatJSON)
		require.NoError(t, err)
		assert.Contains(t, string(data), "total_requests")
		assert.Contains(t, string(data), "1000")
	})

	t.Run("exports to CSV", func(t *testing.T) {
		data, err := gen.Export(report, FormatCSV)
		require.NoError(t, err)
		assert.NotEmpty(t, data)
	})
}

func TestReportScheduler(t *testing.T) {
	gen := NewReportGenerator(nil)
	scheduler := NewReportScheduler(gen)

	t.Run("schedules report", func(t *testing.T) {
		schedule := &ReportSchedule{
			ID:       "sched-1",
			Config:   &ReportConfig{Name: "daily", Type: ReportTypeUsage},
			Cron:     "0 0 * * *", // Daily at midnight
			TenantID: "tenant-1",
			Enabled:  true,
		}

		err := scheduler.Schedule(schedule)
		assert.NoError(t, err)

		schedules := scheduler.ListSchedules("tenant-1")
		assert.Len(t, schedules, 1)
	})

	t.Run("removes schedule", func(t *testing.T) {
		err := scheduler.Unschedule("sched-1")
		assert.NoError(t, err)

		schedules := scheduler.ListSchedules("tenant-1")
		assert.Empty(t, schedules)
	})
}

func TestReportGenerator_Aggregation(t *testing.T) {
	gen := NewReportGenerator(nil)

	// Add data over multiple days
	for i := 0; i < 7; i++ {
		gen.RecordUsage(&UsageRecord{
			TenantID:  "tenant-1",
			Operation: "GetObject",
			Bytes:     int64(1024 * 1024 * (i + 1)),
			Timestamp: time.Now().AddDate(0, 0, -i),
		})
	}

	t.Run("aggregates by day", func(t *testing.T) {
		config := &ReportConfig{
			Name:      "daily-agg",
			Type:      ReportTypeUsage,
			TenantID:  "tenant-1",
			GroupBy:   "day",
			StartDate: time.Now().AddDate(0, 0, -7),
			EndDate:   time.Now().Add(time.Hour),
		}

		report, err := gen.Generate(context.Background(), config)
		require.NoError(t, err)

		data := report.Data.(map[string]interface{})
		assert.Contains(t, data, "by_day")
	})
}

func TestReportGenerator_Filtering(t *testing.T) {
	gen := NewReportGenerator(nil)

	gen.RecordUsage(&UsageRecord{TenantID: "tenant-1", Operation: "GetObject", Bytes: 1024, Timestamp: time.Now()})
	gen.RecordUsage(&UsageRecord{TenantID: "tenant-1", Operation: "PutObject", Bytes: 2048, Timestamp: time.Now()})
	gen.RecordUsage(&UsageRecord{TenantID: "tenant-2", Operation: "GetObject", Bytes: 4096, Timestamp: time.Now()})

	t.Run("filters by tenant", func(t *testing.T) {
		config := &ReportConfig{
			Name:      "filtered",
			Type:      ReportTypeUsage,
			TenantID:  "tenant-1",
			StartDate: time.Now().Add(-time.Hour),
			EndDate:   time.Now().Add(time.Hour),
		}

		report, err := gen.Generate(context.Background(), config)
		require.NoError(t, err)

		data := report.Data.(map[string]interface{})
		// Should only include tenant-1 data
		assert.Equal(t, int64(3072), data["total_bytes"])
	})

	t.Run("filters by operation", func(t *testing.T) {
		config := &ReportConfig{
			Name:      "filtered-op",
			Type:      ReportTypeUsage,
			TenantID:  "tenant-1",
			Filters:   map[string]string{"operation": "GetObject"},
			StartDate: time.Now().Add(-time.Hour),
			EndDate:   time.Now().Add(time.Hour),
		}

		report, err := gen.Generate(context.Background(), config)
		require.NoError(t, err)

		data := report.Data.(map[string]interface{})
		assert.Equal(t, int64(1024), data["total_bytes"])
	})
}

func TestReportTypes(t *testing.T) {
	t.Run("defines report types", func(t *testing.T) {
		assert.Equal(t, "usage", ReportTypeUsage)
		assert.Equal(t, "billing", ReportTypeBilling)
		assert.Equal(t, "compliance", ReportTypeCompliance)
		assert.Equal(t, "security", ReportTypeSecurity)
		assert.Equal(t, "performance", ReportTypePerformance)
	})
}

func TestExportFormats(t *testing.T) {
	t.Run("defines export formats", func(t *testing.T) {
		assert.Equal(t, "json", FormatJSON)
		assert.Equal(t, "csv", FormatCSV)
		assert.Equal(t, "pdf", FormatPDF)
		assert.Equal(t, "xlsx", FormatXLSX)
	})
}

func TestSeverityLevels(t *testing.T) {
	t.Run("defines severity levels", func(t *testing.T) {
		assert.Equal(t, "low", SeverityLow)
		assert.Equal(t, "medium", SeverityMedium)
		assert.Equal(t, "high", SeverityHigh)
		assert.Equal(t, "critical", SeverityCritical)
	})
}
