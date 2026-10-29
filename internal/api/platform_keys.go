package api

// WP-PLAT-1 PR C: a platform's child's keys and STS tokens
// (internal/auth/platform_keys.go). A child has no primary pair; every key
// is scoped, minted by its platform within the calling credential's own
// scope (auth.CapScope: a JWT is the account's own authority; a scoped
// credential on the context bounds what it mints) and revocable by it.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
)

// platformKey is a child's key as the API answers it (the secret only on
// the create).
func platformKey(k *auth.APIKey, withSecret bool) map[string]interface{} {
	out := map[string]interface{}{
		"object": "api_key", "id": k.ID, "name": k.Name, "key": k.Key, "tenant_id": k.TenantID,
		"is_primary": false, "permissions": k.Permissions, "bucket_scope": k.BucketScope,
		"ip_allowlist": k.IPAllowlist, "created_at": k.CreatedAt.UTC(),
	}
	if k.ExpiresAt != nil {
		out["expires_at"] = k.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if k.RevokedAt != nil {
		out["revoked_at"] = k.RevokedAt.UTC()
	}
	if withSecret {
		out["secret"] = k.Secret
	}
	return out
}

// platformChildOrNotFound answers 404 unless {id} is the caller's child.
func (s *Server) platformChildOrNotFound(w http.ResponseWriter, r *http.Request) (parent, child string, ok bool) {
	parent, _ = r.Context().Value(tenantIDKey).(string)
	child = chi.URLParam(r, "id")
	c, err := s.platformChildOf(r, parent, child)
	if err != nil {
		s.logger.Error("platform: read child", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to read the tenant", "")
		return "", "", false
	}
	if c == nil {
		writeManagementError(w, ErrTypeNotFound, "tenant_not_found", "no tenant of this platform has that id", "id")
		return "", "", false
	}
	return parent, child, true
}

// handlePlatformCreateKey is POST /api/v1/platform/tenants/{id}/keys.
func (s *Server) handlePlatformCreateKey(w http.ResponseWriter, r *http.Request) {
	parent, child, ok := s.platformChildOrNotFound(w, r)
	if !ok {
		return
	}
	userID, _ := r.Context().Value(userIDKey).(string)
	var req struct {
		Name        string     `json:"name"`
		Permissions []string   `json:"permissions"`
		BucketScope []string   `json:"bucket_scope"`
		IPAllowlist []string   `json:"ip_allowlist"`
		ExpiresAt   *time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	opts := &auth.KeyCreateOptions{Permissions: req.Permissions, BucketScope: req.BucketScope, IPAllowlist: req.IPAllowlist, ExpiresAt: req.ExpiresAt}
	if err := opts.Validate(time.Now()); err != nil {
		if !writeKeyScopeError(w, err) {
			writeManagementError(w, ErrTypeInvalidRequest, "invalid_scope", err.Error(), "")
		}
		return
	}
	if err := auth.CapScope(auth.KeyScopeFromContext(r.Context()), opts); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "scope_exceeds_parent",
			strings.TrimPrefix(err.Error(), auth.ErrScopeExceedsParent.Error()+": ")+": a child's key never exceeds the scope of the credential that mints it", "")
		return
	}
	key, err := s.auth.GenerateChildKey(r.Context(), userID, parent, child, req.Name, opts)
	switch {
	case errors.Is(err, auth.ErrTenantNotFound):
		writeManagementError(w, ErrTypeNotFound, "tenant_not_found", "no tenant of this platform has that id", "id")
		return
	case errors.Is(err, auth.ErrKeyLimitReached):
		writeManagementError(w, ErrTypeConflict, "key_limit_exceeded", "a child holds at most 50 live keys; revoke one", "")
		return
	case err != nil:
		if !writeKeyScopeError(w, err) {
			s.logger.Error("platform: create child key", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to create the key", "")
		}
		return
	}
	emitEvent(r.Context(), s.db, s.logger, "key.created", parent, map[string]interface{}{"key_id": key.ID, "name": key.Name, "tenant_id": child})
	resp := platformKey(key, true)
	resp["request_id"] = getRequestID(w)
	writeJSON(w, http.StatusCreated, resp)
}

// handlePlatformListKeys is GET /api/v1/platform/tenants/{id}/keys.
func (s *Server) handlePlatformListKeys(w http.ResponseWriter, r *http.Request) {
	_, child, ok := s.platformChildOrNotFound(w, r)
	if !ok {
		return
	}
	keys, err := auth.ListChildKeys(r.Context(), s.db, child)
	if err != nil {
		s.logger.Error("platform: list child keys", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list the keys", "")
		return
	}
	items := make([]interface{}, len(keys))
	for i, k := range keys {
		items[i] = platformKey(k, false)
	}
	writeListResponse(w, items, false, "", len(items))
}

// handlePlatformDeleteKey is DELETE /api/v1/platform/tenants/{id}/keys/{key}.
func (s *Server) handlePlatformDeleteKey(w http.ResponseWriter, r *http.Request) {
	parent, child, ok := s.platformChildOrNotFound(w, r)
	if !ok {
		return
	}
	keyID := chi.URLParam(r, "key")
	err := s.auth.RevokeChildKey(r.Context(), parent, child, keyID)
	switch {
	case errors.Is(err, auth.ErrKeyNotFound):
		writeManagementError(w, ErrTypeNotFound, "key_not_found", "the child has no key with that id", "key")
		return
	case errors.Is(err, auth.ErrKeyRevoked):
		writeManagementError(w, ErrTypeConflict, "key_revoked", "the key is already revoked", "key")
		return
	case err != nil:
		s.logger.Error("platform: revoke child key", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to revoke the key", "")
		return
	}
	emitEvent(r.Context(), s.db, s.logger, "key.revoked", parent, map[string]interface{}{"key_id": keyID, "tenant_id": child})
	writeJSON(w, http.StatusOK, map[string]interface{}{"object": "api_key", "id": keyID, "deleted": true, "request_id": getRequestID(w)})
}

// handlePlatformCreateSTS is POST /api/v1/platform/tenants/{id}/sts: a
// temporary credential of the child bounded by one of its keys
// (parent_key_id, required — a child has no primary), the STS scope
// intersection reused.
func (s *Server) handlePlatformCreateSTS(w http.ResponseWriter, r *http.Request) {
	parent, child, ok := s.platformChildOrNotFound(w, r)
	if !ok {
		return
	}
	var req auth.STSRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	if req.ParentKeyID == "" {
		writeManagementError(w, ErrTypeInvalidRequest, "parent_key_required", "parent_key_id names the child's key that bounds the token (a child has no primary pair)", "parent_key_id")
		return
	}
	if len(req.Permissions) > 0 {
		if err := auth.ValidatePermissions(req.Permissions); err != nil {
			writeManagementError(w, ErrTypeInvalidRequest, "invalid_permissions", err.Error(), "permissions")
			return
		}
	}
	accessKey, scope, err := auth.ChildKeyScope(r.Context(), s.db, child, req.ParentKeyID)
	switch {
	case errors.Is(err, auth.ErrKeyNotFound):
		writeManagementError(w, ErrTypeNotFound, "parent_key_not_found", "parent_key_id is not one of the child's keys", "parent_key_id")
		return
	case errors.Is(err, auth.ErrKeyRevoked):
		writeManagementError(w, ErrTypeInvalidRequest, "parent_key_revoked", "parent_key_id names a revoked key", "parent_key_id")
		return
	case errors.Is(err, auth.ErrKeyExpired):
		writeManagementError(w, ErrTypeAuthentication, "parent_key_expired", "parent_key_id names an expired key", "parent_key_id")
		return
	case err != nil:
		s.logger.Error("platform: sts parent key", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to resolve the parent key", "")
		return
	}
	token, err := auth.GenerateSTSToken(r.Context(), s.db, s.credentialKeys(), child, accessKey, scope, req)
	if err != nil {
		if errors.Is(err, auth.ErrSTSScope) {
			writeManagementError(w, ErrTypeInvalidRequest, "scope_error", strings.TrimPrefix(err.Error(), auth.ErrSTSScope.Error()+": "), "")
			return
		}
		s.logger.Error("platform: sts create token", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to create STS token", "")
		return
	}
	emitEvent(r.Context(), s.db, s.logger, "sts.token_created", parent, map[string]interface{}{"access_key": token.AccessKey, "tenant_id": child})
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: parent, EventType: "platform", Action: "platform.sts_created", Resource: "sts:" + token.AccessKey,
		Metadata: map[string]any{"child": child, "parent_key": req.ParentKeyID, "expires_at": token.ExpiresAt, "permissions": token.Permissions}})
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"object": "sts_token", "access_key": token.AccessKey, "secret_key": token.SecretKey,
		"tenant_id": child, "expiration": token.ExpiresAt.Format(time.RFC3339), "request_id": getRequestID(w),
	})
}
