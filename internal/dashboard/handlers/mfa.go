package handlers

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"image/png"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/pquerna/otp"
	"go.uber.org/zap"
)

// mfaQRPath is where the setup page's <img> finds the QR code of the
// session's pending secret (HandleMFAQR).
const mfaQRPath = "/dashboard/settings/mfa/qr.png"

// sessionID is the dashboard session id of the request (the session cookie's
// value) — the key the pending enrolment is stored under.
func sessionID(r *http.Request) string {
	if c, err := r.Cookie(dashauth.SessionCookieName); err == nil {
		return c.Value
	}
	return ""
}

// mfaFreshSessionWindow: an OAuth-only account (no password to ask for)
// may enrol a second factor or regenerate its backup codes only from a
// session created this recently — a fresh sign-in through Google/GitHub.
const mfaFreshSessionWindow = 10 * time.Minute

// mfaNow is the clock the freshness check reads; tests move it.
var mfaNow = time.Now

// confirmMFAIdentity is the check in front of enrolling a second factor and
// regenerating backup codes (WP-R12-15): a stolen dashboard session must
// not be enough to enrol the thief's authenticator and lock the owner out,
// or to mint a fresh set of recovery codes. The password when the account
// has one (bcrypt, through ValidatePassword); for an OAuth-only account the
// current session must be younger than mfaFreshSessionWindow — the user has
// just proven themselves to the identity provider. Returns "" when the
// identity is confirmed, else the message to show. The same shape as
// confirmDeletionIdentity (account.go).
func confirmMFAIdentity(r *http.Request, authSvc *auth.AuthService, sessions dashauth.SessionStore, sd *dashauth.SessionData, what string) string {
	if authSvc == nil {
		return "Two-factor settings are not available."
	}
	u, err := authSvc.GetUserByID(r.Context(), sd.UserID)
	if err != nil {
		return "Two-factor settings are not available."
	}
	if u.PasswordHash != "" {
		password := r.FormValue("password")
		if password == "" {
			return "Enter your password to " + what + "."
		}
		valid, err := authSvc.ValidatePassword(r.Context(), sd.Email, password)
		if err != nil || !valid {
			return "Incorrect password."
		}
		return ""
	}
	// OAuth-only: a fresh sign-in stands in for the password.
	if sessions == nil {
		return "Sign in again with Google or GitHub, then " + what + " within 10 minutes."
	}
	infos, err := sessions.ListByUserID(r.Context(), sd.UserID)
	if err != nil {
		return "Sign in again with Google or GitHub, then " + what + " within 10 minutes."
	}
	current := sessionID(r)
	for _, si := range infos {
		if si.ID == current && mfaNow().Sub(si.CreatedAt) < mfaFreshSessionWindow {
			return ""
		}
	}
	return "This account signs in with Google or GitHub. Sign in again, then " + what + " within 10 minutes."
}

// hasPassword reports whether the account can be asked for a password
// (false for an OAuth-only account) — the templates choose the field or the
// fresh-sign-in note by it.
func hasPassword(ctx context.Context, authSvc *auth.AuthService, userID string) bool {
	if authSvc == nil {
		return false
	}
	u, err := authSvc.GetUserByID(ctx, userID)
	return err == nil && u.PasswordHash != ""
}

// HandleMFASetup renders the 2FA setup page: the QR code and the manual key
// of a TOTP secret that is generated and KEPT by the server (MFAEnrolmentStore,
// keyed by the session) until HandleMFAEnable sees a valid code for it. The
// page carries no backup codes and no secret-bearing form field (WP-R12-8).
// A reload or a second tab shows the same secret for ten minutes.
func HandleMFASetup(tmpl *template.Template, authSvc *auth.AuthService, mfaSvc *auth.MFAService, store *MFAEnrolmentStore, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// If already enabled, redirect to settings.
		if authSvc != nil {
			if enabled, _ := authSvc.IsMFAEnabled(r.Context(), sd.UserID); enabled {
				http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
				return
			}
		}

		data := sessionData(sd, "settings")
		withCSRF(r.Context(), data)
		withFlash(r.Context(), data)
		data["HasPassword"] = hasPassword(r.Context(), authSvc, sd.UserID)
		// The page shows a secret: no cache may keep it.
		w.Header().Set("Cache-Control", "no-store")

		sid := sessionID(r)
		if mfaSvc == nil || store == nil || sid == "" {
			data["Error"] = "2FA is not available."
			renderMFATemplate(w, tmpl, data, logger)
			return
		}

		e, err := store.Begin(sid, sd.UserID, func() (string, string, error) { return mfaSvc.GenerateSecret(sd.Email) })
		switch {
		case errors.Is(err, ErrMFAEnrolmentBusy):
			logger.Warn("mfa enrolment store is full")
			data["Error"] = "Too many two-factor setups are in progress right now. Try again in a few minutes."
		case err != nil:
			logger.Error("generate totp secret", zap.Error(err))
			data["Error"] = "Could not generate 2FA secret."
		default:
			data["Secret"] = e.Secret
			data["QRPath"] = mfaQRPath
		}
		renderMFATemplate(w, tmpl, data, logger)
	}
}

// HandleMFAQR serves the QR code of the session's pending enrolment as a PNG
// rendered by the server (WP-R12-8). The page used to load a QR library from
// cdn.jsdelivr.net, without SRI, and hand it the secret; no script touches
// the secret now and the page needs none to work.
func HandleMFAQR(store *MFAEnrolmentStore, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil || store == nil {
			http.NotFound(w, r)
			return
		}
		e, ok := store.Peek(sessionID(r), sd.UserID)
		if !ok {
			http.NotFound(w, r)
			return
		}
		key, err := otp.NewKeyFromURL(e.OTPAuthURL)
		if err != nil {
			logger.Error("mfa qr: parse otpauth url", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		img, err := key.Image(240, 240)
		if err != nil {
			logger.Error("mfa qr: render", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			logger.Error("mfa qr: encode", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		// Only the dashboard's own pages may embed it: a page on a sibling
		// subdomain (same-site, so the session cookie rides along) gets no
		// pixels at all.
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		_, _ = w.Write(buf.Bytes())
	}
}

// HandleMFAEnable handles POST /dashboard/settings/mfa/enable. The request
// carries the 6-digit code and the account's password (WP-R12-15 — or, for
// an OAuth-only account, rides a session younger than ten minutes) and
// nothing else that matters: the secret the code is checked against is the
// server's pending one, and the backup codes are generated here, after the
// code verified, stored hashed, and shown ONCE in this response. Nothing
// secret is accepted from the client — a `secret` or `backup_codes` field in
// the form is ignored. The password is checked BEFORE the code is consumed,
// so a refused attempt leaves the pending secret (and the QR code the user
// scanned) in place.
func HandleMFAEnable(tmpl *template.Template, authSvc *auth.AuthService, mfaSvc *auth.MFAService, store *MFAEnrolmentStore, sessions dashauth.SessionStore, logger *zap.Logger) http.HandlerFunc {
	const setupPath = "/dashboard/settings/mfa"
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if authSvc == nil || mfaSvc == nil || store == nil {
			http.Redirect(w, r, setupPath, http.StatusSeeOther)
			return
		}

		// Already enrolled: a replay of the enable POST, a reload of the
		// result page, a second tab. Nothing is replaced and no codes are
		// shown again.
		if enabled, _ := authSvc.IsMFAEnabled(r.Context(), sd.UserID); enabled {
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		if msg := confirmMFAIdentity(r, authSvc, sessions, sd, "enable two-factor authentication"); msg != "" {
			middleware.SetFlash(w, "error", msg)
			http.Redirect(w, r, setupPath, http.StatusSeeOther)
			return
		}

		code := strings.TrimSpace(r.FormValue("totp_code"))
		sid := sessionID(r)
		e, res := store.Confirm(sid, sd.UserID, func(secret string) bool {
			return code != "" && mfaSvc.ValidateCode(secret, code)
		})
		switch res {
		case MFAEnrolNone:
			middleware.SetFlash(w, "error", "That setup session has expired — it lasts 10 minutes, and a service restart ends it. Scan the new QR code below, and remove the earlier Stored entry from your authenticator.")
			http.Redirect(w, r, setupPath, http.StatusSeeOther)
			return
		case MFAEnrolMismatch:
			middleware.SetFlash(w, "error", "That code did not match. Check that your phone's clock is right and enter the current code.")
			http.Redirect(w, r, setupPath, http.StatusSeeOther)
			return
		}

		backupCodes, err := mfaSvc.GenerateBackupCodes()
		if err == nil {
			err = authSvc.EnableMFA(r.Context(), sd.UserID, e.Secret, backupCodes)
		}
		if err != nil {
			// The code was right and the QR code is still good: put the
			// pending secret back so the next attempt can succeed.
			store.Restore(sid, e)
			logger.Error("enable mfa", zap.Error(err))
			middleware.SetFlash(w, "error", "Two-factor authentication could not be enabled. Try the current code again.")
			http.Redirect(w, r, setupPath, http.StatusSeeOther)
			return
		}

		// The code that enrolled is spent: it must not also be the second
		// factor of a sign-in in the same 30 seconds (RFC 6238 §5.2 — the
		// single-use guard of the login flow now knows about it).
		authSvc.ConsumeTOTPCode(sd.UserID, code)

		data := sessionData(sd, "settings")
		withCSRF(r.Context(), data)
		data["Enrolled"] = true
		data["BackupCodes"] = backupCodes
		// Shown once: this response is the only place the codes ever appear.
		w.Header().Set("Cache-Control", "no-store")
		renderMFATemplate(w, tmpl, data, logger)
	}
}

// HandleMFARegenerateBackupCodes handles POST
// /dashboard/settings/mfa/backup-codes (WP-R12-15): a fresh set of ten
// backup codes, shown once in the response; every old code stops working.
// It requires the password (or a fresh OAuth sign-in) like enrolment does,
// and 2FA to be on. It is also the remedy for the crash window between
// EnableMFA and the response that showed the codes: a user whose codes
// page never rendered gets a set here instead of being locked to one
// authenticator.
func HandleMFARegenerateBackupCodes(tmpl *template.Template, authSvc *auth.AuthService, mfaSvc *auth.MFAService, sessions dashauth.SessionStore, logger *zap.Logger) http.HandlerFunc {
	const settingsPath = "/dashboard/settings"
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if authSvc == nil || mfaSvc == nil {
			middleware.SetFlash(w, "error", "Two-factor settings are not available.")
			http.Redirect(w, r, settingsPath, http.StatusSeeOther)
			return
		}
		if enabled, _ := authSvc.IsMFAEnabled(r.Context(), sd.UserID); !enabled {
			middleware.SetFlash(w, "error", "Two-factor authentication is not enabled on this account.")
			http.Redirect(w, r, settingsPath, http.StatusSeeOther)
			return
		}
		if msg := confirmMFAIdentity(r, authSvc, sessions, sd, "regenerate your backup codes"); msg != "" {
			middleware.SetFlash(w, "error", msg)
			http.Redirect(w, r, settingsPath, http.StatusSeeOther)
			return
		}

		codes, err := mfaSvc.GenerateBackupCodes()
		if err == nil {
			err = authSvc.RegenerateBackupCodes(r.Context(), sd.UserID, codes)
		}
		if err != nil {
			logger.Error("regenerate backup codes", zap.String("user", sd.UserID), zap.Error(err))
			middleware.SetFlash(w, "error", "Your backup codes could not be regenerated. Try again.")
			http.Redirect(w, r, settingsPath, http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "settings")
		withCSRF(r.Context(), data)
		data["Enrolled"] = true
		data["Regenerated"] = true
		data["BackupCodes"] = codes
		// Shown once: this response is the only place the codes ever appear.
		w.Header().Set("Cache-Control", "no-store")
		renderMFATemplate(w, tmpl, data, logger)
	}
}

// HandleMFADisable handles POST /dashboard/settings/mfa/disable.
// Requires the user's current password for confirmation. Every OTHER session
// is revoked (R5-22 / WP-R5-8): a device that got in while the second factor
// was on must not outlive the decision to drop it; the issuing device stays.
func HandleMFADisable(settingsTmpl *template.Template, authSvc *auth.AuthService, sessions dashauth.SessionStore, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		password := r.FormValue("password")

		data := sessionData(sd, "settings")
		withCSRF(r.Context(), data)

		// The password is REQUIRED to disable MFA. An empty field used to
		// skip the check entirely (review R5-04): a hijacked session could
		// strip the second factor without knowing the password.
		fail := func(msg string) {
			data["MFAError"] = msg
			data["MFAEnabled"] = true
			populateProfileForMFA(authSvc, r, sd, data)
			renderMFATemplate(w, settingsTmpl, data, logger)
		}
		if authSvc == nil {
			fail("Two-factor settings are not available.")
			return
		}
		if password == "" {
			fail("Enter your password to disable two-factor authentication.")
			return
		}
		if u, err := authSvc.GetUserByID(r.Context(), sd.UserID); err == nil && u.PasswordHash == "" {
			// OAuth-only account: nothing to verify against, and accepting
			// any input would be the bypass again. Admin reset is the path.
			fail("This account signs in with Google/GitHub and has no password; contact support to reset two-factor authentication.")
			return
		}
		valid, err := authSvc.ValidatePassword(r.Context(), sd.Email, password)
		if err != nil || !valid {
			fail("Incorrect password.")
			return
		}

		if err := authSvc.DisableMFA(r.Context(), sd.UserID); err != nil {
			logger.Error("disable mfa", zap.Error(err))
			data["MFAError"] = "Could not disable 2FA."
			data["MFAEnabled"] = true
			populateProfileForMFA(authSvc, r, sd, data)
			renderMFATemplate(w, settingsTmpl, data, logger)
			return
		}

		if sessions != nil {
			current := ""
			if c, cErr := r.Cookie(dashauth.SessionCookieName); cErr == nil {
				current = c.Value
			}
			if err := sessions.DeleteByUserIDExcept(r.Context(), sd.UserID, current); err != nil {
				logger.Error("revoke other sessions on mfa disable", zap.String("user", sd.UserID), zap.Error(err))
			}
		}

		middleware.SetFlash(w, "success", "Two-factor authentication disabled. Other devices were signed out.")
		http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
	}
}

// HandleAdminResetMFA handles POST /admin/tenants/{id}/reset-mfa (an htmx
// form: the response is the fragment for #mfa-feedback, not a redirect — a
// 303 made htmx swap the whole tenant page into the card). Resetting the
// second factor signs the user out everywhere (R5-22): whoever holds a
// session got it under the old factor. The audit row is written by
// auth.DisableMFA.
func HandleAdminResetMFA(authSvc *auth.AuthService, sessions dashauth.SessionStore, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "id")
		if tenantID == "" {
			http.Error(w, "Missing tenant ID", http.StatusBadRequest)
			return
		}

		userID := authSvc.GetUserIDByTenantID(r.Context(), tenantID)
		if userID == "" {
			http.Error(w, "Tenant not found", http.StatusNotFound)
			return
		}

		if err := authSvc.DisableMFA(r.Context(), userID); err != nil {
			logger.Error("admin reset mfa", zap.String("tenant", tenantID), zap.Error(err))
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`<div class="alert alert-error">Failed to reset 2FA.</div>`))
			return
		}
		if sessions != nil {
			if err := sessions.DeleteByUserID(r.Context(), userID); err != nil {
				logger.Error("revoke sessions on admin mfa reset", zap.String("user", userID), zap.Error(err))
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<div class="alert alert-success">Two-factor authentication reset. The user was signed out of every device and can enrol again from Settings.</div>`))
	}
}

func renderMFATemplate(w http.ResponseWriter, tmpl *template.Template, data map[string]any, logger *zap.Logger) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
		logger.Error("render template", zap.Error(err))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func populateProfileForMFA(authSvc *auth.AuthService, r *http.Request, sd *dashauth.SessionData, data map[string]any) {
	data["ProfileEmail"] = sd.Email
	data["ProfileCompany"] = ""
	data["EmailNotifications"] = true
	data["MemberSince"] = ""
	if authSvc != nil {
		prefs, err := authSvc.GetUserPreferences(r.Context(), sd.UserID)
		if err == nil && prefs != nil {
			data["EmailNotifications"] = prefs.EmailNotifications
		}
	}
}
