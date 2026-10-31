package handlers

// The Agents and Apps pages (WP-AGENT-1 / WP-APP-1 PR E): the agent grants
// (the agent, the person it acts for, its expiry; renew, revoke), the
// approvals agents asked for (approve with a TOTP code, deny); the apps
// (make — the backend key shown once — list, delete); and a claim link's
// page. Each write is mounted through dashboardAction (router.go). The
// rules are the API's (internal/auth); a page of a flag that is off is 404.

import (
	"database/sql"
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
)

func renderPage(w http.ResponseWriter, tmpl *template.Template, data map[string]any, status int, logger *zap.Logger) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
		logger.Error("render", zap.Error(err))
	}
}

func sessionRole(sd *dashauth.SessionData) auth.Role {
	if sd.MemberRole != "" {
		return auth.Role(sd.MemberRole)
	}
	return auth.RoleOwner
}

// HandleAgentsPage is GET /dashboard/agents.
func HandleAgentsPage(tmpl *template.Template, db *sql.DB, enabled func(string) bool, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil || db == nil || enabled == nil || !enabled(sd.TenantID) {
			http.NotFound(w, r)
			return
		}
		data := sessionData(sd, "agents")
		withCSRF(r.Context(), data)
		withFlash(r.Context(), data)
		grants, err := auth.ListAgentGrants(r.Context(), db, sd.TenantID)
		if err != nil {
			logger.Error("agents page: grants", zap.Error(err))
		}
		approvals, err := auth.ListApprovals(r.Context(), db, sd.TenantID, false)
		if err != nil {
			logger.Error("agents page: approvals", zap.Error(err))
		}
		data["Grants"], data["Approvals"] = grants, approvals
		data["Focus"] = r.URL.Query().Get("approval")
		data["CanManage"] = auth.Can(sessionRole(sd), auth.ActionManagementKey)
		renderPage(w, tmpl, data, http.StatusOK, logger)
	}
}

// HandleAgentRenew is POST /dashboard/agents/{id}/renew.
func HandleAgentRenew(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if _, err := auth.RenewAgentGrant(r.Context(), db, sd.TenantID, chi.URLParam(r, "id")); err != nil {
			logger.Warn("renew an agent grant", zap.Error(err))
			middleware.SetFlash(w, "error", "The grant could not be renewed.")
		} else {
			audit.Record(r.Context(), db, audit.Entry{TenantID: sd.TenantID, Actor: sd.UserID, Action: "agent.grant_renewed", Resource: "agent:" + chi.URLParam(r, "id")})
			middleware.SetFlash(w, "success", "Renewed for 30 days.")
		}
		http.Redirect(w, r, "/dashboard/agents", http.StatusSeeOther)
	}
}

// HandleAgentRevoke is POST /dashboard/agents/{id}/revoke: both keys of the
// grant, and every S3 key it minted.
func HandleAgentRevoke(db *sql.DB, authSvc *auth.AuthService, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		id := chi.URLParam(r, "id")
		minted, err := auth.RevokeAgentGrant(r.Context(), db, sd.TenantID, id)
		if err != nil {
			logger.Warn("revoke an agent grant", zap.Error(err))
			middleware.SetFlash(w, "error", "The grant could not be revoked.")
			http.Redirect(w, r, "/dashboard/agents", http.StatusSeeOther)
			return
		}
		for _, tk := range minted {
			t, k, _ := strings.Cut(tk, "/")
			if err := authSvc.RevokeAPIKey(r.Context(), t, k); err != nil && !errors.Is(err, auth.ErrKeyRevoked) {
				logger.Warn("revoke an agent's S3 key", zap.Error(err))
			}
		}
		audit.Record(r.Context(), db, audit.Entry{TenantID: sd.TenantID, Actor: sd.UserID, Action: "agent.grant_revoked", Resource: "agent:" + id})
		middleware.SetFlash(w, "success", "Revoked: the agent's keys stopped working.")
		http.Redirect(w, r, "/dashboard/agents", http.StatusSeeOther)
	}
}

// HandleApprovalDecide is POST /dashboard/approvals/{id}/{decision}.
func HandleApprovalDecide(db *sql.DB, authSvc *auth.AuthService, mfa *auth.MFAService, decision string, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		id := chi.URLParam(r, "id")
		back := "/dashboard/agents?approval=" + id
		ap, err := auth.GetApproval(r.Context(), db, sd.TenantID, id)
		if err != nil {
			middleware.SetFlash(w, "error", "No such request.")
			http.Redirect(w, r, "/dashboard/agents", http.StatusSeeOther)
			return
		}
		act := auth.ApprovalActions[ap.Action]
		if decision == "approved" {
			if act.Role != "" && !auth.Can(sessionRole(sd), act.Role) {
				middleware.SetFlash(w, "error", "Your role may not "+act.Says+" yourself, so it may not approve an agent doing it.")
				http.Redirect(w, r, back, http.StatusSeeOther)
				return
			}
			if act.Destructive {
				code := strings.TrimSpace(r.FormValue("totp_code"))
				secret, err := authSvc.GetMFASecret(r.Context(), sd.UserID)
				if code == "" || err != nil || secret == "" || mfa == nil || !mfa.ValidateCode(secret, code) || !authSvc.ConsumeTOTPCode(sd.UserID, code) {
					middleware.SetFlash(w, "error", "Enter the current code of your authenticator app to approve this.")
					http.Redirect(w, r, back, http.StatusSeeOther)
					return
				}
			}
		}
		if _, err := auth.DecideApproval(r.Context(), db, sd.TenantID, id, sd.UserID, decision); err != nil {
			middleware.SetFlash(w, "error", "That request was decided already, or its 24 hours have passed.")
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
		audit.Record(r.Context(), db, audit.Entry{TenantID: sd.TenantID, Actor: sd.UserID, Action: "agent.approval_" + decision, Resource: ap.Resource,
			Metadata: map[string]any{"approval_id": id, "action": ap.Action}})
		if decision == "approved" {
			middleware.SetFlash(w, "success", "Approved once: the agent's next identical request goes through.")
		} else {
			middleware.SetFlash(w, "success", "Denied.")
		}
		http.Redirect(w, r, "/dashboard/agents", http.StatusSeeOther)
	}
}

// HandleAppsPage is GET and POST /dashboard/apps (POST makes one).
func HandleAppsPage(tmpl *template.Template, db *sql.DB, enabled func(string) bool, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil || db == nil || enabled == nil || !enabled(sd.TenantID) {
			http.NotFound(w, r)
			return
		}
		data := sessionData(sd, "apps")
		withCSRF(r.Context(), data)
		withFlash(r.Context(), data)
		data["CanManage"] = auth.Can(sessionRole(sd), auth.ActionChildWrite)
		status := http.StatusOK
		if r.Method == http.MethodPost {
			c, err := auth.CreateChildTenant(r.Context(), db, sd.TenantID, r.FormValue("label"), "", auth.ChildOptions{Kind: auth.ChildApp, SelfServe: true})
			switch {
			case errors.Is(err, auth.ErrAppsNeedVerified):
				data["Error"], status = "Verify your e-mail address first: apps are made by a verified account.", http.StatusForbidden
			case errors.Is(err, auth.ErrAppsNeedPaying):
				data["Error"], status = "Apps are made by a paying account: buy storage first.", http.StatusForbidden
			case errors.Is(err, auth.ErrAppLimit):
				data["Error"], status = "An account holds at most 25 apps. Delete one first.", http.StatusConflict
			case errors.Is(err, auth.ErrBadChildLabel):
				data["Error"], status = "Give the app a name (up to 200 characters).", http.StatusBadRequest
			case err != nil:
				logger.Error("apps page: create", zap.Error(err))
				data["Error"], status = "The app could not be made. Try again.", http.StatusInternalServerError
			default:
				k, secret, err := auth.MintManagementKey(r.Context(), db, c.ID, auth.ManagementKeyOptions{Name: "app backend: " + c.Label,
					Scopes: []string{auth.ScopeApps, auth.ScopeManageWrite}, CreatedBy: sd.UserID})
				if err != nil {
					logger.Error("apps page: the app's key", zap.Error(err))
					data["Error"], status = "The app was made but its key was not: delete it and try again.", http.StatusInternalServerError
				} else {
					audit.Record(r.Context(), db, audit.Entry{TenantID: sd.TenantID, Actor: sd.UserID, Action: "app.created", Resource: "tenant:" + c.ID,
						Metadata: map[string]any{"label": c.Label, "management_key_id": k.ID}})
					data["NewApp"], data["NewSecret"] = c, secret
					status = http.StatusCreated
				}
			}
		}
		apps, err := listApps(r, db, sd.TenantID)
		if err != nil {
			logger.Error("apps page: list", zap.Error(err))
		}
		data["Apps"] = apps
		renderPage(w, tmpl, data, status, logger)
	}
}

type appRow struct {
	ID, Label        string
	StorageUsedBytes int64
	Scheduled        bool
}

func listApps(r *http.Request, db *sql.DB, tenantID string) ([]appRow, error) {
	rows, err := db.QueryContext(r.Context(), `SELECT t.id, COALESCE(t.label, t.name), COALESCE(q.storage_used_bytes, 0), t.deletion_scheduled_at IS NOT NULL
		  FROM tenants t LEFT JOIN tenant_quotas q ON q.tenant_id = t.id
		 WHERE t.parent_tenant_id = $1 AND t.child_kind = 'app' ORDER BY t.created_at, t.id LIMIT 100`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []appRow
	for rows.Next() {
		var a appRow
		if err := rows.Scan(&a.ID, &a.Label, &a.StorageUsedBytes, &a.Scheduled); err != nil {
			return out, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// HandleAppDelete is POST /dashboard/apps/{id}/delete: the app and its end
// users scheduled (the cascade), its keys revoked at once.
func HandleAppDelete(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		id := chi.URLParam(r, "id")
		var isApp bool
		_ = db.QueryRowContext(r.Context(), `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1 AND parent_tenant_id = $2 AND child_kind = 'app')`, id, sd.TenantID).Scan(&isApp)
		if !isApp {
			middleware.SetFlash(w, "error", "No such app.")
			http.Redirect(w, r, "/dashboard/apps", http.StatusSeeOther)
			return
		}
		svc := account.NewService(db, logger)
		if _, err := svc.ScheduleTenant(r.Context(), id); err != nil {
			logger.Error("apps page: schedule", zap.Error(err))
			middleware.SetFlash(w, "error", "The app could not be deleted. Try again.")
			http.Redirect(w, r, "/dashboard/apps", http.StatusSeeOther)
			return
		}
		if _, err := db.ExecContext(r.Context(), `UPDATE tenants SET deletion_scheduled_at = COALESCE(deletion_scheduled_at, (SELECT deletion_scheduled_at FROM tenants WHERE id = $1))
			 WHERE parent_tenant_id = $1`, id); err != nil {
			logger.Warn("apps page: schedule the end users (the cascade erases them with the app)", zap.Error(err))
		}
		if _, err := db.ExecContext(r.Context(), `UPDATE management_keys SET revoked_at = NOW() WHERE tenant_id = $1 AND revoked_at IS NULL`, id); err != nil {
			logger.Error("apps page: revoke the app's keys", zap.Error(err))
		}
		audit.Record(r.Context(), db, audit.Entry{TenantID: sd.TenantID, Actor: sd.UserID, Action: "app.deletion_scheduled", Resource: "tenant:" + id})
		middleware.SetFlash(w, "success", "Deleted: the app and its users are erased after the 30-day grace; its keys stopped now.")
		http.Redirect(w, r, "/dashboard/apps", http.StatusSeeOther)
	}
}

// HandleClaim is GET and POST /dashboard/claim/{token}.
func HandleClaim(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		token := chi.URLParam(r, "token")
		data := sessionData(sd, "apps")
		withCSRF(r.Context(), data)
		data["Token"] = token
		if db == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			if !auth.Can(sessionRole(sd), auth.ActionChildWrite) {
				data["Error"] = "Your role on this account may not keep an app: ask its owner."
				renderPage(w, tmpl, data, http.StatusForbidden, logger)
				return
			}
			c, err := auth.ClaimProvisional(r.Context(), db, token, sd.TenantID, sd.UserID)
			switch {
			case errors.Is(err, auth.ErrClaimGone):
				data["Gone"] = true
				renderPage(w, tmpl, data, http.StatusGone, logger)
				return
			case errors.Is(err, auth.ErrClaimNeedsVerified):
				data["Error"] = "Verify your e-mail address first, then open the link again."
				renderPage(w, tmpl, data, http.StatusForbidden, logger)
				return
			case err != nil:
				logger.Error("claim", zap.Error(err))
				data["Error"] = "The app could not be claimed. Try again."
				renderPage(w, tmpl, data, http.StatusInternalServerError, logger)
				return
			}
			middleware.SetFlash(w, "success", "\""+c.Label+"\" is yours now: it is an app of this account.")
			http.Redirect(w, r, "/dashboard/apps", http.StatusSeeOther)
			return
		}
		c, err := auth.LookupClaim(r.Context(), db, token)
		if err != nil {
			data["Gone"] = true
			renderPage(w, tmpl, data, http.StatusGone, logger)
			return
		}
		data["Claim"] = c
		renderPage(w, tmpl, data, http.StatusOK, logger)
	}
}
