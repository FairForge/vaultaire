package handlers

import (
	"database/sql"
	"errors"
	"html/template"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/email"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// The Members page (WP-TEAM-1 PR B, Stage 1a Prompt 52): who may act on
// the session's active tenant, the pending invitations, and — for an owner
// or an admin (auth.ActionMemberManage) — invite, change a role, remove.
// The owner row is never removed or demoted. The writes are mounted behind
// RequireAction in router.go; the page itself is every member's.

// HandleMembers renders the page.
func HandleMembers(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		data := sessionData(sd, "members")
		withCSRF(r.Context(), data)
		withFlash(r.Context(), data)
		members, err := auth.ListMembers(r.Context(), db, sd.TenantID)
		if err != nil {
			logger.Error("dashboard: list members", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		invs, err := auth.ListInvitations(r.Context(), db, sd.TenantID)
		if err != nil {
			logger.Error("dashboard: list invitations", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		data["Members"] = members
		data["Invitations"] = invs
		data["Now"] = time.Now()
		// The switcher (PR C): the other tenants this user may act on.
		data["Accounts"] = []auth.Membership{}
		if mine, err := auth.Memberships(r.Context(), db, sd.UserID); err == nil && mine != nil {
			data["Accounts"] = mine
		}
		data["CanManage"] = auth.Can(auth.Role(sd.MemberRole), auth.ActionMemberManage)
		data["MyRole"] = sd.MemberRole
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("dashboard: render members", zap.Error(err))
		}
	}
}

// HandleInviteMember sends an invitation.
func HandleInviteMember(db *sql.DB, sender email.Sender, baseURL string, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		role, ok := auth.ParseRole(r.FormValue("role"))
		if !ok || role == auth.RoleOwner {
			middleware.SetFlash(w, "error", "Choose admin, developer or viewer.")
			http.Redirect(w, r, "/dashboard/members", http.StatusSeeOther)
			return
		}
		inv, token, err := auth.CreateInvitation(r.Context(), db, sd.TenantID, sd.UserID, r.FormValue("email"), role, time.Now())
		switch {
		case errors.Is(err, auth.ErrAlreadyMember):
			middleware.SetFlash(w, "error", "That address is already a member.")
		case err != nil && err.Error() == "invalid email address":
			middleware.SetFlash(w, "error", "Enter an e-mail address.")
		case err != nil:
			logger.Error("dashboard: invite member", zap.Error(err))
			middleware.SetFlash(w, "error", "The invitation could not be made. Try again.")
		default:
			audit.Record(r.Context(), db, audit.Entry{TenantID: sd.TenantID, Action: "member.invited", Resource: "invitation:" + inv.ID,
				Metadata: map[string]any{"email": inv.Email, "role": string(inv.Role), "via": "dashboard"}})
			var account string
			_ = db.QueryRowContext(r.Context(), `SELECT COALESCE(NULLIF(name, ''), email) FROM tenants WHERE id = $1`, sd.TenantID).Scan(&account)
			html, text, rerr := email.RenderInvitation(baseURL, token, inv.Email, account, sd.Email, string(inv.Role))
			if rerr == nil && sender != nil {
				rerr = sender.Send(r.Context(), inv.Email, "You are invited to "+account+" on Stored", html, text)
			}
			if rerr != nil {
				logger.Warn("dashboard: invitation e-mail not sent", zap.String("invitation", inv.ID), zap.Error(rerr))
				middleware.SetFlash(w, "error", "The invitation was made but its e-mail could not be sent. Revoke it and try again.")
			} else {
				middleware.SetFlash(w, "success", "Invitation sent to "+inv.Email+". The link works once, for 7 days.")
			}
		}
		http.Redirect(w, r, "/dashboard/members", http.StatusSeeOther)
	}
}

// memberFlash is the page's message for a refused change.
func memberFlash(err error) string {
	switch {
	case errors.Is(err, auth.ErrOwnerMember):
		return "The owner cannot be removed or given another role."
	case errors.Is(err, auth.ErrNotMember):
		return "No such member."
	case errors.Is(err, auth.ErrInviteRole):
		return "Choose admin, developer or viewer."
	}
	return "The change could not be made. Try again."
}

// HandleSetMemberRole changes a member's role.
func HandleSetMemberRole(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		target := chi.URLParam(r, "user")
		role, _ := auth.ParseRole(r.FormValue("role"))
		if err := auth.SetMemberRole(r.Context(), db, sd.TenantID, target, role); err != nil {
			if !errors.Is(err, auth.ErrOwnerMember) && !errors.Is(err, auth.ErrNotMember) && !errors.Is(err, auth.ErrInviteRole) {
				logger.Error("dashboard: set member role", zap.Error(err))
			}
			middleware.SetFlash(w, "error", memberFlash(err))
		} else {
			audit.Record(r.Context(), db, audit.Entry{UserID: target, TenantID: sd.TenantID, Action: "member.role_changed",
				Resource: "user:" + target, Metadata: map[string]any{"role": string(role), "via": "dashboard"}})
			middleware.SetFlash(w, "success", "Role changed.")
		}
		http.Redirect(w, r, "/dashboard/members", http.StatusSeeOther)
	}
}

// HandleRemoveMember removes a member's row (never a user, never a tenant).
func HandleRemoveMember(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		target := chi.URLParam(r, "user")
		if err := auth.RemoveMember(r.Context(), db, sd.TenantID, target); err != nil {
			if !errors.Is(err, auth.ErrOwnerMember) && !errors.Is(err, auth.ErrNotMember) {
				logger.Error("dashboard: remove member", zap.Error(err))
			}
			middleware.SetFlash(w, "error", memberFlash(err))
		} else {
			audit.Record(r.Context(), db, audit.Entry{UserID: target, TenantID: sd.TenantID, Action: "member.removed",
				Resource: "user:" + target, Severity: "warning", Metadata: map[string]any{"via": "dashboard"}})
			middleware.SetFlash(w, "success", "Member removed.")
		}
		http.Redirect(w, r, "/dashboard/members", http.StatusSeeOther)
	}
}

// HandleRevokeInvitation deletes a pending invitation.
func HandleRevokeInvitation(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		id := chi.URLParam(r, "id")
		if err := auth.RevokeInvitation(r.Context(), db, sd.TenantID, id); err != nil {
			if !errors.Is(err, auth.ErrInvitationNotFound) {
				logger.Error("dashboard: revoke invitation", zap.Error(err))
			}
			middleware.SetFlash(w, "error", "No such pending invitation.")
		} else {
			audit.Record(r.Context(), db, audit.Entry{TenantID: sd.TenantID, Action: "member.invitation_revoked",
				Resource: "invitation:" + id, Metadata: map[string]any{"via": "dashboard"}})
			middleware.SetFlash(w, "success", "Invitation revoked.")
		}
		http.Redirect(w, r, "/dashboard/members", http.StatusSeeOther)
	}
}

// InvitationMessage is the accept page's sentence for a refusal.
func InvitationMessage(err error) string {
	switch {
	case errors.Is(err, auth.ErrInvitationExpired):
		return "This invitation has expired. Ask for a new one."
	case errors.Is(err, auth.ErrInvitationUsed):
		return "This invitation has already been used."
	case errors.Is(err, auth.ErrInvitationEmailMismatch):
		return "This invitation was sent to another address. Sign in as that address to accept it."
	}
	return "This invitation link is not valid."
}

// HandleInvitationPage shows an invitation to the signed-in user (the
// address that was invited accepts it here).
func HandleInvitationPage(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		data := sessionData(sd, "members")
		withCSRF(r.Context(), data)
		withFlash(r.Context(), data)
		inv, err := auth.InvitationByToken(r.Context(), db, chi.URLParam(r, "token"), time.Now())
		if err == nil && inv.Email != sd.Email {
			err = auth.ErrInvitationEmailMismatch
		}
		if err != nil {
			data["InvitationError"] = InvitationMessage(err)
		} else {
			var account string
			_ = db.QueryRowContext(r.Context(), `SELECT COALESCE(NULLIF(name, ''), email) FROM tenants WHERE id = $1`, inv.TenantID).Scan(&account)
			data["Invitation"] = inv
			data["Account"] = account
			data["Token"] = chi.URLParam(r, "token")
		}
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("dashboard: render invitation", zap.Error(err))
		}
	}
}

// HandleAcceptInvitation spends the invitation for the signed-in user.
func HandleAcceptInvitation(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		token := chi.URLParam(r, "token")
		inv, err := auth.AcceptInvitation(r.Context(), db, token, sd.UserID, sd.Email, time.Now())
		if err != nil {
			if !errors.Is(err, auth.ErrInvitationExpired) && !errors.Is(err, auth.ErrInvitationUsed) &&
				!errors.Is(err, auth.ErrInvitationEmailMismatch) && !errors.Is(err, auth.ErrInvitationNotFound) {
				logger.Error("dashboard: accept invitation", zap.Error(err))
			}
			middleware.SetFlash(w, "error", InvitationMessage(err))
			http.Redirect(w, r, "/dashboard/members", http.StatusSeeOther) // a fixed path: never one built from the request
			return
		}
		middleware.SetFlash(w, "success", "You are now a "+string(inv.Role)+" of this account. Switch to it on the Members page.")
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}

// HandleSwitchTenant moves the session to another tenant the user is a
// member of (WP-TEAM-1 PR C, the switcher): a new session on that tenant
// replaces this one (the cookie is rotated; the old token is deleted).
func HandleSwitchTenant(db *sql.DB, sessions dashauth.SessionStore, ttl time.Duration, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		target := r.FormValue("tenant_id")
		if _, err := auth.MemberRole(r.Context(), db, target, sd.UserID); err != nil {
			if !errors.Is(err, auth.ErrNotMember) {
				logger.Error("dashboard: switch tenant", zap.Error(err))
			}
			middleware.SetFlash(w, "error", "You are not a member of that account.")
			http.Redirect(w, r, "/dashboard/members", http.StatusSeeOther)
			return
		}
		next := *sd
		next.TenantID, next.MemberRole = target, ""
		next.IPAddress = middleware.ClientIP(r)
		next.UserAgent = dashauth.TruncateUserAgent(r.UserAgent())
		token, err := sessions.Create(r.Context(), next, ttl)
		if err != nil {
			logger.Error("dashboard: switch tenant: session", zap.Error(err))
			middleware.SetFlash(w, "error", "The switch failed. Try again.")
			http.Redirect(w, r, "/dashboard/members", http.StatusSeeOther)
			return
		}
		if c, err := r.Cookie(dashauth.SessionCookieName); err == nil {
			_ = sessions.Delete(r.Context(), c.Value)
		}
		dashauth.SetSessionCookie(w, token, ttl)
		audit.Record(r.Context(), db, audit.Entry{TenantID: target, UserID: sd.UserID, Action: "member.switched_tenant",
			Resource: "tenant:" + target, Metadata: map[string]any{"from": sd.TenantID}})
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}
