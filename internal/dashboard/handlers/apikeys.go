package handlers

import (
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// KeyRow is a single API key for the template.
type KeyRow struct {
	ID           string
	Name         string
	KeyID        string
	ScopeSummary string
	Status       string
	StatusClass  string
	CreatedFmt   string
	LastUsedFmt  string
}

// NewKeyData holds the just-generated key credentials (shown once).
type NewKeyData struct {
	Key    string
	Secret string
}

// HandleAPIKeys returns an http.HandlerFunc that lists API keys for the
// current user.
func HandleAPIKeys(tmpl *template.Template, authSvc *auth.AuthService, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "apikeys")
		withCSRF(r.Context(), data)
		withFlash(r.Context(), data)
		data["Keys"] = listKeys(r, authSvc, sd.UserID)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render apikeys", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// HandleGenerateKey handles POST /dashboard/apikeys to create a new key.
func HandleGenerateKey(tmpl *template.Template, authSvc *auth.AuthService, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "apikeys")
		withCSRF(r.Context(), data)

		name := strings.TrimSpace(r.FormValue("name"))
		if name == "" {
			name = "default"
		}

		var opts *auth.KeyCreateOptions
		perms := r.Form["permissions"]
		bucketScope := parseCSV(r.FormValue("bucket_scope"))
		ipAllowlist := parseCSV(r.FormValue("ip_allowlist"))
		expiresStr := r.FormValue("expires_at")

		hasScope := len(perms) > 0 || len(bucketScope) > 0 || len(ipAllowlist) > 0 || expiresStr != ""
		if hasScope {
			opts = &auth.KeyCreateOptions{
				Permissions: perms,
				BucketScope: bucketScope,
				IPAllowlist: ipAllowlist,
			}
			if expiresStr != "" {
				if t, parseErr := time.Parse("2006-01-02", expiresStr); parseErr == nil {
					eod := t.Add(24*time.Hour - time.Second)
					opts.ExpiresAt = &eod
				}
			}
			if len(opts.Permissions) > 0 {
				if err := auth.ValidatePermissions(opts.Permissions); err != nil {
					data["GenerateError"] = "Invalid permission: " + err.Error()
					data["Keys"] = listKeys(r, authSvc, sd.UserID)
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					_ = tmpl.ExecuteTemplate(w, "base", data)
					return
				}
			}
		}

		key, err := authSvc.GenerateAPIKey(r.Context(), sd.UserID, name, opts)
		if err != nil {
			logger.Error("generate API key", zap.Error(err))
			data["GenerateError"] = "Failed to generate key. Please try again."
			data["Keys"] = listKeys(r, authSvc, sd.UserID)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = tmpl.ExecuteTemplate(w, "base", data)
			return
		}

		// Show the secret once — it won't be available again.
		data["NewKey"] = NewKeyData{
			Key:    key.Key,
			Secret: key.Secret,
		}
		data["Keys"] = listKeys(r, authSvc, sd.UserID)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render apikeys after generate", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// HandleRevokeKey handles POST /dashboard/apikeys/{id}/revoke.
func HandleRevokeKey(authSvc *auth.AuthService, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		keyID := chi.URLParam(r, "id")
		if keyID == "" {
			http.Redirect(w, r, "/dashboard/apikeys", http.StatusSeeOther)
			return
		}

		if err := authSvc.RevokeAPIKey(r.Context(), sd.UserID, keyID); err != nil {
			logger.Warn("revoke API key", zap.Error(err), zap.String("key_id", keyID))
		} else {
			middleware.SetFlash(w, "success", "API key revoked.")
		}

		http.Redirect(w, r, "/dashboard/apikeys", http.StatusSeeOther)
	}
}

func listKeys(r *http.Request, authSvc *auth.AuthService, userID string) []KeyRow {
	keys, err := authSvc.ListAPIKeys(r.Context(), userID)
	if err != nil || keys == nil {
		return nil
	}

	now := time.Now()
	var rows []KeyRow
	for _, k := range keys {
		status := "active"
		statusClass := "success"
		if k.RevokedAt != nil {
			status = "revoked"
			statusClass = "danger"
		} else if k.ExpiresAt != nil && k.ExpiresAt.Before(now) {
			status = "expired"
			statusClass = "default"
		}

		lastUsed := "never"
		if k.LastUsed != nil {
			lastUsed = relativeTime(*k.LastUsed)
		}

		rows = append(rows, KeyRow{
			ID:           k.ID,
			Name:         k.Name,
			KeyID:        k.Key,
			ScopeSummary: scopeSummary(k),
			Status:       status,
			StatusClass:  statusClass,
			CreatedFmt:   relativeTime(k.CreatedAt),
			LastUsedFmt:  lastUsed,
		})
	}
	return rows
}

func scopeSummary(k *auth.APIKey) string {
	if len(k.Permissions) == 1 && k.Permissions[0] == "*" && len(k.BucketScope) == 0 && len(k.IPAllowlist) == 0 {
		return "Full access"
	}
	var parts []string
	if len(k.Permissions) > 0 && (len(k.Permissions) != 1 || k.Permissions[0] != "*") {
		parts = append(parts, strings.Join(k.Permissions, ", "))
	}
	if len(k.BucketScope) > 0 {
		parts = append(parts, "buckets: "+strings.Join(k.BucketScope, ", "))
	}
	if len(k.IPAllowlist) > 0 {
		parts = append(parts, "IPs: "+strings.Join(k.IPAllowlist, ", "))
	}
	if len(parts) == 0 {
		return "Full access"
	}
	return strings.Join(parts, " | ")
}

func parseCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var result []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}
