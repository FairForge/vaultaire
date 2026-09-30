package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// UserInfo is GET /api/v1/user: the account as stored, not a mock
// (Review R11-02 — profile, preferences, activity and the MFA endpoints
// were hard-coded fixtures that answered 200 without touching anything).
type UserInfo struct {
	ID                  string     `json:"id"`
	Email               string     `json:"email"`
	TenantID            string     `json:"tenant_id"`
	Company             string     `json:"company,omitempty"`
	Role                string     `json:"role,omitempty"`
	Status              string     `json:"status,omitempty"`
	DeletionScheduledAt *time.Time `json:"deletion_scheduled_at,omitempty"`
	Quota               QuotaInfo  `json:"quota"`
	MFAEnabled          bool       `json:"mfa_enabled"`
	CreatedAt           time.Time  `json:"created_at"`
}

// registerUserAPIRoutes sets up the JWT-authenticated account endpoints.
// Quota, usage and presign moved here from their dead-by-auth mounts
// (Review R11-07 / WP-R10-7): the old chains never set the tenant the
// handlers read, so every customer got 401.
func (s *Server) registerUserAPIRoutes() {
	s.router.Route("/api/v1/user", func(r chi.Router) {
		r.Use(s.requireJWT)

		r.Get("/", s.handleGetUserInfo)
		// Same contract and handler as DELETE /api/v1/manage/account: one
		// account-deletion flow (30-day grace, cancel via either API).
		r.Delete("/", s.handleMgmtDeleteAccount)

		r.Get("/apikeys", s.handleListUserAPIKeys)
		r.Post("/apikeys", s.handleCreateUserAPIKey)
		r.Post("/apikeys/{keyId}/rotate", s.handleRotateUserAPIKey)
		r.Delete("/apikeys/{keyId}", s.handleDeleteUserAPIKey)
		r.Post("/apikeys/{keyId}/expire", s.handleSetUserAPIKeyExpiration)
		r.Get("/apikeys/audit", s.handleGetUserAPIKeyAuditLogs)

		r.Get("/quota", s.handleGetQuota)
		r.Get("/quota/history", s.handleGetQuotaHistory)
		r.Get("/usage", s.handleGetUsageStats)
		r.Get("/usage/alerts", s.handleGetUsageAlerts)
		r.Get("/presigned", s.handleGetPresignedURL)
	})
}

// handleGetUserInfo returns the account as persisted.
func (s *Server) handleGetUserInfo(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if userID == "" || tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_credentials", "user or tenant not found in token", "")
		return
	}
	email, _ := r.Context().Value(emailKey).(string)

	info := UserInfo{ID: userID, Email: email, TenantID: tenantID}

	if s.db != nil {
		var (
			company, role, status sql.NullString
			createdAt             sql.NullTime
			deletionAt            sql.NullTime
		)
		err := s.db.QueryRowContext(r.Context(),
			`SELECT email, company, role, status, created_at, deletion_scheduled_at FROM users WHERE id = $1`, userID).
			Scan(&info.Email, &company, &role, &status, &createdAt, &deletionAt)
		if errors.Is(err, sql.ErrNoRows) {
			writeManagementError(w, ErrTypeNotFound, "user_not_found", "user not found", "")
			return
		}
		if err != nil {
			s.logger.Error("user info lookup", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "db_error", "failed to load user", "")
			return
		}
		info.Company = company.String
		info.Role = role.String
		info.Status = status.String
		if createdAt.Valid {
			info.CreatedAt = createdAt.Time
		}
		if deletionAt.Valid {
			t := deletionAt.Time
			info.DeletionScheduledAt = &t
		}
	}

	if s.auth != nil {
		info.MFAEnabled, _ = s.auth.IsMFAEnabled(r.Context(), userID)
	}
	if s.quotaManager != nil {
		used, limit, _ := s.quotaManager.GetUsage(r.Context(), tenantID)
		tier, _ := s.quotaManager.GetTier(r.Context(), tenantID)
		info.Quota = QuotaInfo{
			TenantID:     tenantID,
			StorageUsed:  used,
			StorageLimit: limit,
			Percentage:   usagePercent(used, limit),
			Tier:         tier,
			CanUpgrade:   tier != "enterprise",
		}
	}

	writeJSON(w, http.StatusOK, info)
}

func usagePercent(used, limit int64) float64 {
	if limit <= 0 {
		return 0
	}
	return float64(used) / float64(limit) * 100
}

// handleListUserAPIKeys lists all API keys for a user
func (s *Server) handleListUserAPIKeys(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	keys, err := s.auth.ListAPIKeys(r.Context(), userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(keys); err != nil { // #nosec G117 — ListAPIKeys blanks the secret; it is returned once at creation
		s.logger.Error("failed to encode API keys list", zap.Error(err))
	}
}

// handleCreateUserAPIKey creates a new API key
func (s *Server) handleCreateUserAPIKey(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	var req struct {
		Name        string   `json:"name"`
		Permissions []string `json:"permissions"`
		ExpiryDays  *int     `json:"expiry_days"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// The requested scope goes INTO the key (R5-13): it used to be echoed in
	// the response while the stored key stayed full-access.
	var opts *auth.KeyCreateOptions
	if len(req.Permissions) > 0 || (req.ExpiryDays != nil && *req.ExpiryDays > 0) {
		opts = &auth.KeyCreateOptions{}
		if len(req.Permissions) > 0 {
			if err := auth.ValidatePermissions(req.Permissions); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			opts.Permissions = req.Permissions
		}
		if req.ExpiryDays != nil && *req.ExpiryDays > 0 {
			expiresAt := time.Now().AddDate(0, 0, *req.ExpiryDays)
			opts.ExpiresAt = &expiresAt
		}
	}

	key, err := s.auth.GenerateAPIKey(r.Context(), userID, req.Name, opts)
	if err != nil {
		if errors.Is(err, auth.ErrKeyLimitReached) {
			http.Error(w, "API key limit reached for this plan; revoke a key or upgrade", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return with secret (only time it's shown)
	response := map[string]interface{}{
		"id":          key.ID,
		"name":        key.Name,
		"key":         key.Key,
		"secret":      key.Secret,
		"permissions": key.Permissions,
		"expires_at":  key.ExpiresAt,
		"created_at":  key.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.Error("failed to encode create key response", zap.Error(err))
	}
}

// handleRotateUserAPIKey rotates an API key
func (s *Server) handleRotateUserAPIKey(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	keyID := chi.URLParam(r, "keyId")

	newKey, err := s.auth.RotateAPIKey(r.Context(), userID, keyID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := map[string]interface{}{
		"id":     newKey.ID,
		"name":   newKey.Name,
		"key":    newKey.Key,
		"secret": newKey.Secret,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.Error("failed to encode rotate key response", zap.Error(err))
	}
}

// handleDeleteUserAPIKey revokes an API key. The audit row is written by
// the auth service (one site for the dashboard, this API and the
// management API — Review R11-09).
func (s *Server) handleDeleteUserAPIKey(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	keyID := chi.URLParam(r, "keyId")

	if err := s.auth.RevokeAPIKey(r.Context(), userID, keyID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
	s.logger.Info("API key deleted",
		zap.String("user_id", userID),
		zap.String("key_id", keyID))
}

// handleSetUserAPIKeyExpiration sets expiration for an API key
func (s *Server) handleSetUserAPIKeyExpiration(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	keyID := chi.URLParam(r, "keyId")

	var req struct {
		Days int `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	expiresAt := time.Now().AddDate(0, 0, req.Days)
	if err := s.auth.SetAPIKeyExpiration(r.Context(), userID, keyID, expiresAt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]string{
		"message":    "Expiration set successfully",
		"expires_at": expiresAt.Format(time.RFC3339),
	}); err != nil {
		s.logger.Error("failed to encode expiration response", zap.Error(err))
	}
}

// handleGetUserAPIKeyAuditLogs lists the caller's own key events from the
// persisted audit trail (it used to read a per-process in-memory list).
func (s *Server) handleGetUserAPIKeyAuditLogs(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	f := audit.Filter{UserID: userID, EventType: "key", Cursor: r.URL.Query().Get("cursor"), Limit: 100}
	if a := r.URL.Query().Get("action"); a != "" {
		f.Action = a
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			f.Limit = n
		}
	}
	page, err := audit.List(r.Context(), s.db, f)
	if err != nil {
		if errors.Is(err, audit.ErrBadCursor) {
			writeManagementError(w, ErrTypeInvalidRequest, "invalid_cursor", "cursor is not one this endpoint issued", "cursor")
			return
		}
		s.logger.Error("user key audit list", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "db_error", "failed to list audit rows", "")
		return
	}
	items := make([]interface{}, len(page.Rows))
	for i, row := range page.Rows {
		items[i] = row
	}
	writeListResponse(w, items, page.HasMore, page.NextCursor, len(items))
}
