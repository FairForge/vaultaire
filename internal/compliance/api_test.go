package compliance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestAPIHandler_CreateSAR(t *testing.T) {
	service := NewGDPRService(nil, zap.NewNop())
	handler := NewAPIHandler(service, zap.NewNop())

	t.Run("creates SAR with valid user", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/compliance/sar", nil)
		ctx := context.WithValue(req.Context(), UserIDKey, uuid.New())
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.HandleCreateSAR(w, req)

		assert.Equal(t, http.StatusCreated, w.Code)

		var resp map[string]interface{}
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Equal(t, StatusPending, resp["status"])
	})

	t.Run("returns unauthorized without user context", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/compliance/sar", nil)
		w := httptest.NewRecorder()

		handler.HandleCreateSAR(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestAPIHandler_CreateDeletionRequest(t *testing.T) {
	service := NewGDPRService(nil, zap.NewNop())
	handler := NewAPIHandler(service, zap.NewNop())

	t.Run("creates deletion request", func(t *testing.T) {
		body := map[string]string{"method": DeletionMethodHard}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodDelete, "/api/compliance/user-data", bytes.NewReader(bodyBytes))
		ctx := context.WithValue(req.Context(), UserIDKey, uuid.New())
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.HandleCreateDeletionRequest(w, req)

		assert.Equal(t, http.StatusAccepted, w.Code)

		var resp map[string]interface{}
		err := json.NewDecoder(w.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Equal(t, StatusPending, resp["status"])
	})
}

func TestAPIHandler_GetDataInventory(t *testing.T) {
	service := NewGDPRService(nil, zap.NewNop())
	handler := NewAPIHandler(service, zap.NewNop())

	req := httptest.NewRequest(http.MethodGet, "/api/compliance/data-inventory", nil)
	ctx := context.WithValue(req.Context(), UserIDKey, uuid.New())
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleGetDataInventory(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)
	assert.Contains(t, resp, "profile")
	assert.Contains(t, resp, "files")
}

func TestAPIHandler_ListProcessingActivities(t *testing.T) {
	service := NewGDPRService(nil, zap.NewNop())
	handler := NewAPIHandler(service, zap.NewNop())

	req := httptest.NewRequest(http.MethodGet, "/api/compliance/processing-activities", nil)
	w := httptest.NewRecorder()

	handler.HandleListProcessingActivities(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)
	assert.Contains(t, resp, "activities")
	assert.GreaterOrEqual(t, int(resp["count"].(float64)), 1)
}

func TestAPIHandler_GetSARStatus(t *testing.T) {
	service := NewGDPRService(nil, zap.NewNop())
	handler := NewAPIHandler(service, zap.NewNop())

	sarID := uuid.New()

	// Create router with chi for URL params
	r := chi.NewRouter()
	r.Get("/api/compliance/sar/{id}", handler.HandleGetSARStatus)

	req := httptest.NewRequest(http.MethodGet, "/api/compliance/sar/"+sarID.String(), nil)
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}
