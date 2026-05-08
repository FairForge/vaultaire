package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

func (s *Server) registerSTSRoutes() {
	s.router.Route("/api/v1/sts", func(r chi.Router) {
		r.Use(s.requireJWT)
		r.Post("/token", s.handleSTSCreateToken)
	})
}

func (s *Server) handleSTSCreateToken(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if userID == "" || tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_user", "user not found in token", "")
		return
	}

	var req auth.STSRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}

	if len(req.Permissions) > 0 {
		if err := auth.ValidatePermissions(req.Permissions); err != nil {
			writeManagementError(w, ErrTypeInvalidRequest, "invalid_permissions", err.Error(), "permissions")
			return
		}
	}

	parentKeyID := "tenant:" + tenantID
	parentScope := &auth.KeyScope{Permissions: []string{"*"}}

	keys, err := s.auth.ListAPIKeys(r.Context(), userID)
	if err == nil && len(keys) > 0 {
		parentKeyID = keys[0].Key
		parentScope = &auth.KeyScope{
			Permissions: keys[0].Permissions,
			BucketScope: keys[0].BucketScope,
			IPAllowlist: keys[0].IPAllowlist,
			ExpiresAt:   keys[0].ExpiresAt,
		}
	}

	token, err := auth.GenerateSTSToken(r.Context(), s.db, tenantID, parentKeyID, parentScope, req)
	if err != nil {
		s.logger.Error("sts create token", zap.Error(err))
		writeManagementError(w, ErrTypeInvalidRequest, "scope_error", err.Error(), "")
		return
	}

	resp := map[string]interface{}{
		"object":     "sts_token",
		"access_key": token.AccessKey,
		"secret_key": token.SecretKey,
		"expiration": token.ExpiresAt.Format(time.RFC3339),
		"request_id": getRequestID(w),
	}
	writeJSON(w, http.StatusCreated, resp)
}
