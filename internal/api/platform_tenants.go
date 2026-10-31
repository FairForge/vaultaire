package api

// WP-PLAT-1 (Stage 1a Prompt 51; plan § WP-STRIPE — the Connect
// primitive): a PLATFORM tenant (tenants.platform, granted by an admin)
// creates, reads and schedules the erasure of user-less CHILD tenants
// under /api/v1/platform (internal/auth/platform.go). The routes mount on
// every process; whether they exist is decided per caller — any tenant
// that is not a platform gets the 404 an unknown route gets, never a 403,
// so no platform exists on prod until the owner grants one. Every call is
// in the request log under the parent (logManagementRequest) and every
// change is audited platform.* with the parent's caller as actor.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/drivers"
)

// registerPlatformRoutes mounts /api/v1/platform.
func (s *Server) registerPlatformRoutes() {
	rl, im := s.jsonAPIMiddleware()
	s.router.Route("/api/v1/platform", func(r chi.Router) {
		r.Use(s.requireCaller(surfacePlatform)) // a JWT, or a management key (WP-KEY-1)
		r.Use(s.requireMember)                  // WP-TEAM-1: the JWT's user is a member of the platform
		r.Use(s.logManagementRequest)           // the request log, under the parent
		r.Use(s.requirePlatform)                // 404 for any tenant that is not a platform
		r.Use(rl.Middleware)
		r.Use(im.Middleware)

		r.With(s.requireRole(auth.ActionChildWrite)).Post("/tenants", s.handlePlatformCreateTenant)
		r.Get("/tenants", s.handlePlatformListTenants)
		r.Get("/tenants/{id}", s.handlePlatformGetTenant)
		r.With(s.requireRole(auth.ActionChildWrite)).Patch("/tenants/{id}", s.handlePlatformPatchTenant) // WP-ACCT-1: caps
		r.With(s.requireRole(auth.ActionChildWrite)).Delete("/tenants/{id}", s.handlePlatformDeleteTenant)

		// PR C: the child's keys and STS tokens (platform_keys.go).
		r.With(s.requireRole(auth.ActionChildKey)).Post("/tenants/{id}/keys", s.handlePlatformCreateKey)
		r.Get("/tenants/{id}/keys", s.handlePlatformListKeys)
		r.With(s.requireRole(auth.ActionChildKey)).Delete("/tenants/{id}/keys/{key}", s.handlePlatformDeleteKey)
		r.With(s.requireRole(auth.ActionChildKey)).Post("/tenants/{id}/sts", s.handlePlatformCreateSTS)

		// WP-KEY-1: an app's management key, minted by its platform.
		s.registerPlatformManagementKeyRoutes(r)
	})
}

// registerAdminPlatformRoutes mounts the grant inside /api/v1/admin.
func (s *Server) registerAdminPlatformRoutes(r chi.Router) {
	r.Put("/tenants/{tenant_id}/platform", s.requireAdmin(s.handleAdminSetPlatform))
	r.Put("/tenants/{tenant_id}/residency", s.requireAdmin(s.handleAdminSetResidency)) // WP-ACCT-1
}

// requirePlatform answers the unknown route's 404 unless the JWT's tenant
// is a platform.
func (s *Server) requirePlatform(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID, _ := r.Context().Value(tenantIDKey).(string)
		if tenantID == "" || s.db == nil {
			writeManagementError(w, ErrTypeNotFound, "not_found", "no such route", "")
			return
		}
		// An app's key acts here as the app over its own children (WP-KEY-1).
		if actsAsApp(r.Context()) != nil {
			next.ServeHTTP(w, r)
			return
		}
		ok, err := auth.IsPlatform(r.Context(), s.db, tenantID)
		if err != nil {
			s.logger.Error("platform gate: lookup failed", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "the account could not be read", "")
			return
		}
		if !ok {
			writeManagementError(w, ErrTypeNotFound, "not_found", "no such route", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// platformChild is a child tenant as the API answers it.
type platformChild struct {
	Object     string `json:"object"`
	ID         string `json:"id"`
	Label      string `json:"label"`
	ExternalID string `json:"external_id,omitempty"`
	// Kind is the child's edge (managed | app, WP-ACCT-1); ParentID its
	// parent (the platform, or one of its apps).
	Kind     string         `json:"kind"`
	ParentID string         `json:"parent_id"`
	Caps     auth.ChildCaps `json:"caps"`
	// Residency is where the child's bytes may be stored: its own
	// tenants.residency (WP-ACCT-1, inherited from its parent), else — no
	// residency, anywhere — the residency of the deployment's default
	// bucket region, where its buckets default.
	Residency           string        `json:"residency"`
	CreatedAt           time.Time     `json:"created_at"`
	DeletionScheduledAt *time.Time    `json:"deletion_scheduled_at,omitempty"`
	Usage               platformUsage `json:"usage"`
	RequestID           string        `json:"request_id,omitempty"`
}

// platformUsage is a child's usage: its own counter (tenant_quotas), the
// bytes that count in its parent's pool.
type platformUsage struct {
	StorageUsedBytes int64 `json:"storage_used_bytes"`
}

// platformResidency is the residency a child's buckets default to.
func platformResidency() string {
	return drivers.DataResidency(drivers.IDriveDefaultRegion(os.Getenv))
}

// childResidency is the answer's residency: the child's own, else the
// default region's.
func childResidency(own string) string {
	if own != "" {
		return own
	}
	return platformResidency()
}

const platformChildColumns = `t.id, COALESCE(t.label, t.name), COALESCE(t.external_id, ''), COALESCE(t.created_at, NOW()),
	t.deletion_scheduled_at, COALESCE(q.storage_used_bytes, 0), COALESCE(t.child_kind, 'managed'), COALESCE(t.parent_tenant_id, ''),
	COALESCE(q.storage_limit_bytes, 0), COALESCE(q.egress_cap_bytes, 0), COALESCE(t.residency, '')`

func scanPlatformChild(sc interface{ Scan(...any) error }) (platformChild, error) {
	c := platformChild{Object: "tenant"}
	var del *time.Time
	var residency string
	if err := sc.Scan(&c.ID, &c.Label, &c.ExternalID, &c.CreatedAt, &del, &c.Usage.StorageUsedBytes,
		&c.Kind, &c.ParentID, &c.Caps.StorageBytes, &c.Caps.EgressBytes, &residency); err != nil {
		return c, err
	}
	c.Residency = childResidency(residency)
	c.DeletionScheduledAt = del
	return c, nil
}

// handlePlatformCreateTenant is POST /api/v1/platform/tenants
// {label, external_id?}.
func (s *Server) handlePlatformCreateTenant(w http.ResponseWriter, r *http.Request) {
	parent, _ := r.Context().Value(tenantIDKey).(string)
	var req struct {
		Label      string `json:"label"`
		ExternalID string `json:"external_id"`
		Kind       string `json:"kind"`
		ParentID   string `json:"parent_id"`
		StorageCap int64  `json:"storage_cap_bytes"`
		EgressCap  int64  `json:"egress_cap_bytes"`
		Residency  string `json:"residency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	opts := auth.ChildOptions{Kind: auth.ChildKind(req.Kind), ParentID: req.ParentID,
		Caps: auth.ChildCaps{StorageBytes: req.StorageCap, EgressBytes: req.EgressCap}, Residency: req.Residency}
	// An app's key (WP-KEY-1) makes the app's own managed children: its
	// platform makes them, under the app (depth 2 stays the deepest).
	if k := actsAsApp(r.Context()); k != nil {
		switch {
		case req.Kind != "" && req.Kind != string(auth.ChildManaged):
			writeManagementError(w, ErrTypeInvalidRequest, "child_of_child", "an app's children are managed: depth 2 is the deepest", "kind")
			return
		case req.ParentID != "" && req.ParentID != parent:
			writeManagementError(w, ErrTypeNotFound, "parent_not_found", "an app's key makes the app's own children only", "parent_id")
			return
		}
		opts.ParentID, parent = parent, k.ParentTenantID
	}
	c, err := auth.CreateChildTenant(r.Context(), s.db, parent, req.Label, req.ExternalID, opts)
	switch {
	case errors.Is(err, auth.ErrBadChildKind):
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_kind", err.Error(), "kind")
		return
	case errors.Is(err, auth.ErrBadCap):
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_cap", err.Error(), "storage_cap_bytes")
		return
	case errors.Is(err, auth.ErrBadResidency):
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_residency", err.Error(), "residency")
		return
	case errors.Is(err, auth.ErrResidencyWidens):
		writeManagementError(w, ErrTypeInvalidRequest, "residency_widens",
			"a child keeps its parent's residency: it may narrow it, never widen it", "residency")
		return
	case errors.Is(err, auth.ErrTenantNotFound) && req.ParentID != "":
		writeManagementError(w, ErrTypeNotFound, "parent_not_found", "parent_id is not an app of this platform", "parent_id")
		return
	case errors.Is(err, auth.ErrBadChildLabel):
		writeManagementError(w, ErrTypeInvalidRequest, "label_required", err.Error(), "label")
		return
	case errors.Is(err, auth.ErrBadExternalID):
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_external_id", err.Error(), "external_id")
		return
	case errors.Is(err, auth.ErrExternalIDTaken):
		writeManagementError(w, ErrTypeConflict, "external_id_taken", err.Error(), "external_id")
		return
	case errors.Is(err, auth.ErrChildOfChild):
		writeManagementError(w, ErrTypeInvalidRequest, "child_of_child", err.Error(), "")
		return
	case errors.Is(err, auth.ErrTenantSuspended):
		writeManagementError(w, ErrTypePermission, "account_suspended", "the account is suspended: no child can be made", "")
		return
	case errors.Is(err, auth.ErrNotPlatform), errors.Is(err, auth.ErrTenantNotFound):
		writeManagementError(w, ErrTypeNotFound, "not_found", "no such route", "") // revoked meanwhile
		return
	case err != nil:
		s.logger.Error("platform: create child", zap.Error(err), zap.String("parent", parent))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to create the tenant", "")
		return
	}
	// The child reads its platform's inherited flags (a sealing default)
	// from its first request, not the next refresh's (RP82-4).
	if s.flags != nil {
		if err := s.flags.Refresh(r.Context()); err != nil {
			s.logger.Warn("platform: refresh flags after a child", zap.Error(err))
		}
	}
	emitEvent(r.Context(), s.db, s.logger, "tenant.created", parent, map[string]interface{}{
		"tenant_id": c.ID, "label": c.Label, "external_id": c.ExternalID,
	})
	writeJSON(w, http.StatusCreated, platformChild{Object: "tenant", ID: c.ID, Label: c.Label, ExternalID: c.ExternalID,
		Kind: string(c.Kind), ParentID: c.ParentID, Caps: c.Caps,
		Residency: childResidency(c.Residency), CreatedAt: time.Now().UTC(), RequestID: getRequestID(w)})
}

// handlePlatformPatchTenant is PATCH /api/v1/platform/tenants/{id}
// {storage_cap_bytes?, egress_cap_bytes?}: a tenant's caps inside the pool
// (WP-ACCT-1; 0 = the pool decides).
func (s *Server) handlePlatformPatchTenant(w http.ResponseWriter, r *http.Request) {
	parent, _ := r.Context().Value(tenantIDKey).(string)
	var req struct {
		StorageCap *int64 `json:"storage_cap_bytes"`
		EgressCap  *int64 `json:"egress_cap_bytes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := auth.SetChildCaps(r.Context(), s.db, parent, id, req.StorageCap, req.EgressCap); err != nil {
		switch {
		case errors.Is(err, auth.ErrBadCap):
			writeManagementError(w, ErrTypeInvalidRequest, "invalid_cap", err.Error(), "")
		case errors.Is(err, auth.ErrTenantNotFound):
			writeManagementError(w, ErrTypeNotFound, "tenant_not_found", "no such tenant of this platform", "id")
		default:
			s.logger.Error("platform: set caps", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to set the caps", "")
		}
		return
	}
	if s.egress != nil {
		s.egress.forgetMember(id) // the new egress cap applies at once
	}
	c, err := s.platformChildOf(r, parent, id)
	if err != nil || c == nil {
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to read the tenant", "")
		return
	}
	c.RequestID = getRequestID(w)
	writeJSON(w, http.StatusOK, c)
}

// handlePlatformListTenants is GET /api/v1/platform/tenants
// (?limit=1-100&starting_after=<id>), by id.
func (s *Server) handlePlatformListTenants(w http.ResponseWriter, r *http.Request) {
	parent, _ := r.Context().Value(tenantIDKey).(string)
	limit := 20
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 100 {
		limit = l
	}
	after := r.URL.Query().Get("starting_after")
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT `+platformChildColumns+`
		  FROM tenants t LEFT JOIN tenant_quotas q ON q.tenant_id = t.id
		 WHERE `+auth.DescendantT1+` AND t.id > $2
		 ORDER BY t.id LIMIT $3`, parent, after, limit+1)
	if err != nil {
		s.logger.Error("platform: list children", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list the tenants", "")
		return
	}
	defer func() { _ = rows.Close() }()
	var items []interface{}
	for rows.Next() {
		c, err := scanPlatformChild(rows)
		if err != nil {
			s.logger.Error("platform: list children", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list the tenants", "")
			return
		}
		items = append(items, c)
	}
	if err := rows.Err(); err != nil {
		s.logger.Error("platform: list children", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list the tenants", "")
		return
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	next := ""
	if hasMore {
		next = items[len(items)-1].(platformChild).ID
	}
	writeListResponse(w, items, hasMore, next, len(items))
}

// platformChildOf reads the caller's child id (nil, nil when it is not
// the caller's).
func (s *Server) platformChildOf(r *http.Request, parent, id string) (*platformChild, error) {
	row := s.db.QueryRowContext(r.Context(), `
		SELECT `+platformChildColumns+`
		  FROM tenants t LEFT JOIN tenant_quotas q ON q.tenant_id = t.id
		 WHERE t.id = $1 AND `+auth.DescendantT2, id, parent)
	c, err := scanPlatformChild(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// handlePlatformGetTenant is GET /api/v1/platform/tenants/{id}.
func (s *Server) handlePlatformGetTenant(w http.ResponseWriter, r *http.Request) {
	parent, _ := r.Context().Value(tenantIDKey).(string)
	c, err := s.platformChildOf(r, parent, chi.URLParam(r, "id"))
	if err != nil {
		s.logger.Error("platform: read child", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to read the tenant", "")
		return
	}
	if c == nil {
		writeManagementError(w, ErrTypeNotFound, "tenant_not_found", "no tenant of this platform has that id", "id")
		return
	}
	c.RequestID = getRequestID(w)
	writeJSON(w, http.StatusOK, c)
}

// handlePlatformDeleteTenant is DELETE /api/v1/platform/tenants/{id}: the
// child's erasure, scheduled with the ordinary grace (30 days) — the
// deletion runner erases it on the date.
func (s *Server) handlePlatformDeleteTenant(w http.ResponseWriter, r *http.Request) {
	parent, _ := r.Context().Value(tenantIDKey).(string)
	id := chi.URLParam(r, "id")
	c, err := s.platformChildOf(r, parent, id)
	if err != nil {
		s.logger.Error("platform: read child", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to read the tenant", "")
		return
	}
	if c == nil {
		writeManagementError(w, ErrTypeNotFound, "tenant_not_found", "no tenant of this platform has that id", "id")
		return
	}
	at, err := account.NewService(s.db, s.logger).ScheduleTenant(r.Context(), id)
	if err != nil {
		s.logger.Error("platform: schedule child deletion", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to schedule the deletion", "")
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: parent, EventType: "platform", Action: "platform.tenant_deletion_scheduled",
		Resource: "tenant:" + id, Metadata: map[string]any{"child": id, "scheduled_at": at}})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "tenant", "id": id, "deletion_scheduled": true, "deletion_scheduled_at": at.UTC(),
		"request_id": getRequestID(w),
	})
}

// handleAdminSetPlatform is PUT /api/v1/admin/tenants/{tenant_id}/platform
// {"platform": bool}: grants (or revokes) the platform flag, audited
// admin.platform_granted / admin.platform_revoked with the admin as actor.
func (s *Server) handleAdminSetPlatform(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenant_id")
	var req struct {
		Platform *bool `json:"platform"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Platform == nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", `the body must be {"platform": true|false}`, "platform")
		return
	}
	err := auth.SetPlatform(r.Context(), s.db, tenantID, *req.Platform)
	switch {
	case errors.Is(err, auth.ErrTenantNotFound):
		writeManagementError(w, ErrTypeNotFound, "tenant_not_found", "no such tenant", "tenant_id")
		return
	case errors.Is(err, auth.ErrChildOfChild):
		writeManagementError(w, ErrTypeInvalidRequest, "child_of_child", "a child or a sandbox is never a platform", "tenant_id")
		return
	case errors.Is(err, auth.ErrPlatformHasChildren):
		writeManagementError(w, ErrTypeConflict, "platform_has_children", err.Error()+": erase them first", "platform")
		return
	case err != nil:
		s.logger.Error("admin: set platform", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to set the platform flag", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"object": "tenant", "id": tenantID, "platform": *req.Platform,
		"request_id": getRequestID(w)})
}
