// internal/dashboard/handlers/family.go
//
// The Family page (WP-FAMILY-1 PR C, Stage 1a Prompt 56; docs/ACCOUNTS.md §
// A family). The organizer sees each member's usage and cap, the pool's
// total and the pending invitations, and may invite (a fresh authenticator
// code), cap and remove; a member sees whose family it is in, and leaves.
// Nothing here reads a member's buckets, objects, keys or requests. Every
// write is an owner's (auth.ActionBilling: the pool is paid for) through
// dashboardAction. The join page is a link's: a sign-in returns to it
// (dashauth.ReturnToCookie's allow-list).
package handlers

import (
	"context"
	"database/sql"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/email"
	"github.com/FairForge/vaultaire/internal/usage"
)

const familyPath = "/dashboard/family"

// FamilyDeps is what the Family pages need.
type FamilyDeps struct {
	DB      *sql.DB
	Auth    *auth.AuthService
	MFA     *auth.MFAService
	Email   email.Sender
	BaseURL string
	// Enabled reports the family flag of a tenant (the organizer's).
	Enabled func(tenantID string) bool
	Logger  *zap.Logger
}

// rootOf is the pool root of tenantID ("" when unknown).
func (d FamilyDeps) rootOf(ctx context.Context, tenantID string) string {
	if d.DB == nil {
		return ""
	}
	root, _ := usage.PoolRoot(ctx, d.DB, tenantID)
	return root
}

func (d FamilyDeps) on(ctx context.Context, tenantID string) bool {
	root := d.rootOf(ctx, tenantID)
	return root != "" && d.Enabled != nil && d.Enabled(root)
}

// familyRow is one member as the page shows it.
type familyRow struct {
	ID, Label, Used, Cap string
	CapGiB               string
	Joined               time.Time
}

// HandleFamilyPage is GET /dashboard/family.
func HandleFamilyPage(tmpl *template.Template, d FamilyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil || d.DB == nil || !d.on(r.Context(), sd.TenantID) {
			http.NotFound(w, r)
			return
		}
		// Members' addresses and usage: the billing role's (RP92-6) — a
		// viewer of the organizer's team sees no family.
		if !auth.Can(sessionRole(sd), auth.ActionBilling) {
			http.Error(w, "Your role on this account ("+string(sessionRole(sd))+") may not see its family.", http.StatusForbidden)
			return
		}
		data := sessionData(sd, "family")
		withCSRF(r.Context(), data)
		data["Owner"] = sessionRole(sd) == auth.RoleOwner
		v, err := auth.Family(r.Context(), d.DB, sd.TenantID)
		if err != nil {
			d.Logger.Error("family page", zap.Error(err))
			http.Error(w, "the family could not be read", http.StatusInternalServerError)
			return
		}
		data["Role"] = v.Role
		data["Organizer"] = v.Organizer
		var rows []familyRow
		for _, m := range v.Members {
			row := familyRow{ID: m.ID, Label: m.Label, Used: formatBytes(m.StorageUsed), Cap: "the pool decides", Joined: m.JoinedAt}
			if m.StorageLimit != nil {
				row.Cap = formatBytes(*m.StorageLimit)
				row.CapGiB = strconv.FormatFloat(float64(*m.StorageLimit)/float64(1<<30), 'f', -1, 64)
			}
			rows = append(rows, row)
		}
		data["Members"] = rows
		data["Invitations"] = v.Invitations
		data["PoolUsed"] = formatBytes(v.PoolUsed)
		data["PoolLimit"] = formatBytes(v.PoolLimit)
		data["People"] = 1 + len(v.Members) + len(v.Invitations)
		data["MaxPeople"] = auth.MaxFamilyPeople
		renderPage(w, tmpl, data, http.StatusOK, d.Logger)
	}
}

// familyOwnerWrite is the check of every organizer write (false: answered).
func (d FamilyDeps) familyOwnerWrite(w http.ResponseWriter, r *http.Request) (*dashauth.SessionData, bool) {
	sd := dashauth.GetSession(r.Context())
	if sd == nil || d.DB == nil || !d.on(r.Context(), sd.TenantID) {
		http.NotFound(w, r)
		return nil, false
	}
	if sessionRole(sd) != auth.RoleOwner {
		middleware.SetFlash(w, "error", "Only the account's owner acts for its family.")
		http.Redirect(w, r, familyPath, http.StatusSeeOther)
		return nil, false
	}
	return sd, true
}

// HandleFamilyInvite is POST /dashboard/family/invite {email, totp_code}.
func HandleFamilyInvite(d FamilyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd, ok := d.familyOwnerWrite(w, r)
		if !ok {
			return
		}
		var v auth.TOTPValidator
		if d.MFA != nil {
			v = d.MFA
		}
		if d.Auth == nil {
			http.NotFound(w, r)
			return
		}
		if err := d.Auth.CheckFreshTOTP(r.Context(), v, sd.UserID, strings.TrimSpace(r.FormValue("totp_code")), "family_invite"); err != nil {
			msg := "Enter the current code of your authenticator app to invite someone."
			if errors.Is(err, auth.ErrTOTPLocked) {
				msg = "Too many wrong codes: wait 15 minutes, then try again."
			}
			middleware.SetFlash(w, "error", msg)
			http.Redirect(w, r, familyPath, http.StatusSeeOther)
			return
		}
		inv, token, err := auth.InviteFamilyMember(r.Context(), d.DB, sd.TenantID, sd.UserID, r.FormValue("email"), time.Now())
		if err != nil {
			middleware.SetFlash(w, "error", familyFlash(err))
			http.Redirect(w, r, familyPath, http.StatusSeeOther)
			return
		}
		if d.Email != nil {
			html, text, rerr := email.RenderFamilyInvitation(d.BaseURL, token, inv.Email, sd.Email)
			if rerr == nil {
				rerr = d.Email.Send(r.Context(), inv.Email, sd.Email+" invites you to their family on Stored", html, text)
			}
			if rerr != nil {
				d.Logger.Warn("family invitation: the e-mail was not sent", zap.String("invitation", inv.ID), zap.Error(rerr))
			}
		}
		middleware.SetFlash(w, "success", "Invited "+inv.Email+": the link works once, for 7 days.")
		http.Redirect(w, r, familyPath, http.StatusSeeOther)
	}
}

// HandleFamilyCap is POST /dashboard/family/members/{id}/cap {cap_gib}
// (empty = the pool decides).
func HandleFamilyCap(d FamilyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd, ok := d.familyOwnerWrite(w, r)
		if !ok {
			return
		}
		var limit int64
		if raw := strings.TrimSpace(r.FormValue("cap_gib")); raw != "" {
			gib, err := strconv.ParseFloat(raw, 64)
			if err != nil || gib < 0 || gib > 1<<20 {
				middleware.SetFlash(w, "error", "A cap is a number of GiB, or empty for none.")
				http.Redirect(w, r, familyPath, http.StatusSeeOther)
				return
			}
			limit = int64(gib * float64(1<<30))
		}
		member := chi.URLParam(r, "id")
		if err := account.NewService(d.DB, d.Logger).SetFamilyMemberCap(r.Context(), sd.TenantID, member, limit); err != nil {
			middleware.SetFlash(w, "error", familyFlash(err))
			http.Redirect(w, r, familyPath, http.StatusSeeOther)
			return
		}
		audit.Record(r.Context(), d.DB, audit.Entry{TenantID: sd.TenantID, Actor: sd.UserID, Action: "family.member_capped",
			Resource: "tenant:" + member, Metadata: map[string]any{"storage_limit_bytes": limit, "via": "dashboard"}})
		middleware.SetFlash(w, "success", "Cap saved.")
		http.Redirect(w, r, familyPath, http.StatusSeeOther)
	}
}

// HandleFamilyRemove is POST /dashboard/family/members/{id}/remove.
func HandleFamilyRemove(d FamilyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd, ok := d.familyOwnerWrite(w, r)
		if !ok {
			return
		}
		member := chi.URLParam(r, "id")
		if err := account.NewService(d.DB, d.Logger).RemoveFamilyMember(r.Context(), sd.TenantID, member, usage.FreeTierLimits.StorageBytes); err != nil {
			if !errors.Is(err, account.ErrNotAFamilyMember) {
				d.Logger.Error("family page: remove", zap.Error(err))
			}
			middleware.SetFlash(w, "error", familyFlash(err))
			http.Redirect(w, r, familyPath, http.StatusSeeOther)
			return
		}
		audit.Record(r.Context(), d.DB, audit.Entry{TenantID: sd.TenantID, Actor: sd.UserID, Action: "family.member_removed",
			Resource: "tenant:" + member, Metadata: map[string]any{"via": "dashboard"}})
		middleware.SetFlash(w, "success", "Removed: their account and files are theirs, on the free tier.")
		http.Redirect(w, r, familyPath, http.StatusSeeOther)
	}
}

// HandleFamilyLeave is POST /dashboard/family/leave (the member's own).
func HandleFamilyLeave(d FamilyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil || d.DB == nil || !d.on(r.Context(), sd.TenantID) {
			http.NotFound(w, r)
			return
		}
		if sessionRole(sd) != auth.RoleOwner {
			middleware.SetFlash(w, "error", "Only the account's owner takes it out of a family.")
			http.Redirect(w, r, familyPath, http.StatusSeeOther)
			return
		}
		organizer, err := account.NewService(d.DB, d.Logger).LeaveFamily(r.Context(), sd.TenantID, usage.FreeTierLimits.StorageBytes)
		if err != nil {
			if !errors.Is(err, account.ErrNotAFamilyMember) {
				d.Logger.Error("family page: leave", zap.Error(err))
			}
			middleware.SetFlash(w, "error", familyFlash(err))
			http.Redirect(w, r, familyPath, http.StatusSeeOther)
			return
		}
		audit.Record(r.Context(), d.DB, audit.Entry{TenantID: organizer, Actor: sd.UserID, Action: "family.member_left",
			Resource: "tenant:" + sd.TenantID, Metadata: map[string]any{"via": "dashboard"}})
		middleware.SetFlash(w, "success", "You left the family: your account and files are yours, on the free tier.")
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}

// HandleFamilyJoin is GET and POST /dashboard/family/join/{token}.
func HandleFamilyJoin(tmpl *template.Template, d FamilyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		token := chi.URLParam(r, "token")
		if sd == nil || d.DB == nil {
			http.NotFound(w, r)
			return
		}
		data := sessionData(sd, "family")
		withCSRF(r.Context(), data)
		data["Token"] = token
		inv, err := auth.FamilyInvitationByToken(r.Context(), d.DB, token, time.Now())
		if err != nil || d.Enabled == nil || !d.Enabled(inv.TenantID) {
			data["Gone"] = true
			renderPage(w, tmpl, data, http.StatusGone, d.Logger)
			return
		}
		var organizer string
		_ = d.DB.QueryRowContext(r.Context(), `SELECT email FROM tenants WHERE id = $1`, inv.TenantID).Scan(&organizer)
		data["From"] = organizer
		data["Invitation"] = inv
		// The account that joins is the person's OWN (RP92-2), whatever
		// tenant the session acts on; the page names it.
		if _, account, err := auth.OwnRootTenant(r.Context(), d.DB, sd.UserID); err == nil {
			data["Account"] = account
		}
		if r.Method == http.MethodPost {
			if _, err := auth.AcceptFamilyInvitation(r.Context(), d.DB, token, sd.UserID, sd.Email, time.Now()); err != nil {
				data["Error"] = familyFlash(err)
				renderPage(w, tmpl, data, http.StatusConflict, d.Logger)
				return
			}
			middleware.SetFlash(w, "success", "You joined "+organizer+"'s family: your storage counts in their plan; your files stay yours.")
			http.Redirect(w, r, familyPath, http.StatusSeeOther)
			return
		}
		renderPage(w, tmpl, data, http.StatusOK, d.Logger)
	}
}

// familyFlash is a refusal's sentence.
func familyFlash(err error) string {
	switch {
	case errors.Is(err, auth.ErrFamilyFull):
		return "A family is six people, you included (pending invitations count)."
	case errors.Is(err, auth.ErrMemberPaysOwnPlan):
		return "This account pays for its own plan: cancel it first, then join the family."
	case errors.Is(err, auth.ErrFamilyPoolFull):
		return "Your files do not fit the family's plan: ask the organizer for a bigger plan first, or join after deleting some."
	case errors.Is(err, auth.ErrNoOwnAccount):
		return "You have no account of your own to join with."
	case errors.Is(err, auth.ErrCannotJoinFamily):
		return "Only a personal account joins a family: no other members or pending member invitations, no apps or child accounts."
	case errors.Is(err, auth.ErrNotFamilyOrganizer):
		return "A family member does not organize a family of its own."
	case errors.Is(err, auth.ErrAlreadyMember):
		return "That address is already in the family."
	case errors.Is(err, auth.ErrInvitationEmailMismatch):
		return "The invitation was sent to another address: sign in as that address."
	case errors.Is(err, auth.ErrInvitationRateLimited):
		return "Too many invitations today. Try again tomorrow."
	case errors.Is(err, account.ErrNotAFamilyMember):
		return "No such member of this family."
	case errors.Is(err, auth.ErrInvitationExpired), errors.Is(err, auth.ErrInvitationUsed), errors.Is(err, auth.ErrInvitationNotFound):
		return "This link was used already, has expired, or is unknown."
	}
	if err != nil && strings.Contains(err.Error(), "invalid email") {
		return "Enter an e-mail address."
	}
	return "Something went wrong. Try again."
}
