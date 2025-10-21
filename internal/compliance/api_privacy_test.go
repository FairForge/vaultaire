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
	"go.uber.org/zap"
)

var testUserID = uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")

func TestAPIHandler_HandleEnablePrivacyControl(t *testing.T) {
	db := NewMockPrivacyDatabase()
	privacyService := NewPrivacyService(db)
	handler := &APIHandler{
		privacyService: privacyService,
		logger:         zap.NewNop(),
	}

	tests := []struct {
		name       string
		userID     *uuid.UUID
		body       interface{}
		wantStatus int
	}{
		{
			name:   "valid request",
			userID: &testUserID,
			body: map[string]interface{}{
				"type":   "data_minimization",
				"config": map[string]interface{}{"level": "strict"},
			},
			wantStatus: http.StatusCreated,
		},
		{
			name:       "missing auth",
			userID:     nil,
			body:       map[string]interface{}{"type": "data_minimization"},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:   "invalid control type",
			userID: &testUserID,
			body: map[string]interface{}{
				"type": "invalid_type",
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest("POST", "/api/compliance/privacy/controls", bytes.NewReader(body))

			if tt.userID != nil {
				ctx := context.WithValue(req.Context(), UserIDKey, *tt.userID)
				req = req.WithContext(ctx)
			}

			w := httptest.NewRecorder()
			handler.HandleEnablePrivacyControl(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("got status %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}

func TestAPIHandler_HandleMinimizeData(t *testing.T) {
	db := NewMockPrivacyDatabase()
	privacyService := NewPrivacyService(db)

	// Setup minimization policy
	policy := &DataMinimizationPolicy{
		Purpose:      "analytics",
		RequiredData: []string{"user_id"},
		Active:       true,
	}
	_ = db.CreateMinimizationPolicy(context.Background(), policy)

	handler := &APIHandler{
		privacyService: privacyService,
		logger:         zap.NewNop(),
	}

	reqBody := map[string]interface{}{
		"purpose": "analytics",
		"data": map[string]interface{}{
			"user_id": "123",
			"email":   "test@example.com",
			"ssn":     "123-45-6789",
		},
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/api/compliance/privacy/minimize", bytes.NewReader(body))
	ctx := context.WithValue(req.Context(), UserIDKey, testUserID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandleMinimizeData(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&resp)

	minimized := resp["minimized_data"].(map[string]interface{})
	if _, ok := minimized["email"]; ok {
		t.Error("email should be removed")
	}
	if _, ok := minimized["user_id"]; !ok {
		t.Error("user_id should be present")
	}
}

func TestAPIHandler_HandleCheckPurpose(t *testing.T) {
	db := NewMockPrivacyDatabase()
	privacyService := NewPrivacyService(db)

	// Create binding
	binding := &PurposeBinding{
		DataID:      "file123",
		Purpose:     "backup",
		LawfulBasis: "consent",
	}
	_ = db.CreatePurposeBinding(context.Background(), binding)

	handler := &APIHandler{
		privacyService: privacyService,
		logger:         zap.NewNop(),
	}

	// Test with router for URL params
	router := chi.NewRouter()
	router.Get("/api/compliance/privacy/purpose/{dataId}/{purpose}", handler.HandleCheckPurpose)

	req := httptest.NewRequest("GET", "/api/compliance/privacy/purpose/file123/backup", nil)
	ctx := context.WithValue(req.Context(), UserIDKey, testUserID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if !resp["allowed"].(bool) {
		t.Error("expected purpose to be allowed")
	}
}

func TestAPIHandler_HandlePseudonymize(t *testing.T) {
	db := NewMockPrivacyDatabase()
	privacyService := NewPrivacyService(db)
	handler := &APIHandler{
		privacyService: privacyService,
		logger:         zap.NewNop(),
	}

	reqBody := map[string]interface{}{
		"data": map[string]interface{}{
			"email": "test@example.com",
			"name":  "John Doe",
			"age":   30,
		},
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest("POST", "/api/compliance/privacy/pseudonymize", bytes.NewReader(body))
	ctx := context.WithValue(req.Context(), UserIDKey, testUserID)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.HandlePseudonymize(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if resp["mapping_count"].(float64) != 2 {
		t.Errorf("expected 2 mappings, got %v", resp["mapping_count"])
	}
}
