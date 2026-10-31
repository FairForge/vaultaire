package api

// The management key on the management surfaces (WP-KEY-1, Stage 1a Prompt
// 54 PR A; D-STRIPE-29; internal/auth/management_keys.go). requireCaller
// takes a Bearer that is a management key (sk_live_… / sk_test_…) beside a
// JWT on /api/v1/manage and /api/v1/platform — a Bearer is no AWS scheme,
// so the S3 credential gate never captures it (Architecture Decision 8).
// A key has no member and no role of its own: its scopes are its
// authority, given as a role per surface so requireRole and auth.Can stay
// the one rule:
//
//   - /api/v1/manage: manage:write acts as a developer, manage:read as a
//     viewer;
//   - /api/v1/platform: platform acts as an admin (a platform's root key);
//     apps acts as an admin AS THE APP (an app child's key, minted by its
//     platform): its tree is its own managed children, depth 2 the deepest.
//
// What needs a person is requirePerson's: the members (invite, role,
// removal, an invitation's accept or revoke), the export, the erasure and
// minting management keys — a leaked server key never makes its holder a
// member. /api/v1/user, /webhooks, /events and /sts stay JWT only.

import (
	"context"
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

// callerSurface is the surface a management key is mounted on.
type callerSurface int

const (
	surfaceManage callerSurface = iota
	surfacePlatform
)

type mgmtKeyCtxKey struct{}

// managementKeyOf is the management key that authenticated the request
// (nil = a person's JWT).
func managementKeyOf(ctx context.Context) *auth.ManagementKey {
	k, _ := ctx.Value(mgmtKeyCtxKey{}).(*auth.ManagementKey)
	return k
}

// actsAsApp reports whether the request is an app's key on the platform
// surface.
func actsAsApp(ctx context.Context) *auth.ManagementKey {
	if k := managementKeyOf(ctx); k != nil && k.TenantKind == string(auth.ChildApp) && k.Has(auth.ScopeApps) {
		return k
	}
	return nil
}

// managementKeyRole is the role a key's scopes give it on a surface ("" =
// none: requireMember's 403).
func managementKeyRole(k *auth.ManagementKey, surface callerSurface) auth.Role {
	switch surface {
	case surfaceManage:
		if k.Has(auth.ScopeManageWrite) {
			return auth.RoleDeveloper
		}
		if k.Has(auth.ScopeManageRead) {
			return auth.RoleViewer
		}
	case surfacePlatform:
		if k.Has(auth.ScopePlatform) && k.TenantKind == "" {
			return auth.RoleAdmin
		}
		if k.Has(auth.ScopeApps) && k.TenantKind == string(auth.ChildApp) {
			return auth.RoleAdmin
		}
	}
	return ""
}

// requireCaller is requireJWT, or a management key: the key's tenant, its
// role on the surface, its principal (mk:<id> — the request log's key_id,
// the audit row's principal, the idempotency key's prefix), the user its
// rows hang on (the account's owner) and its S3 ceiling as the calling
// credential's scope (CapScope's seam).
func (s *Server) requireCaller(surface callerSurface) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		viaJWT := s.requireJWT(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			if !auth.IsManagementKeySecret(token) {
				viaJWT.ServeHTTP(w, r)
				return
			}
			if s.db == nil {
				writeManagementError(w, ErrTypeAPI, "no_database", "database unavailable", "")
				return
			}
			k, err := auth.LookupManagementKey(r.Context(), s.db, token)
			switch {
			case errors.Is(err, auth.ErrManagementKeyInvalid):
				writeManagementError(w, ErrTypeAuthentication, "invalid_management_key", "the management key is unknown or revoked", "")
				return
			case err != nil:
				s.logger.Error("management key lookup", zap.Error(err))
				writeManagementError(w, ErrTypeAPI, "management_key_lookup_failed", "could not read the management key", "")
				return
			}
			ctx := context.WithValue(r.Context(), mgmtKeyCtxKey{}, k)
			ctx = context.WithValue(ctx, userIDKey, k.Actor)
			ctx = context.WithValue(ctx, tenantIDKey, k.TenantID)
			ctx = context.WithValue(ctx, roleKey, managementKeyRole(k, surface))
			ctx = context.WithValue(ctx, jwtIDKey, k.Principal())
			ctx = audit.WithPrincipal(ctx, k.Principal())
			if ks := k.Ceiling.KeyScope(time.Now()); ks != nil {
				ctx = auth.WithKeyScope(ctx, ks)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// requirePerson refuses a management key (403 person_required).
func (s *Server) requirePerson(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if managementKeyOf(r.Context()) != nil {
			writeManagementError(w, ErrTypePermission, "person_required",
				"a management key never manages members, exports or erases the account, or mints management keys: sign in as a person", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// managementKeyJSON is a key as the API answers it (the secret only on the
// mint).
func managementKeyJSON(k *auth.ManagementKey, secret string) map[string]any {
	out := map[string]any{
		"object": "management_key", "id": k.ID, "tenant_id": k.TenantID, "name": k.Name, "prefix": k.Prefix,
		"hint": k.Hint, "scopes": k.Scopes, "s3_ceiling": k.Ceiling, "created_at": k.CreatedAt.UTC(),
		"last_used_at": k.LastUsedAt, "revoked_at": k.RevokedAt, "expires_at": k.ExpiresAt,
		"livemode": k.Prefix == auth.ManagementKeyLivePrefix,
	}
	if k.CreatedBy != "" {
		out["created_by"] = k.CreatedBy
	}
	if secret != "" {
		out["secret"] = secret
	}
	return out
}

// mintRequest is a mint's body. Test mints the key on the tenant's
// sandbox (sk_test_, RP88-1); ExpiresAt is optional (RP88-2).
type mintRequest struct {
	Name      string          `json:"name"`
	Scopes    []string        `json:"scopes"`
	Ceiling   *auth.S3Ceiling `json:"s3_ceiling"`
	Test      bool            `json:"test"`
	ExpiresAt *time.Time      `json:"expires_at"`
}

// capExpiry bounds a minted key's expiry by the minting key's: a key never
// outlives the key that mints it (nil caller = a person: no bound).
func capExpiry(caller *auth.ManagementKey, at *time.Time) *time.Time {
	if caller == nil || caller.ExpiresAt == nil {
		return at
	}
	if at == nil || at.After(*caller.ExpiresAt) {
		t := *caller.ExpiresAt
		return &t
	}
	return at
}

// capCeiling bounds a minted key's ceiling by the calling credential's
// (nil = a person: no bound). What the request leaves unset is the caller's.
func capCeiling(caller *auth.KeyScope, c *auth.S3Ceiling) (*auth.S3Ceiling, error) {
	if caller == nil {
		return c, nil
	}
	now := time.Now()
	o := &auth.KeyCreateOptions{}
	if c != nil {
		o.Permissions, o.BucketScope, o.IPAllowlist = c.Permissions, c.BucketScope, c.IPAllowlist
		if c.MaxExpirySeconds > 0 {
			at := now.Add(time.Duration(c.MaxExpirySeconds) * time.Second)
			o.ExpiresAt = &at
		}
	}
	if err := auth.CapScope(caller, o); err != nil {
		return nil, err
	}
	out := &auth.S3Ceiling{Permissions: o.Permissions, BucketScope: o.BucketScope, IPAllowlist: o.IPAllowlist}
	if o.ExpiresAt != nil {
		out.MaxExpirySeconds = int64(o.ExpiresAt.Sub(now).Seconds())
	}
	return out, nil
}

// mintManagementKey mints a key of tenantID and answers it (201) or the
// refusal.
func (s *Server) mintManagementKey(w http.ResponseWriter, r *http.Request, tenantID string) {
	var req mintRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	ceiling, err := capCeiling(auth.KeyScopeFromContext(r.Context()), req.Ceiling)
	if err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "scope_exceeds_parent",
			strings.TrimPrefix(err.Error(), auth.ErrScopeExceedsParent.Error()+": ")+": a key's ceiling never exceeds the credential that mints it", "s3_ceiling")
		return
	}
	createdBy := ""
	caller := managementKeyOf(r.Context())
	if caller == nil {
		createdBy, _ = r.Context().Value(userIDKey).(string)
	}
	k, secret, err := auth.MintManagementKey(r.Context(), s.db, tenantID,
		auth.ManagementKeyOptions{Name: req.Name, Scopes: req.Scopes, Ceiling: ceiling, CreatedBy: createdBy,
			Test: req.Test, ExpiresAt: capExpiry(caller, req.ExpiresAt)})
	switch {
	case errors.Is(err, auth.ErrManagementKeyExpiry):
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_expiry", err.Error(), "expires_at")
		return
	case errors.Is(err, auth.ErrSandboxOfSandbox):
		writeManagementError(w, ErrTypeInvalidRequest, "sandbox_of_sandbox", "a test-mode account has no sandbox of its own", "test")
		return
	case errors.Is(err, auth.ErrTenantSuspended):
		writeManagementError(w, ErrTypePermission, "account_suspended", "the account is suspended: no test key can be made", "test")
		return
	case errors.Is(err, auth.ErrManagementKeyScope):
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_scope", err.Error(), "scopes")
		return
	case errors.Is(err, auth.ErrManagementKeyCeiling):
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_ceiling", err.Error(), "s3_ceiling")
		return
	case errors.Is(err, auth.ErrKeyLimitReached):
		writeManagementError(w, ErrTypeConflict, "key_limit_exceeded", "an account holds at most 25 live management keys; revoke one", "")
		return
	case errors.Is(err, auth.ErrTenantNotFound):
		writeManagementError(w, ErrTypeNotFound, "tenant_not_found", "no such account", "")
		return
	case err != nil:
		s.logger.Error("mint a management key", zap.Error(err), zap.String("tenant_id", tenantID))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to mint the key", "")
		return
	}
	meta := map[string]any{"scopes": k.Scopes, "prefix": k.Prefix, "name": k.Name}
	if k.TenantID != tenantID {
		meta["sandbox"] = k.TenantID
	}
	if k.ExpiresAt != nil {
		meta["expires_at"] = k.ExpiresAt.UTC()
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: tenantID, Action: "management_key.created",
		Resource: "management_key:" + k.ID, Metadata: meta})
	resp := managementKeyJSON(k, secret)
	resp["request_id"] = getRequestID(w)
	writeJSON(w, http.StatusCreated, resp)
}

// listManagementKeys answers tenantID's keys.
func (s *Server) listManagementKeys(w http.ResponseWriter, r *http.Request, tenantID string) {
	keys, err := auth.ListManagementKeys(r.Context(), s.db, tenantID)
	if err != nil {
		s.logger.Error("list management keys", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list the keys", "")
		return
	}
	items := make([]any, len(keys))
	for i, k := range keys {
		items[i] = managementKeyJSON(k, "")
	}
	writeListResponse(w, items, false, "", len(items))
}

// revokeManagementKey revokes tenantID's key id.
func (s *Server) revokeManagementKey(w http.ResponseWriter, r *http.Request, tenantID, id string) {
	err := auth.RevokeManagementKey(r.Context(), s.db, tenantID, id)
	switch {
	case errors.Is(err, auth.ErrManagementKeyNotFound):
		writeManagementError(w, ErrTypeNotFound, "management_key_not_found", "no live management key of this account has that id", "id")
		return
	case err != nil:
		s.logger.Error("revoke a management key", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to revoke the key", "")
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: tenantID, Action: "management_key.revoked", Resource: "management_key:" + id})
	writeJSON(w, http.StatusOK, map[string]any{"object": "management_key", "id": id, "revoked": true, "request_id": getRequestID(w)})
}

// registerManagementKeyRoutes adds /management-keys to /api/v1/manage.
func (s *Server) registerManagementKeyRoutes(r chi.Router) {
	r.Get("/management-keys", func(w http.ResponseWriter, r *http.Request) { s.listManagementKeys(w, r, activeTenant(r)) })
	r.With(s.requirePerson, s.requireRole(auth.ActionManagementKey)).Post("/management-keys",
		func(w http.ResponseWriter, r *http.Request) { s.mintManagementKey(w, r, activeTenant(r)) })
	r.With(s.requirePerson, s.requireRole(auth.ActionManagementKey)).Delete("/management-keys/{id}",
		func(w http.ResponseWriter, r *http.Request) {
			s.revokeManagementKey(w, r, activeTenant(r), chi.URLParam(r, "id"))
		})
}

// platformAppOrRefused answers unless {id} is an APP child of the caller
// (an app's key: minted by its platform, for the app's backend).
func (s *Server) platformAppOrRefused(w http.ResponseWriter, r *http.Request) (string, bool) {
	parent, _ := r.Context().Value(tenantIDKey).(string)
	id := chi.URLParam(r, "id")
	c, err := s.platformChildOf(r, parent, id)
	if err != nil {
		s.logger.Error("platform: read child", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to read the tenant", "")
		return "", false
	}
	if c == nil {
		writeManagementError(w, ErrTypeNotFound, "tenant_not_found", "no tenant of this platform has that id", "id")
		return "", false
	}
	if c.Kind != string(auth.ChildApp) {
		writeManagementError(w, ErrTypeInvalidRequest, "not_an_app", "a management key is minted for an app child only (kind app)", "id")
		return "", false
	}
	return id, true
}

// registerPlatformManagementKeyRoutes adds an app's management keys to
// /api/v1/platform.
func (s *Server) registerPlatformManagementKeyRoutes(r chi.Router) {
	r.With(s.requireRole(auth.ActionChildWrite)).Post("/tenants/{id}/management-keys", func(w http.ResponseWriter, r *http.Request) {
		if app, ok := s.platformAppOrRefused(w, r); ok {
			s.mintManagementKey(w, r, app)
		}
	})
	r.Get("/tenants/{id}/management-keys", func(w http.ResponseWriter, r *http.Request) {
		if app, ok := s.platformAppOrRefused(w, r); ok {
			s.listManagementKeys(w, r, app)
		}
	})
	r.With(s.requireRole(auth.ActionChildWrite)).Delete("/tenants/{id}/management-keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		if app, ok := s.platformAppOrRefused(w, r); ok {
			s.revokeManagementKey(w, r, app, chi.URLParam(r, "key"))
		}
	})
}
