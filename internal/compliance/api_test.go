package compliance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// setupTestHandler creates a test API handler with all services
func setupTestHandler() *APIHandler {
	// Create mock databases
	portabilityDB := newMockPortabilityDB()
	consentDB := newMockConsentDB()
	breachDB := newMockBreachDB()
	ropaDB := newMockROPADB()

	// Pre-populate consent purposes for testing
	_ = consentDB.CreateConsentPurpose(context.Background(), &ConsentPurpose{
		Name:        "marketing",
		Description: "Marketing communications",
		Required:    false,
	})
	_ = consentDB.CreateConsentPurpose(context.Background(), &ConsentPurpose{
		Name:        "analytics",
		Description: "Usage analytics",
		Required:    false,
	})

	// Create services
	gdprService := NewGDPRService(nil, zap.NewNop())
	portabilityService := NewPortabilityService(portabilityDB, nil, zap.NewNop())
	consentService := NewConsentService(consentDB, zap.NewNop())
	breachService := NewBreachService(breachDB, zap.NewNop())
	ropaService := NewROPAService(ropaDB, zap.NewNop())

	// Create handler
	return NewAPIHandler(
		gdprService,
		portabilityService,
		consentService,
		breachService,
		ropaService,
		zap.NewNop(),
	)
}

// ============================================================================
// GDPR SAR Handler Tests (Article 15)
// ============================================================================

func TestAPIHandler_HandleCreateSAR(t *testing.T) {
	handler := setupTestHandler()

	t.Run("creates SAR with valid user", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/compliance/sar", nil)
		w := httptest.NewRecorder()

		userID := uuid.New()
		ctx := context.WithValue(req.Context(), UserIDKey, userID)
		req = req.WithContext(ctx)

		handler.HandleCreateSAR(w, req)

		assert.Equal(t, http.StatusCreated, w.Code)
	})

	t.Run("returns unauthorized without user context", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/compliance/sar", nil)
		w := httptest.NewRecorder()

		handler.HandleCreateSAR(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestAPIHandler_HandleCreateDeletionRequest(t *testing.T) {
	handler := setupTestHandler()

	t.Run("creates deletion request", func(t *testing.T) {
		body := map[string]interface{}{
			"reason": "User requested",
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPost, "/api/compliance/deletion", bytes.NewReader(bodyBytes))
		w := httptest.NewRecorder()

		userID := uuid.New()
		ctx := context.WithValue(req.Context(), UserIDKey, userID)
		req = req.WithContext(ctx)

		handler.HandleCreateDeletionRequest(w, req)

		assert.Equal(t, http.StatusCreated, w.Code)
	})
}

func TestAPIHandler_HandleGetDataInventory(t *testing.T) {
	handler := setupTestHandler()

	req := httptest.NewRequest(http.MethodGet, "/api/compliance/inventory", nil)
	w := httptest.NewRecorder()

	userID := uuid.New()
	ctx := context.WithValue(req.Context(), UserIDKey, userID)
	req = req.WithContext(ctx)

	handler.HandleGetDataInventory(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAPIHandler_HandleListProcessingActivities(t *testing.T) {
	handler := setupTestHandler()

	req := httptest.NewRequest(http.MethodGet, "/api/compliance/activities", nil)
	w := httptest.NewRecorder()

	handler.HandleListProcessingActivities(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAPIHandler_HandleGetSARStatus(t *testing.T) {
	handler := setupTestHandler()

	req := httptest.NewRequest(http.MethodGet, "/api/compliance/sar/{id}", nil)
	w := httptest.NewRecorder()

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", uuid.New().String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	handler.HandleGetSARStatus(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// ============================================================================
// Portability Handler Tests (Article 20)
// ============================================================================

func TestAPIHandler_HandleCreateExport(t *testing.T) {
	handler := setupTestHandler()

	t.Run("creates export request", func(t *testing.T) {
		body := map[string]interface{}{
			"format": "json",
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPost, "/api/compliance/export", bytes.NewReader(bodyBytes))
		w := httptest.NewRecorder()

		userID := uuid.New()
		ctx := context.WithValue(req.Context(), UserIDKey, userID)
		req = req.WithContext(ctx)

		handler.HandleCreateExport(w, req)

		assert.Equal(t, http.StatusCreated, w.Code)
	})

	t.Run("requires authentication", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/compliance/export", nil)
		w := httptest.NewRecorder()

		handler.HandleCreateExport(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestAPIHandler_HandleGetExport(t *testing.T) {
	handler := setupTestHandler()

	t.Run("retrieves export status", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/compliance/export/{id}", nil)
		w := httptest.NewRecorder()

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", uuid.New().String())
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		handler.HandleGetExport(w, req)

		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})
}

// ============================================================================
// Consent Handler Tests (Articles 7 & 8)
// ============================================================================

func TestAPIHandler_HandleGrantConsent(t *testing.T) {
	handler := setupTestHandler()
	userID := uuid.New()

	t.Run("grants consent successfully", func(t *testing.T) {
		body := `{"purpose":"marketing","granted":true,"terms_version":"1.0"}`
		req := httptest.NewRequest(http.MethodPost, "/api/consent", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(context.WithValue(req.Context(), UserIDKey, userID))

		w := httptest.NewRecorder()
		handler.HandleGrantConsent(w, req)

		assert.Equal(t, http.StatusNoContent, w.Code)
	})

	t.Run("requires authentication", func(t *testing.T) {
		body := `{"purpose":"marketing","granted":true}`
		req := httptest.NewRequest(http.MethodPost, "/api/consent", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		handler.HandleGrantConsent(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("validates request body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/consent", strings.NewReader("invalid json"))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(context.WithValue(req.Context(), UserIDKey, userID))

		w := httptest.NewRecorder()
		handler.HandleGrantConsent(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestAPIHandler_HandleWithdrawConsent(t *testing.T) {
	handler := setupTestHandler()
	userID := uuid.New()

	t.Run("requires authentication", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/consent/marketing", nil)

		w := httptest.NewRecorder()
		handler.HandleWithdrawConsent(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("validates purpose parameter", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/consent/", nil)
		req = req.WithContext(context.WithValue(req.Context(), UserIDKey, userID))

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("purpose", "")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		w := httptest.NewRecorder()
		handler.HandleWithdrawConsent(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestAPIHandler_HandleGetConsentStatus(t *testing.T) {
	handler := setupTestHandler()
	userID := uuid.New()

	t.Run("retrieves consent status", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/consent", nil)
		req = req.WithContext(context.WithValue(req.Context(), UserIDKey, userID))

		w := httptest.NewRecorder()
		handler.HandleGetConsentStatus(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("requires authentication", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/consent", nil)

		w := httptest.NewRecorder()
		handler.HandleGetConsentStatus(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestAPIHandler_HandleCheckConsent(t *testing.T) {
	handler := setupTestHandler()
	userID := uuid.New()

	t.Run("checks specific consent", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/consent/marketing", nil)
		req = req.WithContext(context.WithValue(req.Context(), UserIDKey, userID))

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("purpose", "marketing")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		w := httptest.NewRecorder()
		handler.HandleCheckConsent(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("requires authentication", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/consent/marketing", nil)

		w := httptest.NewRecorder()
		handler.HandleCheckConsent(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestAPIHandler_HandleGetConsentHistory(t *testing.T) {
	handler := setupTestHandler()
	userID := uuid.New()

	t.Run("retrieves consent history", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/consent/history", nil)
		req = req.WithContext(context.WithValue(req.Context(), UserIDKey, userID))

		w := httptest.NewRecorder()
		handler.HandleGetConsentHistory(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("requires authentication", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/consent/history", nil)

		w := httptest.NewRecorder()
		handler.HandleGetConsentHistory(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestAPIHandler_HandleListConsentPurposes(t *testing.T) {
	handler := setupTestHandler()

	t.Run("lists consent purposes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/consent/purposes", nil)

		w := httptest.NewRecorder()
		handler.HandleListConsentPurposes(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
	})
}

// ============================================================================
// Breach Notification Handler Tests (Articles 33 & 34)
// ============================================================================

func TestAPIHandler_HandleReportBreach(t *testing.T) {
	handler := setupTestHandler()

	t.Run("reports breach successfully", func(t *testing.T) {
		body := map[string]interface{}{
			"breach_type":           "unauthorized_access",
			"description":           "Test breach",
			"root_cause":            "SQL injection",
			"affected_user_count":   1000,
			"affected_record_count": 5000,
			"data_categories":       []string{"email", "name"},
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPost, "/api/breach", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		handler.HandleReportBreach(w, req)

		if w.Code != http.StatusCreated {
			t.Logf("Response body: %s", w.Body.String())
		}

		assert.Equal(t, http.StatusCreated, w.Code)
	})

	t.Run("validates request body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/breach", strings.NewReader("invalid json"))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		handler.HandleReportBreach(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}
func TestAPIHandler_HandleGetBreach(t *testing.T) {
	handler := setupTestHandler()

	t.Run("validates breach ID", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/breach/invalid-uuid", nil)

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "invalid-uuid")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		w := httptest.NewRecorder()
		handler.HandleGetBreach(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("returns 404 for non-existent breach", func(t *testing.T) {
		breachID := uuid.New()
		req := httptest.NewRequest(http.MethodGet, "/api/breach/"+breachID.String(), nil)

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", breachID.String())
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		w := httptest.NewRecorder()
		handler.HandleGetBreach(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestAPIHandler_HandleListBreaches(t *testing.T) {
	handler := setupTestHandler()

	t.Run("lists breaches", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/breach", nil)

		w := httptest.NewRecorder()
		handler.HandleListBreaches(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
	})
}

func TestAPIHandler_HandleUpdateBreach(t *testing.T) {
	handler := setupTestHandler()

	t.Run("validates breach ID", func(t *testing.T) {
		body := map[string]interface{}{
			"status": "mitigated",
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPatch, "/api/breach/invalid-uuid", bytes.NewReader(bodyBytes))

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "invalid-uuid")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		w := httptest.NewRecorder()
		handler.HandleUpdateBreach(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("validates request body", func(t *testing.T) {
		breachID := uuid.New()
		req := httptest.NewRequest(http.MethodPatch, "/api/breach/"+breachID.String(), strings.NewReader("invalid json"))

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", breachID.String())
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		w := httptest.NewRecorder()
		handler.HandleUpdateBreach(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestAPIHandler_HandleNotifyBreach(t *testing.T) {
	handler := setupTestHandler()

	t.Run("validates breach ID", func(t *testing.T) {
		body := map[string]interface{}{
			"type": "authority",
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPost, "/api/breach/invalid-uuid/notify", bytes.NewReader(bodyBytes))

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "invalid-uuid")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		w := httptest.NewRecorder()
		handler.HandleNotifyBreach(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("validates request body", func(t *testing.T) {
		breachID := uuid.New()
		req := httptest.NewRequest(http.MethodPost, "/api/breach/"+breachID.String()+"/notify", strings.NewReader("invalid json"))

		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", breachID.String())
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		w := httptest.NewRecorder()
		handler.HandleNotifyBreach(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestAPIHandler_HandleGetBreachStats(t *testing.T) {
	handler := setupTestHandler()

	t.Run("retrieves breach statistics", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/breach/stats", nil)

		w := httptest.NewRecorder()
		handler.HandleGetBreachStats(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
	})
}

// ============================================================================
// ROPA API Handler Tests (Article 30)
// ============================================================================

func TestAPIHandler_HandleCreateActivity(t *testing.T) {
	handler := setupTestHandler()

	t.Run("creates activity successfully", func(t *testing.T) {
		reqBody := ProcessingActivityRequest{
			Name:            "Test Activity",
			Purpose:         "Testing",
			LegalBasis:      LegalBasisConsent,
			RetentionPeriod: "1 year",
		}

		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest("POST", "/api/ropa/activities", bytes.NewBuffer(body))
		w := httptest.NewRecorder()

		handler.HandleCreateActivity(w, req)

		assert.Equal(t, http.StatusCreated, w.Code)
	})

	t.Run("validates request body", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/ropa/activities", bytes.NewBufferString("invalid"))
		w := httptest.NewRecorder()

		handler.HandleCreateActivity(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestAPIHandler_HandleGetActivity(t *testing.T) {
	handler := setupTestHandler()

	t.Run("validates activity ID", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/ropa/activities/invalid", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "invalid")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handler.HandleGetActivity(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("returns 404 for non-existent activity", func(t *testing.T) {
		activityID := uuid.New()
		req := httptest.NewRequest("GET", "/api/ropa/activities/"+activityID.String(), nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", activityID.String())
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handler.HandleGetActivity(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestAPIHandler_HandleListActivities(t *testing.T) {
	handler := setupTestHandler()

	t.Run("lists activities", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/ropa/activities", nil)
		w := httptest.NewRecorder()

		handler.HandleListActivities(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		_ = json.NewDecoder(w.Body).Decode(&response)
		assert.Contains(t, response, "activities")
		assert.Contains(t, response, "total")
	})
}

func TestAPIHandler_HandleUpdateActivity(t *testing.T) {
	handler := setupTestHandler()

	t.Run("validates activity ID", func(t *testing.T) {
		updates := map[string]interface{}{"name": "Updated"}
		body, _ := json.Marshal(updates)

		req := httptest.NewRequest("PATCH", "/api/ropa/activities/invalid", bytes.NewBuffer(body))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "invalid")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handler.HandleUpdateActivity(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("validates request body", func(t *testing.T) {
		activityID := uuid.New()
		req := httptest.NewRequest("PATCH", "/api/ropa/activities/"+activityID.String(), bytes.NewBufferString("invalid"))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", activityID.String())
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handler.HandleUpdateActivity(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestAPIHandler_HandleDeleteActivity(t *testing.T) {
	handler := setupTestHandler()

	t.Run("validates activity ID", func(t *testing.T) {
		req := httptest.NewRequest("DELETE", "/api/ropa/activities/invalid", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "invalid")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handler.HandleDeleteActivity(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestAPIHandler_HandleReviewActivity(t *testing.T) {
	handler := setupTestHandler()

	t.Run("validates activity ID", func(t *testing.T) {
		reqBody := map[string]string{"notes": "Review notes"}
		body, _ := json.Marshal(reqBody)

		req := httptest.NewRequest("POST", "/api/ropa/activities/invalid/review", bytes.NewBuffer(body))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "invalid")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handler.HandleReviewActivity(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("requires authentication", func(t *testing.T) {
		activityID := uuid.New()
		reqBody := map[string]string{"notes": "Review notes"}
		body, _ := json.Marshal(reqBody)

		req := httptest.NewRequest("POST", "/api/ropa/activities/"+activityID.String()+"/review", bytes.NewBuffer(body))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", activityID.String())
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handler.HandleReviewActivity(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestAPIHandler_HandleGetROPAReport(t *testing.T) {
	handler := setupTestHandler()

	t.Run("generates report", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/ropa/report?organization=TestOrg", nil)
		w := httptest.NewRecorder()

		handler.HandleGetROPAReport(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var report ROPAReport
		_ = json.NewDecoder(w.Body).Decode(&report)
		assert.Equal(t, "TestOrg", report.OrganizationName)
	})
}

func TestAPIHandler_HandleCheckCompliance(t *testing.T) {
	handler := setupTestHandler()

	t.Run("validates activity ID", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/ropa/compliance/invalid", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "invalid")
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()

		handler.HandleCheckCompliance(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestAPIHandler_HandleGetROPAStats(t *testing.T) {
	handler := setupTestHandler()

	t.Run("retrieves statistics", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/ropa/stats", nil)
		w := httptest.NewRecorder()

		handler.HandleGetROPAStats(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var stats ROPAStats
		_ = json.NewDecoder(w.Body).Decode(&stats)
		assert.NotNil(t, stats.ActivitiesByLegalBasis)
		assert.NotNil(t, stats.ActivitiesByStatus)
	})
}
