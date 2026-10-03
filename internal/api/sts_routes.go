package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
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

// handleSTSCreateToken mints a temporary credential bounded by the caller's
// authority. The JWT holder owns the tenant, so the default parent scope is
// the account's own full access; `parent_key_id` bounds the token to one of
// the caller's own live keys instead. (Review R11-03: the parent used to be
// `ListAPIKeys()[0]` — a random map entry, and a fresh account's primary key
// carries no permission list, so no token could ever be minted for it.)
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

	// The default parent is the account's primary pair — a real key row, so
	// the token dies when the primary is rotated (WP-R5-5: a parent named
	// by nothing could never be revoked).
	var parent *auth.APIKey
	var err error
	if req.ParentKeyID != "" {
		parent, err = s.auth.GetOwnedAPIKey(r.Context(), userID, req.ParentKeyID)
	} else {
		parent, err = s.auth.GetPrimaryAPIKey(r.Context(), tenantID)
	}
	switch {
	case errors.Is(err, auth.ErrKeyNotFound):
		writeManagementError(w, ErrTypeNotFound, "parent_key_not_found", "parent_key_id is not one of your API keys", "parent_key_id")
		return
	case errors.Is(err, auth.ErrKeyRevoked):
		writeManagementError(w, ErrTypeInvalidRequest, "parent_key_revoked", "parent_key_id names a revoked key", "parent_key_id")
		return
	case err != nil:
		s.logger.Error("sts parent key lookup", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to resolve parent key", "")
		return
	}
	parentKeyID := parent.Key
	perms := parent.Permissions
	if len(perms) == 0 {
		// A key row without a permission list is a full-access key
		// (the S3 auth path reads COALESCE(permissions, '["*"]')).
		perms = []string{"*"}
	}
	parentScope := &auth.KeyScope{
		Permissions: perms,
		BucketScope: parent.BucketScope,
		IPAllowlist: parent.IPAllowlist,
		ExpiresAt:   parent.ExpiresAt,
	}

	token, err := auth.GenerateSTSToken(r.Context(), s.db, tenantID, parentKeyID, parentScope, req)
	if err != nil {
		if errors.Is(err, auth.ErrSTSScope) {
			writeManagementError(w, ErrTypeInvalidRequest, "scope_error",
				strings.TrimPrefix(err.Error(), auth.ErrSTSScope.Error()+": "), "")
			return
		}
		// Persist failures used to be echoed as a 400 with the SQL text in it.
		s.logger.Error("sts create token", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to create STS token", "")
		return
	}

	emitEvent(r.Context(), s.db, s.logger, "sts.token_created", tenantID, map[string]interface{}{
		"access_key": token.AccessKey,
	})
	audit.Record(r.Context(), s.db, audit.Entry{
		UserID: userID, TenantID: tenantID, Action: "sts.token_created", Resource: "sts:" + token.AccessKey,
		Metadata: map[string]any{"parent_key": parentKeyID, "expires_at": token.ExpiresAt, "permissions": token.Permissions},
	})

	resp := map[string]interface{}{
		"object":     "sts_token",
		"access_key": token.AccessKey,
		"secret_key": token.SecretKey,
		"expiration": token.ExpiresAt.Format(time.RFC3339),
		"request_id": getRequestID(w),
	}
	writeJSON(w, http.StatusCreated, resp)
}
