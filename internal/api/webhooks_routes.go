package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"go.uber.org/zap"
)

func (s *Server) registerWebhookRoutes() {
	rl := NewManagementRateLimiter()
	im := newIdempotencyMiddleware(s.db, s.logger)

	s.router.Route("/api/v1/webhooks", func(r chi.Router) {
		r.Use(s.requireJWT)
		r.Use(rl.Middleware)
		r.Use(im.Middleware)

		r.Post("/", s.handleCreateWebhook)
		r.Get("/", s.handleListWebhooks)
		r.Patch("/{id}", s.handleUpdateWebhook)
		r.Delete("/{id}", s.handleDeleteWebhook)
		r.Get("/{id}/deliveries", s.handleListDeliveries)
		r.Post("/{id}/test", s.handleTestWebhook)
	})

	s.router.Route("/api/v1/events", func(r chi.Router) {
		r.Use(s.requireJWT)
		r.Use(rl.Middleware)

		r.Get("/", s.handleListEvents)
	})
}

type createWebhookRequest struct {
	URL     string   `json:"url"`
	Events  []string `json:"events"`
	Enabled *bool    `json:"enabled,omitempty"`
}

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}

	var req createWebhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}

	if req.URL == "" {
		writeManagementError(w, ErrTypeInvalidRequest, "missing_url", "url is required", "url")
		return
	}
	if _, err := url.ParseRequestURI(req.URL); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_url", "url must be a valid URL", "url")
		return
	}
	if len(req.Events) == 0 {
		writeManagementError(w, ErrTypeInvalidRequest, "missing_events", "events is required and must not be empty", "events")
		return
	}
	if !isValidEventFilter(req.Events) {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_events", "events contains invalid event types", "events")
		return
	}

	id := uuid.New().String()
	secret, err := generateWebhookSecret()
	if err != nil {
		s.logger.Error("generate webhook secret", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to generate webhook secret", "")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	now := time.Now().UTC()
	_, err = s.db.ExecContext(r.Context(), `
		INSERT INTO webhook_endpoints (id, tenant_id, url, event_filter, secret, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, tenantID, req.URL, pq.Array(req.Events), secret, enabled, now, now)
	if err != nil {
		s.logger.Error("create webhook endpoint", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to create webhook", "")
		return
	}

	resp := map[string]interface{}{
		"object":     "webhook",
		"id":         id,
		"url":        req.URL,
		"events":     req.Events,
		"secret":     secret,
		"enabled":    enabled,
		"created_at": now.Format(time.RFC3339),
		"request_id": getRequestID(w),
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}

	if s.db == nil {
		writeListResponse(w, nil, false, "", 0)
		return
	}

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}
	startingAfter := r.URL.Query().Get("starting_after")

	var rows *sql.Rows
	var dbErr error
	if startingAfter != "" {
		rows, dbErr = s.db.QueryContext(r.Context(), `
			SELECT id, url, event_filter, enabled, created_at, updated_at
			FROM webhook_endpoints
			WHERE tenant_id = $1 AND id > $2
			ORDER BY id LIMIT $3`,
			tenantID, startingAfter, limit+1)
	} else {
		rows, dbErr = s.db.QueryContext(r.Context(), `
			SELECT id, url, event_filter, enabled, created_at, updated_at
			FROM webhook_endpoints
			WHERE tenant_id = $1
			ORDER BY id LIMIT $2`,
			tenantID, limit+1)
	}
	if dbErr != nil {
		s.logger.Error("list webhooks", zap.Error(dbErr))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list webhooks", "")
		return
	}
	defer func() { _ = rows.Close() }()

	var items []interface{}
	for rows.Next() {
		var id string
		var whURL string
		var filter []string
		var enabled bool
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&id, &whURL, pq.Array(&filter), &enabled, &createdAt, &updatedAt); err != nil {
			s.logger.Error("scan webhook row", zap.Error(err))
			continue
		}
		items = append(items, map[string]interface{}{
			"object":     "webhook",
			"id":         id,
			"url":        whURL,
			"events":     filter,
			"enabled":    enabled,
			"created_at": createdAt.Format(time.RFC3339),
			"updated_at": updatedAt.Format(time.RFC3339),
		})
	}

	hasMore := len(items) > limit
	nextCursor := ""
	if hasMore {
		items = items[:limit]
		last := items[limit-1].(map[string]interface{})
		nextCursor = last["id"].(string)
	}

	var total int
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM webhook_endpoints WHERE tenant_id = $1`,
		tenantID).Scan(&total)

	writeListResponse(w, items, hasMore, nextCursor, total)
}

type updateWebhookRequest struct {
	URL     *string  `json:"url,omitempty"`
	Events  []string `json:"events,omitempty"`
	Enabled *bool    `json:"enabled,omitempty"`
}

func (s *Server) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}

	webhookID := chi.URLParam(r, "id")

	var req updateWebhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}

	if req.URL != nil && *req.URL == "" {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_url", "url must not be empty", "url")
		return
	}
	if req.URL != nil {
		if _, err := url.ParseRequestURI(*req.URL); err != nil {
			writeManagementError(w, ErrTypeInvalidRequest, "invalid_url", "url must be a valid URL", "url")
			return
		}
	}
	if req.Events != nil && len(req.Events) == 0 {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_events", "events must not be empty", "events")
		return
	}
	if req.Events != nil && !isValidEventFilter(req.Events) {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_events", "events contains invalid event types", "events")
		return
	}

	var currentURL string
	var currentFilter []string
	var currentEnabled bool
	var createdAt time.Time
	err := s.db.QueryRowContext(r.Context(), `
		SELECT url, event_filter, enabled, created_at
		FROM webhook_endpoints
		WHERE id = $1 AND tenant_id = $2`,
		webhookID, tenantID).Scan(&currentURL, pq.Array(&currentFilter), &currentEnabled, &createdAt)
	if err == sql.ErrNoRows {
		writeManagementError(w, ErrTypeNotFound, "webhook_not_found", "webhook not found", "")
		return
	}
	if err != nil {
		s.logger.Error("get webhook for update", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to get webhook", "")
		return
	}

	if req.URL != nil {
		currentURL = *req.URL
	}
	if req.Events != nil {
		currentFilter = req.Events
	}
	if req.Enabled != nil {
		currentEnabled = *req.Enabled
	}

	now := time.Now().UTC()
	_, err = s.db.ExecContext(r.Context(), `
		UPDATE webhook_endpoints
		SET url = $1, event_filter = $2, enabled = $3, updated_at = $4
		WHERE id = $5 AND tenant_id = $6`,
		currentURL, pq.Array(currentFilter), currentEnabled, now, webhookID, tenantID)
	if err != nil {
		s.logger.Error("update webhook", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to update webhook", "")
		return
	}

	resp := map[string]interface{}{
		"object":     "webhook",
		"id":         webhookID,
		"url":        currentURL,
		"events":     currentFilter,
		"enabled":    currentEnabled,
		"created_at": createdAt.Format(time.RFC3339),
		"updated_at": now.Format(time.RFC3339),
		"request_id": getRequestID(w),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}

	webhookID := chi.URLParam(r, "id")

	result, err := s.db.ExecContext(r.Context(), `
		DELETE FROM webhook_endpoints
		WHERE id = $1 AND tenant_id = $2`,
		webhookID, tenantID)
	if err != nil {
		s.logger.Error("delete webhook", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to delete webhook", "")
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		writeManagementError(w, ErrTypeNotFound, "webhook_not_found", "webhook not found", "")
		return
	}

	resp := map[string]interface{}{
		"object":     "webhook",
		"id":         webhookID,
		"deleted":    true,
		"request_id": getRequestID(w),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}

	webhookID := chi.URLParam(r, "id")

	var exists bool
	err := s.db.QueryRowContext(r.Context(), `
		SELECT EXISTS(SELECT 1 FROM webhook_endpoints WHERE id = $1 AND tenant_id = $2)`,
		webhookID, tenantID).Scan(&exists)
	if err != nil || !exists {
		writeManagementError(w, ErrTypeNotFound, "webhook_not_found", "webhook not found", "")
		return
	}

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}
	cursor := r.URL.Query().Get("cursor")

	var rows *sql.Rows
	var dbErr error
	if cursor != "" {
		rows, dbErr = s.db.QueryContext(r.Context(), `
			SELECT id, event_id, status, response_code, latency_ms, retry_count, created_at
			FROM webhook_deliveries
			WHERE webhook_id = $1 AND created_at < $2
			ORDER BY created_at DESC LIMIT $3`,
			webhookID, cursor, limit+1)
	} else {
		rows, dbErr = s.db.QueryContext(r.Context(), `
			SELECT id, event_id, status, response_code, latency_ms, retry_count, created_at
			FROM webhook_deliveries
			WHERE webhook_id = $1
			ORDER BY created_at DESC LIMIT $2`,
			webhookID, limit+1)
	}
	if dbErr != nil {
		s.logger.Error("list deliveries", zap.Error(dbErr))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list deliveries", "")
		return
	}
	defer func() { _ = rows.Close() }()

	var items []interface{}
	for rows.Next() {
		var id, eventID, status string
		var responseCode, latencyMs, retryCount int
		var createdAt time.Time
		if err := rows.Scan(&id, &eventID, &status, &responseCode, &latencyMs, &retryCount, &createdAt); err != nil {
			s.logger.Error("scan delivery row", zap.Error(err))
			continue
		}
		items = append(items, map[string]interface{}{
			"object":        "webhook_delivery",
			"id":            id,
			"event_id":      eventID,
			"status":        status,
			"response_code": responseCode,
			"latency_ms":    latencyMs,
			"retry_count":   retryCount,
			"created_at":    createdAt.Format(time.RFC3339),
		})
	}

	hasMore := len(items) > limit
	nextCursor := ""
	if hasMore {
		items = items[:limit]
		last := items[limit-1].(map[string]interface{})
		nextCursor = last["created_at"].(string)
	}

	var total int
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = $1`,
		webhookID).Scan(&total)

	writeListResponse(w, items, hasMore, nextCursor, total)
}

func (s *Server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}

	webhookID := chi.URLParam(r, "id")

	var exists bool
	err := s.db.QueryRowContext(r.Context(), `
		SELECT EXISTS(SELECT 1 FROM webhook_endpoints WHERE id = $1 AND tenant_id = $2)`,
		webhookID, tenantID).Scan(&exists)
	if err != nil || !exists {
		writeManagementError(w, ErrTypeNotFound, "webhook_not_found", "webhook not found", "")
		return
	}

	eventID := uuid.New().String()
	data := map[string]interface{}{"webhook_id": webhookID}
	dataJSON, _ := json.Marshal(data)

	_, err = s.db.ExecContext(r.Context(), `
		INSERT INTO events (id, type, tenant_id, data)
		VALUES ($1, $2, $3, $4)`,
		eventID, "webhook.test", tenantID, dataJSON)
	if err != nil {
		s.logger.Error("insert test event", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to create test event", "")
		return
	}

	go dispatchWebhooks(s.db, s.logger, eventID, "webhook.test", tenantID, dataJSON)

	resp := map[string]interface{}{
		"object":     "webhook_test",
		"event_id":   eventID,
		"request_id": getRequestID(w),
	}
	writeJSON(w, http.StatusOK, resp)
}
