package api

// Apps (WP-APP-1 PR C, Stage 1a Prompt 55; docs/ACCOUNTS.md § Apps; F-43):
// any verified, paying account makes app children with no platform grant —
// up to auth.MaxSelfServeApps — each with its own management key (scope
// apps) for its backend. That key reaches the platform routes AS the app
// (actsAsApp): it makes the app's end users (managed children, depth 2),
// their keys and STS tokens, all inside the builder's pool. DELETE
// schedules the app and its end users (the erasure cascades) and revokes
// the app's keys at once. An app's bucket is never Public until the root
// is verified (modes.ErrUnverifiedNeverPublic). Dark behind the apps flag.

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
)

// flagApps opens self-serve apps for a tenant (WP-APP-1). Default OFF, per
// tenant first.
const flagApps = "apps"

func (s *Server) registerAppRoutes(r chi.Router) {
	r.Get("/apps", s.handleMgmtListApps)
	r.With(s.requireRole(auth.ActionChildWrite)).Post("/apps", s.handleMgmtCreateApp)
	r.With(s.requireRole(auth.ActionChildWrite)).Delete("/apps/{id}", s.handleMgmtDeleteApp)
}

func (s *Server) appsOr404(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := activeTenant(r)
	if s.db == nil || s.flags == nil || tenantID == "" || !s.flags.Enabled(flagApps, tenantID) {
		writeManagementError(w, ErrTypeNotFound, "not_found", "no such route", "")
		return "", false
	}
	return tenantID, true
}

// handleMgmtCreateApp is POST /api/v1/manage/apps {label, storage_cap_bytes?,
// egress_cap_bytes?}: the app child and its backend's management key (the
// secret shown once).
func (s *Server) handleMgmtCreateApp(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.appsOr404(w, r)
	if !ok {
		return
	}
	var req struct {
		Label      string `json:"label"`
		StorageCap int64  `json:"storage_cap_bytes"`
		EgressCap  int64  `json:"egress_cap_bytes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	c, err := auth.CreateChildTenant(r.Context(), s.db, tenantID, req.Label, "", auth.ChildOptions{Kind: auth.ChildApp, SelfServe: true,
		Caps: auth.ChildCaps{StorageBytes: req.StorageCap, EgressBytes: req.EgressCap}})
	switch {
	case errors.Is(err, auth.ErrAppsNeedVerified):
		writeManagementError(w, ErrTypePermission, "apps_need_verified_account", "verify the account's e-mail address first: an app is made by a verified account", "")
		return
	case errors.Is(err, auth.ErrAppsNeedPaying):
		writeManagementError(w, ErrTypePermission, "apps_need_paying_account", "apps are made by a paying account: buy storage first ("+billingURL()+")", "")
		return
	case errors.Is(err, auth.ErrAppLimit):
		writeManagementError(w, ErrTypeConflict, "app_limit_reached", "an account holds at most 25 apps: delete one first", "")
		return
	case errors.Is(err, auth.ErrBadChildLabel):
		writeManagementError(w, ErrTypeInvalidRequest, "label_required", err.Error(), "label")
		return
	case errors.Is(err, auth.ErrBadCap):
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_cap", err.Error(), "storage_cap_bytes")
		return
	case errors.Is(err, auth.ErrChildOfChild):
		writeManagementError(w, ErrTypeInvalidRequest, "child_of_child", "an app, a sandbox or a child account makes no apps", "")
		return
	case errors.Is(err, auth.ErrTenantSuspended):
		writeManagementError(w, ErrTypePermission, "account_suspended", "the account is suspended: no app can be made", "")
		return
	case err != nil:
		s.logger.Error("apps: create", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to create the app", "")
		return
	}
	userID, _ := r.Context().Value(userIDKey).(string)
	k, secret, err := auth.MintManagementKey(r.Context(), s.db, c.ID, auth.ManagementKeyOptions{Name: "app backend: " + c.Label,
		Scopes: []string{auth.ScopeApps, auth.ScopeManageWrite}, CreatedBy: userID, ExpiresAt: capExpiry(managementKeyOf(r.Context()), nil)})
	if err != nil {
		s.logger.Error("apps: mint the app's key", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "the app was made but its key was not: delete it and try again", "")
		return
	}
	if s.flags != nil {
		_ = s.flags.Refresh(r.Context()) // the app reads its builder's inherited flags at once (RP82-4)
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: tenantID, Action: "app.created", Resource: "tenant:" + c.ID,
		Metadata: map[string]any{"label": c.Label, "management_key_id": k.ID}})
	emitEvent(r.Context(), s.db, s.logger, "app.created", tenantID, map[string]any{"app_id": c.ID, "label": c.Label})
	writeJSON(w, http.StatusCreated, map[string]any{"object": "app", "id": c.ID, "label": c.Label, "kind": string(c.Kind),
		"parent_id": tenantID, "caps": c.Caps, "residency": childResidency(c.Residency), "created_at": time.Now().UTC(),
		"management_key": managementKeyJSON(k, secret), "request_id": getRequestID(w)})
}

// handleMgmtListApps is GET /api/v1/manage/apps: the caller's live apps.
func (s *Server) handleMgmtListApps(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.appsOr404(w, r)
	if !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT `+platformChildColumns+`
		  FROM tenants t LEFT JOIN tenant_quotas q ON q.tenant_id = t.id
		 WHERE t.parent_tenant_id = $1 AND t.child_kind = 'app' ORDER BY t.created_at, t.id LIMIT 100`, tenantID)
	if err != nil {
		s.logger.Error("apps: list", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list the apps", "")
		return
	}
	defer func() { _ = rows.Close() }()
	items := []interface{}{}
	for rows.Next() {
		c, err := scanPlatformChild(rows)
		if err != nil {
			s.logger.Error("apps: list", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list the apps", "")
			return
		}
		c.Object = "app"
		items = append(items, c)
	}
	if err := rows.Err(); err != nil {
		s.logger.Error("apps: list", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list the apps", "")
		return
	}
	writeListResponse(w, items, false, "", len(items))
}

// handleMgmtDeleteApp is DELETE /api/v1/manage/apps/{id}: the app and its
// end users scheduled (the erasure cascades, D-16's grace), the app's keys
// and its end users' keys revoked at once.
func (s *Server) handleMgmtDeleteApp(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.appsOr404(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var isApp bool
	if err := s.db.QueryRowContext(r.Context(), `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1 AND parent_tenant_id = $2 AND child_kind = 'app')`,
		id, tenantID).Scan(&isApp); err != nil {
		s.logger.Error("apps: read", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to read the app", "")
		return
	}
	if !isApp {
		writeManagementError(w, ErrTypeNotFound, "app_not_found", "no app of this account has that id", "id")
		return
	}
	svc := account.NewService(s.db, s.logger)
	at, err := svc.ScheduleTenant(r.Context(), id)
	if err != nil {
		s.logger.Error("apps: schedule", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to schedule the app's deletion", "")
		return
	}
	users, err := s.appEndUsers(r, id)
	if err != nil {
		s.logger.Warn("apps: list the end users (the cascade still erases them with the app)", zap.Error(err))
	}
	for _, u := range users {
		if _, err := svc.ScheduleTenant(r.Context(), u); err != nil {
			s.logger.Warn("apps: schedule an end user", zap.String("tenant_id", u), zap.Error(err))
		}
	}
	if _, err := s.db.ExecContext(r.Context(), `UPDATE management_keys SET revoked_at = NOW() WHERE tenant_id = $1 AND revoked_at IS NULL`, id); err != nil {
		s.logger.Error("apps: revoke the app's keys", zap.Error(err))
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: tenantID, Action: "app.deletion_scheduled", Resource: "tenant:" + id,
		Metadata: map[string]any{"scheduled_at": at}})
	writeJSON(w, http.StatusOK, map[string]any{"object": "app", "id": id, "deletion_scheduled": true,
		"deletion_scheduled_at": at.UTC(), "request_id": getRequestID(w)})
}

// appEndUsers lists the app's children.
func (s *Server) appEndUsers(r *http.Request, app string) ([]string, error) {
	rows, err := s.db.QueryContext(r.Context(), `SELECT id FROM tenants WHERE parent_tenant_id = $1 ORDER BY id`, app)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return out, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
