package audit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/database"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testContext() context.Context {
	return context.Background()
}

func testContextWithChi(rctx *chi.Context) context.Context {
	return context.WithValue(context.Background(), chi.RouteCtxKey, rctx)
}

func TestAuditAPIEndpoints(t *testing.T) {
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
	handler := NewAuditAPIHandler(auditor, logger)

	// Seed test data
	userID := uuid.New()
	err = auditor.LogEvent(testContext(), &AuditEvent{
		UserID:    userID,
		TenantID:  "test-tenant",
		EventType: EventTypeLogin,
		Action:    "LOGIN",
		Resource:  "/auth/login",
		Result:    ResultSuccess,
		Severity:  SeverityInfo,
	})
	require.NoError(t, err)

	t.Run("GET /api/v1/audit/events - search events", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/audit/events?tenant_id=test-tenant", nil)
		w := httptest.NewRecorder()

		handler.SearchEvents(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Contains(t, response, "events")
	})

	t.Run("GET /api/v1/audit/stats/overview", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/audit/stats/overview", nil)
		w := httptest.NewRecorder()

		handler.GetOverviewStats(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var stats OverviewStats
		err := json.Unmarshal(w.Body.Bytes(), &stats)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, stats.TotalEvents, int64(0))
	})

	t.Run("GET /api/v1/audit/alerts/failed-logins", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/audit/alerts/failed-logins?threshold=3", nil)
		w := httptest.NewRecorder()

		handler.GetFailedLoginAlerts(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("GET /api/v1/audit/forensics/session/{userID}", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/audit/forensics/session/"+userID.String(), nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("userID", userID.String())
		req = req.WithContext(testContextWithChi(rctx))
		w := httptest.NewRecorder()

		handler.GetUserSession(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("GET /api/v1/audit/reports/soc2", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/audit/reports/soc2", nil)
		w := httptest.NewRecorder()

		handler.GetSOC2Report(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var report SOC2Report
		err := json.Unmarshal(w.Body.Bytes(), &report)
		require.NoError(t, err)
		assert.NotEmpty(t, report.Summary)
	})
}
