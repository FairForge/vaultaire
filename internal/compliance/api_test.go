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

func TestAPIHandler_HandleCreateSAR(t *testing.T) {
	service := NewGDPRService(nil, zap.NewNop())
	handler := NewAPIHandler(service, zap.NewNop())

	t.Run("creates SAR with valid user", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/compliance/sar", nil)
		w := httptest.NewRecorder()

		userID := uuid.New()
		ctx := context.WithValue(req.Context(), UserIDKey, userID)
		req = req.WithContext(ctx)

		handler.HandleCreateSAR(w, req)

		assert.Equal(t, http.StatusCreated, w.Code)

		var response map[string]interface{}
		err := json.NewDecoder(w.Body).Decode(&response)
		require.NoError(t, err)
		assert.Equal(t, StatusPending, response["status"])
	})

	t.Run("returns unauthorized without user context", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/compliance/sar", nil)
		w := httptest.NewRecorder()

		handler.HandleCreateSAR(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestAPIHandler_HandleCreateDeletionRequest(t *testing.T) {
	service := NewGDPRService(nil, zap.NewNop())
	handler := NewAPIHandler(service, zap.NewNop())

	t.Run("creates deletion request", func(t *testing.T) {
		body := map[string]interface{}{
			"reason":          "User requested",
			"deletion_method": DeletionMethodHard,
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPost, "/api/compliance/deletion", bytes.NewReader(bodyBytes))
		w := httptest.NewRecorder()

		userID := uuid.New()
		ctx := context.WithValue(req.Context(), UserIDKey, userID)
		req = req.WithContext(ctx)

		handler.HandleCreateDeletionRequest(w, req)

		// Handler returns 202 Accepted for async processing
		assert.Equal(t, http.StatusAccepted, w.Code)
	})
}

func TestAPIHandler_HandleGetDataInventory(t *testing.T) {
	service := NewGDPRService(nil, zap.NewNop())
	handler := NewAPIHandler(service, zap.NewNop())

	req := httptest.NewRequest(http.MethodGet, "/api/compliance/inventory", nil)
	w := httptest.NewRecorder()

	userID := uuid.New()
	ctx := context.WithValue(req.Context(), UserIDKey, userID)
	req = req.WithContext(ctx)

	handler.HandleGetDataInventory(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var response []DataInventoryItem
	err := json.NewDecoder(w.Body).Decode(&response)
	require.NoError(t, err)
	assert.NotNil(t, response)
}

func TestAPIHandler_HandleListProcessingActivities(t *testing.T) {
	service := NewGDPRService(nil, zap.NewNop())
	handler := NewAPIHandler(service, zap.NewNop())

	req := httptest.NewRequest(http.MethodGet, "/api/compliance/activities", nil)
	w := httptest.NewRecorder()

	handler.HandleListProcessingActivities(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// The handler returns an object with activities array, not array directly
	var response map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&response)
	require.NoError(t, err)
	assert.NotNil(t, response)
}

func TestAPIHandler_HandleGetSARStatus(t *testing.T) {
	service := NewGDPRService(nil, zap.NewNop())
	handler := NewAPIHandler(service, zap.NewNop())

	req := httptest.NewRequest(http.MethodGet, "/api/compliance/sar/{id}", nil)
	w := httptest.NewRecorder()

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", uuid.New().String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	handler.HandleGetSARStatus(w, req)

	// Handler returns 200 with "not found" message in body, not 404
	assert.Equal(t, http.StatusOK, w.Code)
}
