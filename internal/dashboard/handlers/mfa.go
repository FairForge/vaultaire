package handlers

import (
	"encoding/base64"
	"html/template"
	"net/http"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// HandleMFASetup renders the 2FA setup page with a QR code and backup codes.
// The user must confirm with a TOTP code before MFA is actually enabled.
func HandleMFASetup(tmpl *template.Template, authSvc *auth.AuthService, mfaSvc *auth.MFAService, logger *zap.Logger) http.HandlerFunc {
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

		if mfaSvc == nil {
			data["Error"] = "2FA is not available."
			renderMFATemplate(w, tmpl, data, logger)
			return
		}

		// Generate a new TOTP secret.
		secret, otpauthURL, err := mfaSvc.GenerateSecret(sd.Email)
		if err != nil {
			logger.Error("generate totp secret", zap.Error(err))
			data["Error"] = "Could not generate 2FA secret."
			renderMFATemplate(w, tmpl, data, logger)
			return
		}

		// Generate backup codes.
		backupCodes, err := mfaSvc.GenerateBackupCodes()
		if err != nil {
			logger.Error("generate backup codes", zap.Error(err))
			data["Error"] = "Could not generate backup codes."
			renderMFATemplate(w, tmpl, data, logger)
			return
		}

		data["Secret"] = secret
		data["OTPAuthURL"] = otpauthURL
		data["QRDataURL"] = qrDataURL(otpauthURL)
		data["BackupCodes"] = backupCodes

		renderMFATemplate(w, tmpl, data, logger)
	}
}

// HandleMFAEnable handles POST /dashboard/settings/mfa/enable.
// Validates the TOTP code the user entered to confirm setup.
func HandleMFAEnable(settingsTmpl *template.Template, authSvc *auth.AuthService, mfaSvc *auth.MFAService, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		secret := r.FormValue("secret")
		code := r.FormValue("totp_code")
		backupCodesRaw := r.FormValue("backup_codes")

		if secret == "" || code == "" {
			http.Redirect(w, r, "/dashboard/settings/mfa", http.StatusSeeOther)
			return
		}

		// Validate the TOTP code against the secret.
		if mfaSvc == nil || !mfaSvc.ValidateCode(secret, code) {
			http.Redirect(w, r, "/dashboard/settings/mfa", http.StatusSeeOther)
			return
		}

		// Parse backup codes.
		var backupCodes []string
		for _, c := range splitCodes(backupCodesRaw) {
			if c != "" {
				backupCodes = append(backupCodes, c)
			}
		}

		// Enable MFA.
		if err := authSvc.EnableMFA(r.Context(), sd.UserID, secret, backupCodes); err != nil {
			logger.Error("enable mfa", zap.Error(err))
			http.Redirect(w, r, "/dashboard/settings/mfa", http.StatusSeeOther)
			return
		}

		http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
	}
}

// HandleMFADisable handles POST /dashboard/settings/mfa/disable.
// Requires the user's current password for confirmation.
func HandleMFADisable(settingsTmpl *template.Template, authSvc *auth.AuthService, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		password := r.FormValue("password")

		data := sessionData(sd, "settings")
		withCSRF(r.Context(), data)

		// Verify password before disabling MFA.
		if password != "" && authSvc != nil {
			valid, err := authSvc.ValidatePassword(r.Context(), sd.Email, password)
			if err != nil || !valid {
				data["MFAError"] = "Incorrect password."
				data["MFAEnabled"] = true
				populateProfileForMFA(authSvc, r, sd, data)
				renderMFATemplate(w, settingsTmpl, data, logger)
				return
			}
		}

		if err := authSvc.DisableMFA(r.Context(), sd.UserID); err != nil {
			logger.Error("disable mfa", zap.Error(err))
			data["MFAError"] = "Could not disable 2FA."
			data["MFAEnabled"] = true
			populateProfileForMFA(authSvc, r, sd, data)
			renderMFATemplate(w, settingsTmpl, data, logger)
			return
		}

		http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
	}
}

// HandleAdminResetMFA handles POST /admin/tenants/{id}/reset-mfa.
func HandleAdminResetMFA(authSvc *auth.AuthService, logger *zap.Logger) http.HandlerFunc {
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
			http.Error(w, "Failed to reset 2FA", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, "/admin/tenants/"+tenantID, http.StatusSeeOther)
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

func splitCodes(s string) []string {
	var codes []string
	current := ""
	for _, c := range s {
		if c == ',' {
			codes = append(codes, current)
			current = ""
		} else {
			current += string(c)
		}
	}
	if current != "" {
		codes = append(codes, current)
	}
	return codes
}

// qrDataURL encodes the otpauth URL as a base64 data URI for QR rendering.
func qrDataURL(otpauthURL string) string {
	return "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte(otpauthURL))
}
