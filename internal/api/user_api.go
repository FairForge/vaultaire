package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/go-chi/chi/v5"
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

// APIKeyResponse represents API key info
type APIKeyResponse struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Key       string     `json:"key"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}

// registerUserAPIRoutes sets up all user management endpoints
func (s *Server) registerUserAPIRoutes() {
	s.router.Route("/api/v1/user", func(r chi.Router) {
		r.Use(s.requireAuth)

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

		// API Keys
		r.Get("/apikeys", s.handleListAPIKeys)
		r.Post("/apikeys", s.handleCreateAPIKey)
		r.Delete("/apikeys/{keyId}", s.handleDeleteAPIKey)

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
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	tenantID, _ := r.Context().Value(common.TenantIDKey).(string)

	// Get quota info
	used, limit, _ := s.quotaManager.GetUsage(r.Context(), tenantID)
	tier, _ := s.quotaManager.GetTier(r.Context(), tenantID)

	userInfo := UserInfo{
		ID:       userID,
		Email:    r.Context().Value("email").(string),
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

	// TODO: Update user in database

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"message": "user updated successfully",
	})
}

// handleDeleteUser deletes user account
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement account deletion with confirmation
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

	// TODO: Save to database

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

	// TODO: Save to database

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(prefs)
}

// handleGetActivity returns user activity history
func (s *Server) handleGetActivity(w http.ResponseWriter, r *http.Request) {
	// TODO: Fetch from activity tracker
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

// handleListAPIKeys lists user's API keys
func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys := []APIKeyResponse{
		{
			ID:        "key-1",
			Name:      "Primary Key",
			Key:       "VK12345****",
			CreatedAt: time.Now().Add(-30 * 24 * time.Hour),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(keys)
}

// handleCreateAPIKey creates a new API key
func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// TODO: Generate actual API key

	response := APIKeyResponse{
		ID:        "key-new",
		Name:      req.Name,
		Key:       "VKnew12345" + auth.GenerateID()[:10],
		CreatedAt: time.Now(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(response)
}

// handleDeleteAPIKey deletes an API key
func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "keyId")

	// TODO: Delete from database

	w.WriteHeader(http.StatusNoContent)
	s.logger.Info("API key deleted", zap.String("key_id", keyID))
}

// handleEnableMFA enables MFA for user
func (s *Server) handleEnableMFA(w http.ResponseWriter, r *http.Request) {
	// TODO: Generate TOTP secret

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
	// TODO: Verify password and disable MFA

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
