package handlers

import (
	"database/sql"
	"html/template"
	"net/http"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

// HandleSettings renders the settings page with current profile, preferences,
// and the list of active sessions (devices signed in as this user).
func HandleSettings(tmpl *template.Template, authSvc *auth.AuthService, db *sql.DB, sessions dashauth.SessionStore, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "settings")
		withCSRF(r.Context(), data)
		withFlash(r.Context(), data)
		populateProfile(authSvc, db, r, sd, data)
		populateEmailVerified(r.Context(), db, sd.UserID, data)

		// MFA status for the settings page.
		if authSvc != nil {
			mfaEnabled, _ := authSvc.IsMFAEnabled(r.Context(), sd.UserID)
			data["MFAEnabled"] = mfaEnabled
		}

		// Active sessions list.
		data["SessionRows"] = loadSessionRows(r, sessions, sd.UserID, logger)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render settings", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// loadSessionRows fetches the current user's active sessions for display.
// Returns an empty slice (not nil) if the store is nil or the query fails
// so the template can always range over it.
func loadSessionRows(r *http.Request, sessions dashauth.SessionStore, userID string, logger *zap.Logger) []SessionRow {
	if sessions == nil {
		return []SessionRow{}
	}
	list, err := sessions.ListByUserID(r.Context(), userID)
	if err != nil {
		logger.Error("list sessions for settings", zap.Error(err))
		return []SessionRow{}
	}
	currentToken := ""
	if c, cErr := r.Cookie(dashauth.SessionCookieName); cErr == nil {
		currentToken = c.Value
	}
	return buildSessionRows(list, currentToken)
}

// HandleUpdateProfile handles POST /dashboard/settings/profile.
func HandleUpdateProfile(tmpl *template.Template, authSvc *auth.AuthService, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		company := r.FormValue("company")

		// Update company in DB directly (the authoritative source).
		if db != nil && company != "" {
			_, err := db.ExecContext(r.Context(),
				`UPDATE users SET company = $1, updated_at = NOW() WHERE id = $2`,
				company, sd.UserID)
			if err != nil {
				logger.Error("update company", zap.Error(err))
			}
		}

		// Also update in-memory profile if auth service supports it.
		if authSvc != nil {
			_ = authSvc.UpdateUserProfile(r.Context(), sd.UserID, auth.ProfileUpdate{
				Company: company,
			})
		}

		data := sessionData(sd, "settings")
		withCSRF(r.Context(), data)
		data["ProfileSuccess"] = "Profile updated."
		populateProfile(authSvc, db, r, sd, data)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render settings after profile update", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// HandleChangePassword handles POST /dashboard/settings/password.
// On success it invalidates every OTHER session the user has so stolen
// devices are signed out, while keeping the issuing session alive so the
// user doesn't immediately get bounced to /login on the device that just
// changed the password.
func HandleChangePassword(tmpl *template.Template, authSvc *auth.AuthService, db *sql.DB, sessions dashauth.SessionStore, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		current := r.FormValue("current_password")
		newPass := r.FormValue("new_password")
		confirm := r.FormValue("confirm_password")

		data := sessionData(sd, "settings")
		withCSRF(r.Context(), data)
		populateProfile(authSvc, db, r, sd, data)

		renderWithMsg := func(errMsg, successMsg string) {
			if errMsg != "" {
				data["PasswordError"] = errMsg
			}
			if successMsg != "" {
				data["PasswordSuccess"] = successMsg
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if errMsg != "" {
				w.WriteHeader(http.StatusBadRequest)
			}
			if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
				logger.Error("render settings after password change", zap.Error(err))
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}

		if len(newPass) < 8 {
			renderWithMsg("New password must be at least 8 characters.", "")
			return
		}
		if newPass != confirm {
			renderWithMsg("Passwords do not match.", "")
			return
		}
		if current == newPass {
			renderWithMsg("New password must be different from current password.", "")
			return
		}

		if authSvc == nil {
			renderWithMsg("Password change is not available.", "")
			return
		}

		if err := authSvc.ChangePassword(r.Context(), sd.UserID, current, newPass); err != nil {
			logger.Warn("password change failed", zap.String("user", sd.UserID), zap.Error(err))
			renderWithMsg("Current password is incorrect.", "")
			return
		}

		// Revoke every other session the user has. The current session
		// token is in the vaultaire_session cookie; preserve it.
		if sessions != nil {
			currentToken := ""
			if c, err := r.Cookie(dashauth.SessionCookieName); err == nil {
				currentToken = c.Value
			}
			if err := sessions.DeleteByUserIDExcept(r.Context(), sd.UserID, currentToken); err != nil {
				logger.Error("invalidate other sessions on password change",
					zap.String("user", sd.UserID), zap.Error(err))
			}
		}

		renderWithMsg("", "Password changed. You have been signed out of other devices.")
	}
}

// HandleUpdateNotifications handles POST /dashboard/settings/notifications.
func HandleUpdateNotifications(tmpl *template.Template, authSvc *auth.AuthService, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		emailNotif := r.FormValue("email_notifications") == "on"

		if authSvc != nil {
			prefs, _ := authSvc.GetUserPreferences(r.Context(), sd.UserID)
			if prefs == nil {
				prefs = &auth.UserPreferences{}
			}
			prefs.EmailNotifications = emailNotif
			_ = authSvc.SetUserPreferences(r.Context(), sd.UserID, *prefs)
		}

		data := sessionData(sd, "settings")
		withCSRF(r.Context(), data)
		data["NotifSuccess"] = "Notification preferences updated."
		populateProfile(authSvc, db, r, sd, data)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render settings after notif update", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func populateProfile(authSvc *auth.AuthService, db *sql.DB, r *http.Request, sd *dashauth.SessionData, data map[string]any) {
	data["ProfileEmail"] = sd.Email
	data["ProfileCompany"] = ""
	data["EmailNotifications"] = true
	data["MemberSince"] = ""

	// Load company from DB (authoritative).
	if db != nil {
		var company sql.NullString
		var createdAt sql.NullTime
		err := db.QueryRowContext(r.Context(),
			`SELECT company, created_at FROM users WHERE id = $1`, sd.UserID).
			Scan(&company, &createdAt)
		if err == nil {
			if company.Valid {
				data["ProfileCompany"] = company.String
			}
			if createdAt.Valid {
				data["MemberSince"] = createdAt.Time.Format("January 2006")
			}
		}
	}

	// Load notification preferences from auth service.
	if authSvc != nil {
		prefs, err := authSvc.GetUserPreferences(r.Context(), sd.UserID)
		if err == nil && prefs != nil {
			data["EmailNotifications"] = prefs.EmailNotifications
		}
	}
}
