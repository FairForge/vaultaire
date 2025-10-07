package compliance

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// APIHandler provides HTTP handlers for GDPR compliance
type APIHandler struct {
	service *GDPRService
	logger  *zap.Logger
}

// NewAPIHandler creates a new GDPR API handler
func NewAPIHandler(service *GDPRService, logger *zap.Logger) *APIHandler {
	return &APIHandler{
		service: service,
		logger:  logger,
	}
}

// HandleCreateSAR handles POST /api/compliance/sar
func (h *APIHandler) HandleCreateSAR(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get user ID from context (set by auth middleware)
	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sar, err := h.service.CreateSubjectAccessRequest(ctx, userID)
	if err != nil {
		h.logger.Error("failed to create SAR", zap.Error(err))
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"id":           sar.ID,
		"status":       sar.Status,
		"request_date": sar.RequestDate,
		"message":      "Subject access request created. You will receive your data within 30 days.",
	}); err != nil {
		h.logger.Error("failed to encode response", zap.Error(err))
	}
}

// HandleGetSARStatus handles GET /api/compliance/sar/:id
func (h *APIHandler) HandleGetSARStatus(w http.ResponseWriter, r *http.Request) {
	sarID := chi.URLParam(r, "id")
	id, err := uuid.Parse(sarID)
	if err != nil {
		http.Error(w, "invalid SAR ID", http.StatusBadRequest)
		return
	}

	// TODO: Query database for SAR status
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"id":     id,
		"status": StatusPending,
	}); err != nil {
		h.logger.Error("failed to encode response", zap.Error(err))
	}
}

// HandleCreateDeletionRequest handles DELETE /api/compliance/user-data
func (h *APIHandler) HandleCreateDeletionRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Get email from context or database
	userEmail := "user@example.com" // TODO: Get from user service

	// Parse request body for deletion method
	var reqBody struct {
		Method string `json:"method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		reqBody.Method = DeletionMethodHard // Default
	}

	req, err := h.service.CreateDeletionRequest(ctx, userID, userEmail, reqBody.Method)
	if err != nil {
		h.logger.Error("failed to create deletion request", zap.Error(err))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"id":           req.ID,
		"status":       req.Status,
		"request_date": req.RequestDate,
		"message":      "Deletion request accepted. Your data will be deleted within 30 days.",
	}); err != nil {
		h.logger.Error("failed to encode response", zap.Error(err))
	}
}

// HandleGetDataInventory handles GET /api/compliance/data-inventory
func (h *APIHandler) HandleGetDataInventory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	inventory, err := h.service.GetDataInventory(ctx, userID)
	if err != nil {
		h.logger.Error("failed to get data inventory", zap.Error(err))
		http.Error(w, "failed to retrieve inventory", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(inventory); err != nil {
		h.logger.Error("failed to encode response", zap.Error(err))
	}
}

// HandleListProcessingActivities handles GET /api/compliance/processing-activities
func (h *APIHandler) HandleListProcessingActivities(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	activities, err := h.service.ListProcessingActivities(ctx)
	if err != nil {
		h.logger.Error("failed to list processing activities", zap.Error(err))
		http.Error(w, "failed to retrieve activities", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"activities": activities,
		"count":      len(activities),
	}); err != nil {
		h.logger.Error("failed to encode response", zap.Error(err))
	}
}
