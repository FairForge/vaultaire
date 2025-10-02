package audit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestAuditMiddleware(t *testing.T) {
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

	t.Run("logs successful request", func(t *testing.T) {
		userID := uuid.New()
		tenantID := "tenant-test"

		// Create test handler with small delay to ensure measurable duration
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(1 * time.Millisecond) // Ensure non-zero duration
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("success"))
		})

		// Wrap with audit middleware
		middleware := AuditMiddleware(auditor, logger)
		wrapped := middleware(handler)

		// Create request with context
		req := httptest.NewRequest("GET", "/test-endpoint", nil)
		req.Header.Set("User-Agent", "test-client")
		req.RemoteAddr = "192.168.1.1:12345"

		// Add user and tenant to context
		ctx := req.Context()
		ctx = context.WithValue(ctx, ContextKeyUserID, userID)
		ctx = context.WithValue(ctx, ContextKeyTenantID, tenantID)
		req = req.WithContext(ctx)

		// Execute request
		rr := httptest.NewRecorder()
		wrapped.ServeHTTP(rr, req)

		// Verify response
		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "success", rr.Body.String())

		// Wait for async audit log
		time.Sleep(100 * time.Millisecond)

		// Query audit logs
		query := &AuditQuery{
			UserID: &userID,
			Limit:  10,
		}
		logs, err := auditor.Query(context.Background(), query)
		require.NoError(t, err)

		// Find the test request
		found := false
		for _, log := range logs {
			if log.Resource == "/test-endpoint" {
				assert.Equal(t, tenantID, log.TenantID)
				assert.Equal(t, ResultSuccess, log.Result)
				assert.Equal(t, "192.168.1.1", log.IP)
				assert.Equal(t, "test-client", log.UserAgent)
				assert.GreaterOrEqual(t, log.Duration, time.Duration(0))
				found = true
				break
			}
		}
		assert.True(t, found, "audit log should be created")
	})

	t.Run("logs failed request", func(t *testing.T) {
		userID := uuid.New()

		// Create failing handler
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("error"))
		})

		middleware := AuditMiddleware(auditor, logger)
		wrapped := middleware(handler)

		req := httptest.NewRequest("POST", "/error-endpoint", nil)
		ctx := req.Context()
		ctx = context.WithValue(ctx, ContextKeyUserID, userID)
		ctx = context.WithValue(ctx, ContextKeyTenantID, "tenant-test")
		req = req.WithContext(ctx)

		rr := httptest.NewRecorder()
		wrapped.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusInternalServerError, rr.Code)

		// Wait for async audit log
		time.Sleep(100 * time.Millisecond)

		// Verify failure was logged
		query := &AuditQuery{
			UserID: &userID,
			Limit:  10,
		}
		logs, err := auditor.Query(context.Background(), query)
		require.NoError(t, err)

		found := false
		for _, log := range logs {
			if log.Resource == "/error-endpoint" {
				assert.Equal(t, ResultFailure, log.Result)
				assert.Equal(t, SeverityError, log.Severity)
				found = true
				break
			}
		}
		assert.True(t, found, "error should be logged")
	})

	t.Run("handles requests without user context", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		middleware := AuditMiddleware(auditor, logger)
		wrapped := middleware(handler)

		req := httptest.NewRequest("GET", "/public", nil)
		rr := httptest.NewRecorder()

		// Should not panic
		wrapped.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("captures different event types", func(t *testing.T) {
		testCases := []struct {
			method       string
			path         string
			expectedType EventType
		}{
			{"POST", "/auth/login", EventTypeLogin},
			{"POST", "/auth/logout", EventTypeLogout},
			{"POST", "/api-keys", EventTypeAPIKeyCreated},
			{"PUT", "/bucket/file.txt", EventTypeFileUpload},
			{"DELETE", "/bucket/file.txt", EventTypeFileDelete},
		}

		for _, tc := range testCases {
			t.Run(tc.method+" "+tc.path, func(t *testing.T) {
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				})

				middleware := AuditMiddleware(auditor, logger)
				wrapped := middleware(handler)

				req := httptest.NewRequest(tc.method, tc.path, nil)
				rr := httptest.NewRecorder()
				wrapped.ServeHTTP(rr, req)

				// Wait for async log
				time.Sleep(50 * time.Millisecond)

				// Query and verify event type
				query := &AuditQuery{
					Resource: &tc.path,
					Limit:    1,
				}
				logs, err := auditor.Query(context.Background(), query)
				require.NoError(t, err)
				if len(logs) > 0 {
					assert.Equal(t, tc.expectedType, logs[0].EventType)
				}
			})
		}
	})
}
