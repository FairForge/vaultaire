package handlers

import (
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// KeyRow is a single API key for the template.
type KeyRow struct {
	ID          string
	Name        string
	KeyID       string
	Status      string
	StatusClass string
	CreatedFmt  string
	LastUsedFmt string
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

		key, err := authSvc.GenerateAPIKey(r.Context(), sd.UserID, name)
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
			ID:          k.ID,
			Name:        k.Name,
			KeyID:       k.Key,
			Status:      status,
			StatusClass: statusClass,
			CreatedFmt:  relativeTime(k.CreatedAt),
			LastUsedFmt: lastUsed,
		})
	}
	return rows
}
