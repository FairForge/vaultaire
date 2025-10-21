package compliance

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// APIHandler handles all compliance API requests
type APIHandler struct {
	gdprService        *GDPRService
	portabilityService *PortabilityService
	consentService     *ConsentService
	breachService      *BreachService
	ropaService        *ROPAService
	privacyService     *PrivacyService
	logger             *zap.Logger
}

// NewAPIHandler creates a new compliance API handler
func NewAPIHandler(
	gdprService *GDPRService,
	portabilityService *PortabilityService,
	consentService *ConsentService,
	breachService *BreachService,
	ropaService *ROPAService,
	privacyService *PrivacyService,
	logger *zap.Logger,
) *APIHandler {
	return &APIHandler{
		gdprService:        gdprService,
		portabilityService: portabilityService,
		consentService:     consentService,
		breachService:      breachService,
		ropaService:        ropaService,
		privacyService:     privacyService,
		logger:             logger,
	}
}

// ============================================================================
// GDPR SAR Handlers (Article 15)
// ============================================================================

// HandleCreateSAR handles POST /api/compliance/sar
func (h *APIHandler) HandleCreateSAR(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	sar, err := h.gdprService.CreateSubjectAccessRequest(ctx, userID)
	if err != nil {
		h.logger.Error("failed to create SAR", zap.Error(err))
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(sar)
}

// HandleCreateDeletionRequest handles POST /api/compliance/deletion
func (h *APIHandler) HandleCreateDeletionRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	deletionID, err := h.gdprService.CreateDeletionRequest(ctx, userID, req.Reason, DeletionScopeAll)
	if err != nil {
		h.logger.Error("failed to create deletion request", zap.Error(err))
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":     deletionID,
		"status": "pending",
	})
}

// HandleGetDataInventory handles GET /api/compliance/inventory
func (h *APIHandler) HandleGetDataInventory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	inventory, err := h.gdprService.GetDataInventory(ctx, userID)
	if err != nil {
		h.logger.Error("failed to get data inventory", zap.Error(err))
		http.Error(w, "Failed to get inventory", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(inventory)
}

// HandleListProcessingActivities handles GET /api/compliance/activities
func (h *APIHandler) HandleListProcessingActivities(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	activities, err := h.gdprService.ListProcessingActivities(ctx)
	if err != nil {
		h.logger.Error("failed to list processing activities", zap.Error(err))
		http.Error(w, "Failed to list activities", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(activities)
}

// HandleGetSARStatus handles GET /api/compliance/sar/:id
func (h *APIHandler) HandleGetSARStatus(w http.ResponseWriter, r *http.Request) {
	sarIDStr := chi.URLParam(r, "id")
	if sarIDStr == "" {
		http.Error(w, "SAR ID required", http.StatusBadRequest)
		return
	}

	sarID, err := uuid.Parse(sarIDStr)
	if err != nil {
		http.Error(w, "Invalid SAR ID", http.StatusBadRequest)
		return
	}

	_ = sarID
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":     sarID,
		"status": "pending",
	})
}

// ============================================================================
// Portability Handlers (Article 20)
// ============================================================================

// HandleCreateExport handles POST /api/compliance/export
func (h *APIHandler) HandleCreateExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		Format string `json:"format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Format == "" {
		req.Format = "json"
	}

	exportReq, err := h.portabilityService.CreateExportRequest(ctx, userID, req.Format)
	if err != nil {
		h.logger.Error("failed to create export request", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(exportReq)
}

// HandleGetExport handles GET /api/compliance/export/:id
func (h *APIHandler) HandleGetExport(w http.ResponseWriter, r *http.Request) {
	exportIDStr := chi.URLParam(r, "id")
	if exportIDStr == "" {
		http.Error(w, "Export ID required", http.StatusBadRequest)
		return
	}

	exportID, err := uuid.Parse(exportIDStr)
	if err != nil {
		http.Error(w, "Invalid export ID", http.StatusBadRequest)
		return
	}

	exportReq, err := h.portabilityService.GetExportRequest(r.Context(), exportID)
	if err != nil {
		h.logger.Error("failed to get export request", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(exportReq)
}

// ============================================================================
// Consent Handlers (Articles 7 & 8)
// ============================================================================

// HandleGrantConsent handles POST /api/compliance/consent
func (h *APIHandler) HandleGrantConsent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req ConsentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	req.UserID = userID

	_, err := h.consentService.GrantConsent(ctx, &req)
	if err != nil {
		h.logger.Error("failed to grant consent", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleWithdrawConsent handles DELETE /api/compliance/consent/:purpose
func (h *APIHandler) HandleWithdrawConsent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	purpose := chi.URLParam(r, "purpose")
	if purpose == "" {
		http.Error(w, "Purpose required", http.StatusBadRequest)
		return
	}

	withdrawReq := &ConsentRequest{
		UserID:  userID,
		Purpose: purpose,
		Granted: false,
	}

	err := h.consentService.WithdrawConsent(ctx, userID, purpose, withdrawReq)
	if err != nil {
		h.logger.Error("failed to withdraw consent", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleGetConsentStatus handles GET /api/compliance/consent
func (h *APIHandler) HandleGetConsentStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	status, err := h.consentService.GetConsentStatus(ctx, userID)
	if err != nil {
		h.logger.Error("failed to get consent status", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// HandleCheckConsent handles GET /api/compliance/consent/:purpose
func (h *APIHandler) HandleCheckConsent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	purpose := chi.URLParam(r, "purpose")
	if purpose == "" {
		http.Error(w, "Purpose required", http.StatusBadRequest)
		return
	}

	granted, err := h.consentService.CheckConsent(ctx, userID, purpose)
	if err != nil {
		h.logger.Error("failed to check consent", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"purpose": purpose,
		"granted": granted,
	})
}

// HandleGetConsentHistory handles GET /api/compliance/consent/history
func (h *APIHandler) HandleGetConsentHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	history, err := h.consentService.GetConsentHistory(ctx, userID)
	if err != nil {
		h.logger.Error("failed to get consent history", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(history)
}

// HandleListConsentPurposes handles GET /api/compliance/consent/purposes
func (h *APIHandler) HandleListConsentPurposes(w http.ResponseWriter, r *http.Request) {
	purposes, err := h.consentService.ListPurposes(r.Context())
	if err != nil {
		h.logger.Error("failed to list consent purposes", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(purposes)
}

// ============================================================================
// Breach Notification Handlers (Articles 33 & 34)
// ============================================================================

// HandleReportBreach handles POST /api/compliance/breach
func (h *APIHandler) HandleReportBreach(w http.ResponseWriter, r *http.Request) {
	var req BreachRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	breach, err := h.breachService.DetectBreach(r.Context(), &req)
	if err != nil {
		h.logger.Error("failed to report breach", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(breach)
}

// HandleGetBreach handles GET /api/compliance/breach/:id
func (h *APIHandler) HandleGetBreach(w http.ResponseWriter, r *http.Request) {
	breachIDStr := chi.URLParam(r, "id")
	if breachIDStr == "" {
		http.Error(w, "Breach ID required", http.StatusBadRequest)
		return
	}

	breachID, err := uuid.Parse(breachIDStr)
	if err != nil {
		http.Error(w, "Invalid breach ID", http.StatusBadRequest)
		return
	}

	breach, err := h.breachService.GetBreachStatus(r.Context(), breachID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "Breach not found", http.StatusNotFound)
			return
		}
		h.logger.Error("failed to get breach", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(breach)
}

// HandleListBreaches handles GET /api/compliance/breach
func (h *APIHandler) HandleListBreaches(w http.ResponseWriter, r *http.Request) {
	filters := make(map[string]interface{})

	if severity := r.URL.Query().Get("severity"); severity != "" {
		filters["severity"] = severity
	}
	if status := r.URL.Query().Get("status"); status != "" {
		filters["status"] = status
	}

	breaches, err := h.breachService.ListBreaches(r.Context(), filters)
	if err != nil {
		h.logger.Error("failed to list breaches", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"breaches": breaches,
		"total":    len(breaches),
	})
}

// HandleUpdateBreach handles PATCH /api/compliance/breach/:id
func (h *APIHandler) HandleUpdateBreach(w http.ResponseWriter, r *http.Request) {
	breachIDStr := chi.URLParam(r, "id")
	if breachIDStr == "" {
		http.Error(w, "Breach ID required", http.StatusBadRequest)
		return
	}

	breachID, err := uuid.Parse(breachIDStr)
	if err != nil {
		http.Error(w, "Invalid breach ID", http.StatusBadRequest)
		return
	}

	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.breachService.UpdateBreach(r.Context(), breachID, updates); err != nil {
		h.logger.Error("failed to update breach", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleNotifyBreach handles POST /api/compliance/breach/:id/notify
func (h *APIHandler) HandleNotifyBreach(w http.ResponseWriter, r *http.Request) {
	breachIDStr := chi.URLParam(r, "id")
	if breachIDStr == "" {
		http.Error(w, "Breach ID required", http.StatusBadRequest)
		return
	}

	breachID, err := uuid.Parse(breachIDStr)
	if err != nil {
		http.Error(w, "Invalid breach ID", http.StatusBadRequest)
		return
	}

	var req struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	switch req.Type {
	case "authority":
		if err := h.breachService.NotifyAuthority(r.Context(), breachID); err != nil {
			h.logger.Error("failed to notify authority", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case "subjects":
		if err := h.breachService.NotifySubjects(r.Context(), breachID); err != nil {
			h.logger.Error("failed to notify subjects", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	default:
		http.Error(w, "Invalid notification type", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleGetBreachStats handles GET /api/compliance/breach/stats
func (h *APIHandler) HandleGetBreachStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.breachService.GetBreachStats(r.Context())
	if err != nil {
		h.logger.Error("failed to get breach stats", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stats)
}

// ============================================================================
// ROPA API Handlers (Article 30)
// ============================================================================

// HandleCreateActivity creates a new processing activity
func (h *APIHandler) HandleCreateActivity(w http.ResponseWriter, r *http.Request) {
	var req ProcessingActivityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	activity, err := h.ropaService.CreateActivity(r.Context(), &req)
	if err != nil {
		h.logger.Error("failed to create activity", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(activity)
}

// HandleGetActivity retrieves a processing activity
func (h *APIHandler) HandleGetActivity(w http.ResponseWriter, r *http.Request) {
	activityIDStr := chi.URLParam(r, "id")
	if activityIDStr == "" {
		http.Error(w, "Activity ID required", http.StatusBadRequest)
		return
	}

	activityID, err := uuid.Parse(activityIDStr)
	if err != nil {
		http.Error(w, "Invalid activity ID", http.StatusBadRequest)
		return
	}

	activity, err := h.ropaService.GetActivity(r.Context(), activityID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "Activity not found", http.StatusNotFound)
			return
		}
		h.logger.Error("failed to get activity", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(activity)
}

// HandleListActivities lists all processing activities
func (h *APIHandler) HandleListActivities(w http.ResponseWriter, r *http.Request) {
	filters := make(map[string]interface{})

	if status := r.URL.Query().Get("status"); status != "" {
		filters["status"] = status
	}
	if legalBasis := r.URL.Query().Get("legal_basis"); legalBasis != "" {
		filters["legal_basis"] = legalBasis
	}

	activities, err := h.ropaService.ListActivities(r.Context(), filters)
	if err != nil {
		h.logger.Error("failed to list activities", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"activities": activities,
		"total":      len(activities),
	})
}

// HandleUpdateActivity updates a processing activity
func (h *APIHandler) HandleUpdateActivity(w http.ResponseWriter, r *http.Request) {
	activityIDStr := chi.URLParam(r, "id")
	if activityIDStr == "" {
		http.Error(w, "Activity ID required", http.StatusBadRequest)
		return
	}

	activityID, err := uuid.Parse(activityIDStr)
	if err != nil {
		http.Error(w, "Invalid activity ID", http.StatusBadRequest)
		return
	}

	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.ropaService.UpdateActivity(r.Context(), activityID, updates); err != nil {
		h.logger.Error("failed to update activity", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleDeleteActivity marks an activity as inactive
func (h *APIHandler) HandleDeleteActivity(w http.ResponseWriter, r *http.Request) {
	activityIDStr := chi.URLParam(r, "id")
	if activityIDStr == "" {
		http.Error(w, "Activity ID required", http.StatusBadRequest)
		return
	}

	activityID, err := uuid.Parse(activityIDStr)
	if err != nil {
		http.Error(w, "Invalid activity ID", http.StatusBadRequest)
		return
	}

	if err := h.ropaService.DeleteActivity(r.Context(), activityID); err != nil {
		h.logger.Error("failed to delete activity", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleReviewActivity marks an activity as reviewed
func (h *APIHandler) HandleReviewActivity(w http.ResponseWriter, r *http.Request) {
	activityIDStr := chi.URLParam(r, "id")
	if activityIDStr == "" {
		http.Error(w, "Activity ID required", http.StatusBadRequest)
		return
	}

	activityID, err := uuid.Parse(activityIDStr)
	if err != nil {
		http.Error(w, "Invalid activity ID", http.StatusBadRequest)
		return
	}

	reviewerID, ok := r.Context().Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		Notes string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.ropaService.ReviewActivity(r.Context(), activityID, reviewerID, req.Notes); err != nil {
		h.logger.Error("failed to review activity", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleGetROPAReport generates a complete ROPA report
func (h *APIHandler) HandleGetROPAReport(w http.ResponseWriter, r *http.Request) {
	orgName := r.URL.Query().Get("organization")
	if orgName == "" {
		orgName = "Organization"
	}

	report, err := h.ropaService.GenerateROPAReport(r.Context(), orgName)
	if err != nil {
		h.logger.Error("failed to generate ROPA report", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}

// HandleCheckCompliance checks compliance status of an activity
func (h *APIHandler) HandleCheckCompliance(w http.ResponseWriter, r *http.Request) {
	activityIDStr := chi.URLParam(r, "id")
	if activityIDStr == "" {
		http.Error(w, "Activity ID required", http.StatusBadRequest)
		return
	}

	activityID, err := uuid.Parse(activityIDStr)
	if err != nil {
		http.Error(w, "Invalid activity ID", http.StatusBadRequest)
		return
	}

	check, err := h.ropaService.ValidateCompliance(r.Context(), activityID)
	if err != nil {
		h.logger.Error("failed to check compliance", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(check)
}

// HandleGetROPAStats retrieves ROPA statistics
func (h *APIHandler) HandleGetROPAStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.ropaService.GetROPAStats(r.Context())
	if err != nil {
		h.logger.Error("failed to get ROPA stats", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stats)
}

// ============================================================================
// Privacy Control Handlers (GDPR Article 25 - Data Protection by Design)
// ============================================================================

// HandleEnablePrivacyControl handles POST /api/compliance/privacy/controls
func (h *APIHandler) HandleEnablePrivacyControl(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		Type   string                 `json:"type"`
		Config map[string]interface{} `json:"config"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Validate control type
	controlType := PrivacyControlType(req.Type)
	switch controlType {
	case ControlDataMinimization, ControlPurposeLimitation, ControlAccessControl,
		ControlPseudonymization, ControlEncryption:
		// Valid type
	default:
		http.Error(w, "invalid control type", http.StatusBadRequest)
		return
	}

	if err := h.privacyService.EnableControl(ctx, controlType, req.Config); err != nil {
		h.logger.Error("failed to enable privacy control",
			zap.String("user_id", userID.String()),
			zap.String("type", req.Type),
			zap.Error(err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"message": "privacy control enabled",
		"type":    req.Type,
	})
}

// HandleMinimizeData handles POST /api/compliance/privacy/minimize
func (h *APIHandler) HandleMinimizeData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		Purpose string                 `json:"purpose"`
		Data    map[string]interface{} `json:"data"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	minimized, err := h.privacyService.MinimizeData(ctx, req.Purpose, req.Data)
	if err != nil {
		h.logger.Error("failed to minimize data",
			zap.String("user_id", userID.String()),
			zap.String("purpose", req.Purpose),
			zap.Error(err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"minimized_data": minimized,
		"purpose":        req.Purpose,
	})
}

// HandleCheckPurpose handles GET /api/compliance/privacy/purpose/:dataId/:purpose
func (h *APIHandler) HandleCheckPurpose(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	dataID := chi.URLParam(r, "dataId")
	purpose := chi.URLParam(r, "purpose")

	if dataID == "" || purpose == "" {
		http.Error(w, "missing required parameters", http.StatusBadRequest)
		return
	}

	allowed, err := h.privacyService.CheckPurpose(ctx, dataID, purpose)
	if err != nil {
		h.logger.Error("failed to check purpose",
			zap.String("user_id", userID.String()),
			zap.String("data_id", dataID),
			zap.String("purpose", purpose),
			zap.Error(err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"allowed": allowed,
		"data_id": dataID,
		"purpose": purpose,
	})
}

// HandlePseudonymize handles POST /api/compliance/privacy/pseudonymize
func (h *APIHandler) HandlePseudonymize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := ctx.Value(UserIDKey).(uuid.UUID)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		Data map[string]interface{} `json:"data"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	pseudonymized, mapping, err := h.privacyService.Pseudonymize(ctx, req.Data)
	if err != nil {
		h.logger.Error("failed to pseudonymize data",
			zap.String("user_id", userID.String()),
			zap.Error(err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"pseudonymized": pseudonymized,
		"mapping_count": len(mapping),
		// Don't return the actual mapping for security
	})
}
