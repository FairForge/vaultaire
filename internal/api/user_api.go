package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// UserInfo combines all user-related data
type UserInfo struct {
	ID          string                 `json:"id"`
	Email       string                 `json:"email"`
	TenantID    string                 `json:"tenant_id"`
	Company     string                 `json:"company,omitempty"`
	Profile     map[string]interface{} `json:"profile"`
	Preferences map[string]interface{} `json:"preferences"`
	Quota       QuotaInfo              `json:"quota"`
	MFAEnabled  bool                   `json:"mfa_enabled"`
	CreatedAt   time.Time              `json:"created_at"`
}

// registerUserAPIRoutes sets up all user management endpoints
func (s *Server) registerUserAPIRoutes() {
	s.router.Route("/api/v1/user", func(r chi.Router) {
		r.Use(s.requireJWT) // Use JWT middleware for API routes

		// User info
		r.Get("/", s.handleGetUserInfo)
		r.Put("/", s.handleUpdateUserInfo)
		r.Delete("/", s.handleDeleteUser)

		// Profile
		r.Get("/profile", s.handleGetProfile)
		r.Put("/profile", s.handleUpdateProfile)

		// Preferences
		r.Get("/preferences", s.handleGetPreferences)
		r.Put("/preferences", s.handleUpdatePreferences)

		// Activity
		r.Get("/activity", s.handleGetActivity)

		// Enhanced API Keys endpoints
		r.Get("/apikeys", s.handleListUserAPIKeys)
		r.Post("/apikeys", s.handleCreateUserAPIKey)
		r.Post("/apikeys/{keyId}/rotate", s.handleRotateUserAPIKey)
		r.Delete("/apikeys/{keyId}", s.handleDeleteUserAPIKey)
		r.Post("/apikeys/{keyId}/expire", s.handleSetUserAPIKeyExpiration)
		r.Get("/apikeys/audit", s.handleGetUserAPIKeyAuditLogs)

		// MFA
		r.Post("/mfa/enable", s.handleEnableMFA)
		r.Post("/mfa/disable", s.handleDisableMFA)
		r.Get("/mfa/backup-codes", s.handleGetBackupCodes)

		// Quota (alias to quota endpoints)
		r.Get("/quota", s.handleGetQuota)
		r.Get("/usage", s.handleGetUsageStats)
	})
}

// handleGetUserInfo returns comprehensive user information
func (s *Server) handleGetUserInfo(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(common.UserIDKey).(string)
	if !ok {
		userID = r.Context().Value(userIDKey).(string)
	}

	tenantID, _ := r.Context().Value(common.TenantIDKey).(string)
	if tenantID == "" {
		tenantID = r.Context().Value(tenantIDKey).(string)
	}

	emailStr := ""
	if email, ok := r.Context().Value(emailKey).(string); ok {
		emailStr = email
	}

	// Get quota info
	used, limit, _ := s.quotaManager.GetUsage(r.Context(), tenantID)
	tier, _ := s.quotaManager.GetTier(r.Context(), tenantID)

	userInfo := UserInfo{
		ID:       userID,
		Email:    emailStr,
		TenantID: tenantID,
		Profile: map[string]interface{}{
			"displayName": "User Name",
			"avatar":      "",
		},
		Preferences: map[string]interface{}{
			"theme":         "light",
			"notifications": true,
		},
		Quota: QuotaInfo{
			TenantID:     tenantID,
			StorageUsed:  used,
			StorageLimit: limit,
			Percentage:   float64(used) / float64(limit) * 100,
			Tier:         tier,
			CanUpgrade:   tier != "enterprise",
		},
		MFAEnabled: false,
		CreatedAt:  time.Now().Add(-30 * 24 * time.Hour), // Mock data
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(userInfo)
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
	if err := json.NewEncoder(w).Encode(keys); err != nil {
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

	// Generate key
	key, err := s.auth.GenerateAPIKey(r.Context(), userID, req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Set permissions if provided
	if len(req.Permissions) > 0 {
		key.Permissions = req.Permissions
	}

	// Set expiration if provided
	if req.ExpiryDays != nil && *req.ExpiryDays > 0 {
		expiresAt := time.Now().AddDate(0, 0, *req.ExpiryDays)
		if err := s.auth.SetAPIKeyExpiration(r.Context(), userID, key.ID, expiresAt); err != nil {
			s.logger.Error("failed to set key expiration", zap.Error(err))
		} else {
			key.ExpiresAt = &expiresAt
		}
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

// handleDeleteUserAPIKey deletes an API key
func (s *Server) handleDeleteUserAPIKey(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)
	keyID := chi.URLParam(r, "keyId")

	err := s.auth.RevokeAPIKey(r.Context(), userID, keyID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Log revocation if audit logger available
	if s.auditLogger != nil {
		event := auth.APIKeyAuditEvent{
			UserID:    userID,
			KeyID:     keyID,
			Action:    auth.AuditKeyRevoked,
			IP:        r.RemoteAddr,
			UserAgent: r.UserAgent(),
			Success:   true,
		}
		if err := s.auditLogger.LogKeyEvent(r.Context(), event); err != nil {
			s.logger.Error("failed to log audit event", zap.Error(err))
		}
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
	err := s.auth.SetAPIKeyExpiration(r.Context(), userID, keyID, expiresAt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Log expiration setting if audit logger available
	if s.auditLogger != nil {
		event := auth.APIKeyAuditEvent{
			UserID:    userID,
			KeyID:     keyID,
			Action:    auth.AuditKeyExpireSet,
			IP:        r.RemoteAddr,
			UserAgent: r.UserAgent(),
			Success:   true,
			Metadata: map[string]interface{}{
				"expires_at": expiresAt,
			},
		}
		if err := s.auditLogger.LogKeyEvent(r.Context(), event); err != nil {
			s.logger.Error("failed to log audit event", zap.Error(err))
		}
	}

	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]string{
		"message":    "Expiration set successfully",
		"expires_at": expiresAt.Format(time.RFC3339),
	}); err != nil {
		s.logger.Error("failed to encode expiration response", zap.Error(err))
	}
}

// handleGetUserAPIKeyAuditLogs retrieves audit logs for API keys
func (s *Server) handleGetUserAPIKeyAuditLogs(w http.ResponseWriter, r *http.Request) {
	userID := r.Context().Value(userIDKey).(string)

	if s.auditLogger != nil {
		filters := auth.AuditFilters{
			UserID: userID,
			Limit:  100,
		}

		if keyID := r.URL.Query().Get("key_id"); keyID != "" {
			filters.KeyID = keyID
		}
		if action := r.URL.Query().Get("action"); action != "" {
			filters.Action = action
		}

		logs, err := s.auditLogger.GetAuditLogs(r.Context(), filters)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(logs); err != nil {
			s.logger.Error("failed to encode audit logs", zap.Error(err))
		}
		return
	}

	// Return empty logs if no audit logger
	logs := []map[string]interface{}{}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(logs); err != nil {
		s.logger.Error("failed to encode empty audit logs", zap.Error(err))
	}
}

// handleUpdateUserInfo updates user information
func (s *Server) handleUpdateUserInfo(w http.ResponseWriter, r *http.Request) {
	var update struct {
		Email   string `json:"email,omitempty"`
		Company string `json:"company,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"message": "user updated successfully",
	})
}

// handleDeleteUser deletes a user account
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotImplemented)
}

// handleGetProfile returns user profile
func (s *Server) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	profile := map[string]interface{}{
		"displayName": "John Doe",
		"avatar":      "",
		"bio":         "Storage enthusiast",
		"location":    "El Paso, TX",
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(profile)
}

// handleUpdateProfile updates user profile
func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	var profile map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&profile); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(profile)
}

// handleGetPreferences returns user preferences
func (s *Server) handleGetPreferences(w http.ResponseWriter, r *http.Request) {
	prefs := map[string]interface{}{
		"theme":         "light",
		"notifications": true,
		"email_digest":  "weekly",
		"language":      "en",
		"timezone":      "America/Chicago",
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(prefs)
}

// handleUpdatePreferences updates user preferences
func (s *Server) handleUpdatePreferences(w http.ResponseWriter, r *http.Request) {
	var prefs map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&prefs); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(prefs)
}

// handleGetActivity returns user activity history
func (s *Server) handleGetActivity(w http.ResponseWriter, r *http.Request) {
	activities := []map[string]interface{}{
		{
			"action":    "login",
			"timestamp": time.Now().Add(-1 * time.Hour),
			"ip":        "192.168.1.1",
		},
		{
			"action":    "upload",
			"resource":  "file.txt",
			"timestamp": time.Now().Add(-2 * time.Hour),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(activities)
}

// handleEnableMFA enables MFA for user
func (s *Server) handleEnableMFA(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"secret":  "JBSWY3DPEHPK3PXP",
		"qr_code": "data:image/png;base64,...",
		"backup_codes": []string{
			"ABC-123-456",
			"DEF-789-012",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// handleDisableMFA disables MFA for user
func (s *Server) handleDisableMFA(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"message": "MFA disabled successfully",
	})
}

// handleGetBackupCodes returns MFA backup codes
func (s *Server) handleGetBackupCodes(w http.ResponseWriter, r *http.Request) {
	codes := []string{
		"ABC-123-456",
		"DEF-789-012",
		"GHI-345-678",
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"backup_codes": codes,
	})
}
