package audit

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// AuditAPIHandler handles HTTP requests for audit endpoints
type AuditAPIHandler struct {
	service *AuditService
	logger  *zap.Logger
}

// NewAuditAPIHandler creates a new audit API handler
func NewAuditAPIHandler(service *AuditService, logger *zap.Logger) *AuditAPIHandler {
	return &AuditAPIHandler{
		service: service,
		logger:  logger,
	}
}

// RegisterRoutes registers all audit API routes
func (h *AuditAPIHandler) RegisterRoutes(r chi.Router) {
	r.Route("/api/v1/audit", func(r chi.Router) {
		// Event endpoints
		r.Get("/events", h.SearchEvents)
		r.Get("/events/{id}", h.GetEvent)

		// Statistics endpoints
		r.Get("/stats/overview", h.GetOverviewStats)
		r.Get("/stats/timeseries", h.GetTimeSeries)
		r.Get("/stats/top-users", h.GetTopUsers)
		r.Get("/stats/top-resources", h.GetTopResources)

		// Alert endpoints
		r.Get("/alerts/failed-logins", h.GetFailedLoginAlerts)
		r.Get("/alerts/suspicious", h.GetSuspiciousActivity)
		r.Get("/alerts/summary", h.GetAlertSummary)

		// Forensics endpoints
		r.Get("/forensics/session/{userID}", h.GetUserSession)
		r.Get("/forensics/ip/{ip}", h.GetIPActivity)
		r.Get("/forensics/incident/{userID}", h.GetIncidentReport)

		// Report endpoints
		r.Get("/reports/soc2", h.GetSOC2Report)
		r.Get("/reports/gdpr/{userID}", h.GetGDPRReport)
	})
}

// SearchEvents searches audit events with filters
func (h *AuditAPIHandler) SearchEvents(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filters := &SearchFilters{
		TenantID: query.Get("tenant_id"),
	}

	if userID := query.Get("user_id"); userID != "" {
		uid, err := uuid.Parse(userID)
		if err == nil {
			filters.UserID = &uid
		}
	}

	if eventType := query.Get("event_type"); eventType != "" {
		filters.EventType = EventType(eventType)
	}

	limit, _ := strconv.Atoi(query.Get("limit"))
	if limit == 0 {
		limit = 100
	}

	events, err := h.service.Search(r.Context(), filters, limit, 0)
	if err != nil {
		h.respondError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondJSON(w, http.StatusOK, map[string]interface{}{
		"events": events,
		"count":  len(events),
	})
}

// GetEvent retrieves a single audit event by ID
func (h *AuditAPIHandler) GetEvent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	event, err := h.service.GetEventByID(r.Context(), id)
	if err != nil {
		h.respondError(w, http.StatusNotFound, err)
		return
	}

	h.respondJSON(w, http.StatusOK, event)
}

// GetOverviewStats returns overview statistics
func (h *AuditAPIHandler) GetOverviewStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.service.GetOverviewStats(r.Context())
	if err != nil {
		h.respondError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondJSON(w, http.StatusOK, stats)
}

// GetTimeSeries returns time series data
func (h *AuditAPIHandler) GetTimeSeries(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	start, _ := time.Parse(time.RFC3339, query.Get("start"))
	end, _ := time.Parse(time.RFC3339, query.Get("end"))

	if start.IsZero() {
		start = time.Now().Add(-24 * time.Hour)
	}
	if end.IsZero() {
		end = time.Now()
	}

	series, err := h.service.GetTimeSeriesData(r.Context(), start, end, time.Hour)
	if err != nil {
		h.respondError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondJSON(w, http.StatusOK, series)
}

// GetTopUsers returns most active users
func (h *AuditAPIHandler) GetTopUsers(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 10
	}

	users, err := h.service.GetTopUsers(r.Context(), time.Now().Add(-24*time.Hour), time.Now(), limit)
	if err != nil {
		h.respondError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondJSON(w, http.StatusOK, users)
}

// GetTopResources returns most accessed resources
func (h *AuditAPIHandler) GetTopResources(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 10
	}

	resources, err := h.service.GetTopResources(r.Context(), time.Now().Add(-24*time.Hour), time.Now(), limit)
	if err != nil {
		h.respondError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondJSON(w, http.StatusOK, resources)
}

// GetFailedLoginAlerts returns failed login alerts
func (h *AuditAPIHandler) GetFailedLoginAlerts(w http.ResponseWriter, r *http.Request) {
	threshold, _ := strconv.Atoi(r.URL.Query().Get("threshold"))
	if threshold == 0 {
		threshold = 3
	}

	alerts, err := h.service.DetectFailedLoginAttempts(r.Context(), 1*time.Hour, threshold)
	if err != nil {
		h.respondError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondJSON(w, http.StatusOK, alerts)
}

// GetSuspiciousActivity returns suspicious activity alerts
func (h *AuditAPIHandler) GetSuspiciousActivity(w http.ResponseWriter, r *http.Request) {
	alerts, err := h.service.DetectSuspiciousActivity(r.Context(), 1*time.Hour)
	if err != nil {
		h.respondError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondJSON(w, http.StatusOK, alerts)
}

// GetAlertSummary returns alert summary
func (h *AuditAPIHandler) GetAlertSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := h.service.GetAlertSummary(r.Context(), 24*time.Hour)
	if err != nil {
		h.respondError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondJSON(w, http.StatusOK, summary)
}

// GetUserSession retrieves user session timeline
func (h *AuditAPIHandler) GetUserSession(w http.ResponseWriter, r *http.Request) {
	userIDStr := chi.URLParam(r, "userID")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, err)
		return
	}

	start := time.Now().Add(-24 * time.Hour)
	end := time.Now()

	timeline, err := h.service.ReconstructUserSession(r.Context(), userID, start, end)
	if err != nil {
		h.respondError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondJSON(w, http.StatusOK, timeline)
}

// GetIPActivity retrieves IP activity
func (h *AuditAPIHandler) GetIPActivity(w http.ResponseWriter, r *http.Request) {
	ip := chi.URLParam(r, "ip")

	activity, err := h.service.TraceIPActivity(r.Context(), ip, 24*time.Hour)
	if err != nil {
		h.respondError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondJSON(w, http.StatusOK, activity)
}

// GetIncidentReport generates incident report
func (h *AuditAPIHandler) GetIncidentReport(w http.ResponseWriter, r *http.Request) {
	userIDStr := chi.URLParam(r, "userID")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, err)
		return
	}

	report, err := h.service.GenerateIncidentReport(r.Context(), userID, 24*time.Hour)
	if err != nil {
		h.respondError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondJSON(w, http.StatusOK, report)
}

// GetSOC2Report generates SOC2 report
func (h *AuditAPIHandler) GetSOC2Report(w http.ResponseWriter, r *http.Request) {
	start := time.Now().Add(-30 * 24 * time.Hour)
	end := time.Now()

	report, err := h.service.GenerateSOC2Report(r.Context(), start, end)
	if err != nil {
		h.respondError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondJSON(w, http.StatusOK, report)
}

// GetGDPRReport generates GDPR report
func (h *AuditAPIHandler) GetGDPRReport(w http.ResponseWriter, r *http.Request) {
	userIDStr := chi.URLParam(r, "userID")
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, err)
		return
	}

	start := time.Now().Add(-90 * 24 * time.Hour)
	end := time.Now()

	report, err := h.service.GenerateGDPRReport(r.Context(), userID, start, end)
	if err != nil {
		h.respondError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondJSON(w, http.StatusOK, report)
}

// Helper methods

func (h *AuditAPIHandler) respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		h.logger.Error("failed to encode JSON response", zap.Error(err))
	}
}

func (h *AuditAPIHandler) respondError(w http.ResponseWriter, status int, err error) {
	h.logger.Error("API error", zap.Error(err), zap.Int("status", status))
	h.respondJSON(w, status, map[string]string{
		"error": err.Error(),
	})
}
