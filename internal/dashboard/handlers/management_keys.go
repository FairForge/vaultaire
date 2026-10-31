package handlers

// The dashboard's management keys (WP-KEY-1 PR C; D-STRIPE-29), on the API
// keys page: a server's bearer key of the account (sk_live_… / sk_test_…),
// minted with its scopes by an admin or the owner (the route's action is
// management_key.write), the secret shown once, listed with its hint only,
// revoked at once. A platform is offered the platform scope.

import (
	"database/sql"
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
)

// ManagementKeyRow is one management key for the template.
type ManagementKeyRow struct {
	ID          string
	Name        string
	Hint        string // prefix + the first four characters of the secret
	Scopes      string
	CreatedFmt  string
	LastUsedFmt string
	Status      string
	StatusClass string
}

// NewManagementKeyData is the just-minted key (shown once).
type NewManagementKeyData struct {
	Secret string
	Scopes string
}

// withManagementKeys puts the tenant's management keys and whether it is a
// platform on the page's data.
func withManagementKeys(r *http.Request, db *sql.DB, tenantID string, data map[string]any) {
	if db == nil || tenantID == "" {
		return
	}
	if ok, err := auth.IsPlatform(r.Context(), db, tenantID); err == nil {
		data["IsPlatform"] = ok
	}
	keys, err := auth.ListManagementKeys(r.Context(), db, tenantID)
	if err != nil {
		return
	}
	rows := make([]ManagementKeyRow, 0, len(keys))
	for _, k := range keys {
		row := ManagementKeyRow{ID: k.ID, Name: k.Name, Hint: k.Prefix + k.Hint + "…", Scopes: strings.Join(k.Scopes, ", "),
			CreatedFmt: k.CreatedAt.Format("Jan 2, 2006"), LastUsedFmt: "never", Status: "active", StatusClass: "success"}
		if k.LastUsedAt != nil {
			row.LastUsedFmt = relativeTime(*k.LastUsedAt)
		}
		if k.RevokedAt != nil {
			row.Status, row.StatusClass = "revoked", "danger"
		}
		rows = append(rows, row)
	}
	data["ManagementKeys"] = rows
}

// HandleGenerateManagementKey handles POST /dashboard/management-keys.
func HandleGenerateManagementKey(tmpl *template.Template, authSvc *auth.AuthService, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		data := sessionData(sd, "apikeys")
		withCSRF(r.Context(), data)
		render := func() {
			data["Keys"] = listKeys(r, authSvc, sd.TenantID)
			withManagementKeys(r, db, sd.TenantID, data)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
				logger.Error("render apikeys", zap.Error(err))
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}
		if err := r.ParseForm(); err != nil || db == nil {
			data["GenerateError"] = "The form could not be read."
			render()
			return
		}
		k, secret, err := auth.MintManagementKey(r.Context(), db, sd.TenantID, auth.ManagementKeyOptions{
			Name: strings.TrimSpace(r.PostFormValue("name")), Scopes: r.PostForm["scope"], CreatedBy: sd.UserID})
		switch {
		case errors.Is(err, auth.ErrManagementKeyScope):
			data["GenerateError"] = "Choose at least one scope for the management key."
			render()
			return
		case errors.Is(err, auth.ErrKeyLimitReached):
			data["GenerateError"] = "An account holds at most 25 live management keys. Revoke one first."
			render()
			return
		case err != nil:
			logger.Error("mint a management key", zap.Error(err), zap.String("tenant_id", sd.TenantID))
			data["GenerateError"] = "The key could not be made. Try again."
			render()
			return
		}
		audit.Record(r.Context(), db, audit.Entry{TenantID: sd.TenantID, Actor: sd.UserID, Action: "management_key.created",
			Resource: "management_key:" + k.ID, Metadata: map[string]any{"scopes": k.Scopes, "prefix": k.Prefix, "name": k.Name, "via": "dashboard"}})
		data["NewManagementKey"] = &NewManagementKeyData{Secret: secret, Scopes: strings.Join(k.Scopes, ", ")}
		render()
	}
}

// HandleRevokeManagementKey handles POST /dashboard/management-keys/{id}/revoke.
func HandleRevokeManagementKey(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		id := chi.URLParam(r, "id")
		switch err := auth.RevokeManagementKey(r.Context(), db, sd.TenantID, id); {
		case errors.Is(err, auth.ErrManagementKeyNotFound):
			middleware.SetFlash(w, "error", "That management key is not live.")
		case err != nil:
			logger.Warn("revoke a management key", zap.Error(err), zap.String("id", id))
			middleware.SetFlash(w, "error", "That management key could not be revoked.")
		default:
			audit.Record(r.Context(), db, audit.Entry{TenantID: sd.TenantID, Actor: sd.UserID, Action: "management_key.revoked",
				Resource: "management_key:" + id, Metadata: map[string]any{"via": "dashboard"}})
			middleware.SetFlash(w, "success", "Management key revoked: it is refused from its next request.")
		}
		http.Redirect(w, r, "/dashboard/apikeys", http.StatusSeeOther)
	}
}
