package handlers

import (
	"database/sql"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"go.uber.org/zap"
)

type abuseReportRow struct {
	ID            int64
	ReporterEmail string
	ReportType    string
	URL           string
	Status        string
	TenantID      string
	BadgeClass    string
	RelTime       string
}

type abuseReportDetail struct {
	ID            int64
	ReporterEmail string
	ReporterName  string
	ReportType    string
	Description   string
	URL           string
	Status        string
	TenantID      string
	Bucket        string
	ObjectKey     string
	ResolvedBy    string
	ResolvedAt    string
	BadgeClass    string
	RelTime       string
}

var validAbuseActions = map[string]bool{
	"reviewing": true,
	"actioned":  true,
	"dismissed": true,
}

func abuseBadgeClass(status string) string {
	switch status {
	case "open":
		return "warning"
	case "reviewing":
		return "info"
	case "actioned":
		return "success"
	case "dismissed":
		return "default"
	default:
		return "default"
	}
}

// HandleAdminAbuse lists abuse reports filtered by status.
func HandleAdminAbuse(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "admin-abuse")
		withCSRF(r.Context(), data)
		withFlash(r.Context(), data)

		status := r.URL.Query().Get("status")
		if status == "" {
			status = "open"
		}
		data["FilterStatus"] = status
		data["Reports"] = []abuseReportRow{}

		if db != nil {
			var rows *sql.Rows
			var err error
			if status == "all" {
				rows, err = db.QueryContext(r.Context(),
					`SELECT id, reporter_email, report_type, url, status, tenant_id, created_at
					 FROM abuse_reports ORDER BY created_at DESC LIMIT 100`)
			} else {
				rows, err = db.QueryContext(r.Context(),
					`SELECT id, reporter_email, report_type, url, status, tenant_id, created_at
					 FROM abuse_reports WHERE status = $1 ORDER BY created_at DESC LIMIT 100`, status)
			}
			if err != nil {
				logger.Error("query abuse reports", zap.Error(err))
			} else {
				defer func() { _ = rows.Close() }()
				var reports []abuseReportRow
				for rows.Next() {
					var id int64
					var reporterEmail, reportType, reportURL, reportStatus string
					var tenantID sql.NullString
					var createdAt time.Time
					if err := rows.Scan(&id, &reporterEmail, &reportType, &reportURL,
						&reportStatus, &tenantID, &createdAt); err != nil {
						logger.Error("scan abuse report", zap.Error(err))
						continue
					}
					reports = append(reports, abuseReportRow{
						ID:            id,
						ReporterEmail: reporterEmail,
						ReportType:    reportType,
						URL:           reportURL,
						Status:        reportStatus,
						TenantID:      tenantID.String,
						BadgeClass:    abuseBadgeClass(reportStatus),
						RelTime:       relativeTime(createdAt),
					})
				}
				data["Reports"] = reports
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin abuse", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// HandleAdminAbuseDetail renders a single abuse report with action buttons.
func HandleAdminAbuseDetail(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		idStr := chi.URLParam(r, "id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		data := sessionData(sd, "admin-abuse")
		withCSRF(r.Context(), data)
		withFlash(r.Context(), data)

		if db == nil {
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}

		var report abuseReportDetail
		var tenantID, bucket, objectKey, resolvedBy sql.NullString
		var resolvedAt sql.NullTime
		var createdAt time.Time

		err = db.QueryRowContext(r.Context(),
			`SELECT id, reporter_email, reporter_name, report_type, description, url,
			        status, tenant_id, bucket, object_key, resolved_by, resolved_at, created_at
			 FROM abuse_reports WHERE id = $1`, id).
			Scan(&report.ID, &report.ReporterEmail, &report.ReporterName,
				&report.ReportType, &report.Description, &report.URL,
				&report.Status, &tenantID, &bucket, &objectKey,
				&resolvedBy, &resolvedAt, &createdAt)
		if err != nil {
			if err == sql.ErrNoRows {
				http.Error(w, "Not Found", http.StatusNotFound)
			} else {
				logger.Error("query abuse report detail", zap.Error(err))
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
			return
		}

		report.TenantID = tenantID.String
		report.Bucket = bucket.String
		report.ObjectKey = objectKey.String
		report.ResolvedBy = resolvedBy.String
		if resolvedAt.Valid {
			report.ResolvedAt = resolvedAt.Time.Format("Jan 2, 2006 15:04 UTC")
		}
		report.BadgeClass = abuseBadgeClass(report.Status)
		report.RelTime = relativeTime(createdAt)

		data["Report"] = report

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render abuse detail", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// HandleAbuseAction processes status changes on an abuse report.
func HandleAbuseAction(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		idStr := chi.URLParam(r, "id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		action := r.FormValue("action")
		if !validAbuseActions[action] {
			http.Error(w, "Bad Request: invalid action", http.StatusBadRequest)
			return
		}

		if db == nil {
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}

		_, err = db.ExecContext(r.Context(),
			`UPDATE abuse_reports SET status = $1, resolved_by = $2, resolved_at = NOW()
			 WHERE id = $3`, action, sd.Email, id)
		if err != nil {
			logger.Error("update abuse report", zap.Error(err))
			middleware.SetFlash(w, "error", "Failed to update report.")
			http.Redirect(w, r, "/admin/abuse/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
			return
		}

		middleware.SetFlash(w, "success", "Report status updated to "+action+".")
		http.Redirect(w, r, "/admin/abuse/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
	}
}
