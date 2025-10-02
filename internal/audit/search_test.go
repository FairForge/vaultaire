package audit

import (
	"context"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestAdvancedSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database test in short mode")
	}

	config := database.GetTestConfig()
	logger := zap.NewNop()

	db, err := database.NewPostgres(config, logger)
	require.NoError(t, err)
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("failed to close database: %v", err)
		}
	}()

	auditor := NewAuditService(db)
	ctx := context.Background()

	t.Run("text search in resource", func(t *testing.T) {
		userID := uuid.New()

		// Create logs with searchable content
		logs := []struct {
			resource string
			action   string
		}{
			{"/api/users/12345/profile", "GET"},
			{"/api/documents/contract.pdf", "GET"},
			{"/api/images/photo.jpg", "PUT"},
			{"/api/users/67890/settings", "POST"},
		}

		for _, l := range logs {
			err := auditor.LogEvent(ctx, &AuditEvent{
				UserID:    userID,
				TenantID:  "tenant-search",
				EventType: EventTypeFileDownload,
				Action:    l.action,
				Resource:  l.resource,
				Result:    ResultSuccess,
			})
			require.NoError(t, err)
		}

		// Search for "users"
		results, err := auditor.SearchResources(ctx, "users", 100)
		require.NoError(t, err)

		count := 0
		for _, log := range results {
			if log.TenantID == "tenant-search" {
				assert.Contains(t, log.Resource, "users")
				count++
			}
		}
		assert.GreaterOrEqual(t, count, 2, "should find at least 2 user-related logs")
	})

	t.Run("search by metadata", func(t *testing.T) {
		userID := uuid.New()

		// Create log with specific metadata
		err := auditor.LogEvent(ctx, &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-metadata",
			EventType: EventTypeFileUpload,
			Action:    "PUT",
			Resource:  "/test-file.txt",
			Result:    ResultSuccess,
			Metadata: map[string]string{
				"file_size": "1024",
				"mime_type": "text/plain",
				"encrypted": "true",
			},
		})
		require.NoError(t, err)

		// Search for logs with encryption enabled
		filter := &MetadataFilter{
			Key:   "encrypted",
			Value: "true",
		}
		results, err := auditor.SearchByMetadata(ctx, filter, 100)
		require.NoError(t, err)

		found := false
		for _, log := range results {
			if log.TenantID == "tenant-metadata" {
				assert.Equal(t, "true", log.Metadata["encrypted"])
				found = true
				break
			}
		}
		assert.True(t, found, "should find encrypted file log")
	})

	t.Run("complex filter with AND conditions", func(t *testing.T) {
		userID := uuid.New()

		// Create test data
		err := auditor.LogEvent(ctx, &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-complex",
			EventType: EventTypeFileUpload,
			Action:    "PUT",
			Resource:  "/important-doc.pdf",
			Result:    ResultSuccess,
			Severity:  SeverityWarning,
		})
		require.NoError(t, err)

		// Complex filter: event type AND severity AND result
		filter := &ComplexFilter{
			EventType: EventTypeFileUpload,
			Severity:  SeverityWarning,
			Result:    ResultSuccess,
		}

		results, err := auditor.SearchComplex(ctx, filter, 100)
		require.NoError(t, err)

		found := false
		for _, log := range results {
			if log.TenantID == "tenant-complex" {
				assert.Equal(t, EventTypeFileUpload, log.EventType)
				assert.Equal(t, SeverityWarning, log.Severity)
				assert.Equal(t, ResultSuccess, log.Result)
				found = true
				break
			}
		}
		assert.True(t, found, "should find log matching all conditions")
	})

	t.Run("date range search", func(t *testing.T) {
		userID := uuid.New()

		// Use fixed timestamps for predictable testing
		baseTime := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)

		// Create one log in January
		err := auditor.LogEvent(ctx, &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-daterange-jan",
			EventType: EventTypeFileUpload,
			Action:    "PUT",
			Resource:  "/january-file.txt",
			Result:    ResultSuccess,
			Timestamp: baseTime,
		})
		require.NoError(t, err)

		// Create one log in December (previous month)
		decemberTime := baseTime.AddDate(0, -1, 0)
		err = auditor.LogEvent(ctx, &AuditEvent{
			UserID:    userID,
			TenantID:  "tenant-daterange-dec",
			EventType: EventTypeFileUpload,
			Action:    "PUT",
			Resource:  "/december-file.txt",
			Result:    ResultSuccess,
			Timestamp: decemberTime,
		})
		require.NoError(t, err)

		// Search for January logs only
		filter := &DateRangeFilter{
			Start: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			End:   time.Date(2025, 1, 31, 23, 59, 59, 0, time.UTC),
		}

		results, err := auditor.SearchDateRange(ctx, filter, 100)
		require.NoError(t, err)

		// Check results
		foundJan := false
		foundDec := false
		for _, log := range results {
			if log.TenantID == "tenant-daterange-jan" {
				foundJan = true
			}
			if log.TenantID == "tenant-daterange-dec" {
				foundDec = true
			}
		}

		assert.True(t, foundJan, "should find January log")
		assert.False(t, foundDec, "should not find December log")
	})

	t.Run("search with pagination", func(t *testing.T) {
		userID := uuid.New()

		// Create 15 logs
		for i := 0; i < 15; i++ {
			err := auditor.LogEvent(ctx, &AuditEvent{
				UserID:    userID,
				TenantID:  "tenant-pagination",
				EventType: EventTypeFileUpload,
				Action:    "PUT",
				Resource:  "/file.txt",
				Result:    ResultSuccess,
			})
			require.NoError(t, err)
		}

		// Get first page (10 results)
		results1, err := auditor.SearchResources(ctx, "file.txt", 10)
		require.NoError(t, err)

		count1 := 0
		for _, log := range results1 {
			if log.TenantID == "tenant-pagination" {
				count1++
			}
		}

		assert.LessOrEqual(t, count1, 10, "first page should have max 10 results")
	})
}
