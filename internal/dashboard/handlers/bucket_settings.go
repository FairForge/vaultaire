package handlers

import (
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

const cdnBaseHost = "https://cdn.stored.ge/"

func HandleBucketSettings(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		bucketName := chi.URLParam(r, "name")
		if bucketName == "" {
			http.Redirect(w, r, "/dashboard/buckets", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "buckets")
		withCSRF(r.Context(), data)
		withFlash(r.Context(), data)
		data["BucketName"] = bucketName

		vis := "private"
		corsOrigins := "*"
		cacheMaxAge := 3600
		slug := ""

		if db != nil {
			_ = db.QueryRowContext(r.Context(),
				`SELECT visibility, cors_origins, cache_max_age_secs
				 FROM buckets WHERE tenant_id = $1 AND name = $2`,
				sd.TenantID, bucketName).Scan(&vis, &corsOrigins, &cacheMaxAge)

			_ = db.QueryRowContext(r.Context(),
				`SELECT COALESCE(slug, '') FROM tenants WHERE id = $1`,
				sd.TenantID).Scan(&slug)

			var tier string
			_ = db.QueryRowContext(r.Context(),
				`SELECT COALESCE(tier, '') FROM tenant_quotas WHERE tenant_id = $1`,
				sd.TenantID).Scan(&tier)

			allowed, reason := auth.CanEnablePublicRead(tier)
			if !allowed {
				data["ArchiveRestricted"] = true
				data["ArchiveReason"] = reason
			}
		}

		data["Visibility"] = vis
		data["CORSOrigins"] = corsOrigins
		data["CacheMaxAge"] = cacheMaxAge
		data["Slug"] = slug

		if vis == "public-read" && slug != "" {
			data["CDNBaseURL"] = cdnBaseHost + slug + "/" + bucketName
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render bucket settings", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func HandleUpdateBucketSettings(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		bucketName := chi.URLParam(r, "name")
		if bucketName == "" {
			http.Redirect(w, r, "/dashboard/buckets", http.StatusSeeOther)
			return
		}

		visibility := r.FormValue("visibility")
		if visibility != "private" && visibility != "public-read" {
			data := sessionData(sd, "buckets")
			withCSRF(r.Context(), data)
			data["BucketName"] = bucketName
			data["Visibility"] = "private"
			data["CORSOrigins"] = "*"
			data["CacheMaxAge"] = 3600
			data["FlashError"] = "Invalid visibility setting."
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = tmpl.ExecuteTemplate(w, "base", data)
			return
		}

		corsOrigins := strings.TrimSpace(r.FormValue("cors_origins"))
		if corsOrigins == "" {
			corsOrigins = "*"
		}

		cacheMaxAge := 3600
		if v := r.FormValue("cache_max_age"); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil {
				cacheMaxAge = parsed
			}
		}
		if cacheMaxAge < 0 {
			cacheMaxAge = 0
		}
		if cacheMaxAge > 86400 {
			cacheMaxAge = 86400
		}

		if db == nil {
			middleware.SetFlash(w, "error", "Database not available.")
			http.Redirect(w, r, fmt.Sprintf("/dashboard/buckets/%s/settings", bucketName), http.StatusSeeOther)
			return
		}

		if visibility == "public-read" {
			var tier string
			_ = db.QueryRowContext(r.Context(),
				`SELECT COALESCE(tier, '') FROM tenant_quotas WHERE tenant_id = $1`,
				sd.TenantID).Scan(&tier)

			allowed, reason := auth.CanEnablePublicRead(tier)
			if !allowed {
				middleware.SetFlash(w, "error", reason)
				http.Redirect(w, r, fmt.Sprintf("/dashboard/buckets/%s/settings", bucketName), http.StatusSeeOther)
				return
			}
		}

		result, err := db.ExecContext(r.Context(),
			`UPDATE buckets
			 SET visibility = $1, cors_origins = $2, cache_max_age_secs = $3, updated_at = NOW()
			 WHERE tenant_id = $4 AND name = $5`,
			visibility, corsOrigins, cacheMaxAge, sd.TenantID, bucketName)
		if err != nil {
			logger.Error("update bucket settings", zap.Error(err))
			middleware.SetFlash(w, "error", "Failed to save settings.")
			http.Redirect(w, r, fmt.Sprintf("/dashboard/buckets/%s/settings", bucketName), http.StatusSeeOther)
			return
		}

		rows, _ := result.RowsAffected()
		if rows == 0 {
			middleware.SetFlash(w, "error", "Bucket not found.")
			http.Redirect(w, r, "/dashboard/buckets", http.StatusSeeOther)
			return
		}

		middleware.SetFlash(w, "success", "Bucket settings saved.")
		http.Redirect(w, r, fmt.Sprintf("/dashboard/buckets/%s/settings", bucketName), http.StatusSeeOther)
	}
}
