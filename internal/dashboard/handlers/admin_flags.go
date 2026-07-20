package handlers

import (
	"html/template"
	"net/http"
	"strconv"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// Admin feature-flags page (1.13 live-iteration kit): a table of every
// registered flag (default, global state, per-tenant overrides) with toggle
// buttons and a tenant-override form. Mutations go through the flag
// service's write-through Set/Unset, so a toggle is live on the next
// request — no deploy, no restart.

// HandleAdminFlags renders the flags dashboard page.
func HandleAdminFlags(tmpl *template.Template, svc *flags.Service, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "admin-flags")
		withCSRF(r.Context(), data)
		data["Flags"] = svc.Resolved()

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin flags", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// HandleAdminFlagSet handles POST /admin/flags/{key}/set — form fields
// `enabled` (bool) and optional `tenant_id` (empty = global row).
func HandleAdminFlagSet(svc *flags.Service, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		key := chi.URLParam(r, "key")
		enabled, err := strconv.ParseBool(r.FormValue("enabled"))
		if err != nil {
			http.Error(w, "enabled must be true or false", http.StatusBadRequest)
			return
		}

		if err := svc.Set(r.Context(), key, r.FormValue("tenant_id"), enabled, sd.Email); err != nil {
			logger.Error("dashboard flag set failed",
				zap.String("flag", key), zap.Error(err))
			http.Error(w, "failed to set flag", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/admin/flags", http.StatusSeeOther)
	}
}

// HandleAdminFlagClear handles POST /admin/flags/{key}/clear — removes the
// row for `tenant_id` (empty = the global row), reverting to global/default.
func HandleAdminFlagClear(svc *flags.Service, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		key := chi.URLParam(r, "key")
		if err := svc.Unset(r.Context(), key, r.FormValue("tenant_id")); err != nil {
			logger.Error("dashboard flag clear failed",
				zap.String("flag", key), zap.Error(err))
			http.Error(w, "failed to clear flag", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/admin/flags", http.StatusSeeOther)
	}
}
