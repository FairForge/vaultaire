package handlers

import (
	"database/sql"
	"html/template"
	"net/http"
	"strings"

	"go.uber.org/zap"
)

var validReportTypes = map[string]bool{
	"copyright":  true,
	"csam":       true,
	"malware":    true,
	"spam":       true,
	"harassment": true,
	"other":      true,
}

// HandleAbuseForm renders the public abuse report form.
func HandleAbuseForm(tmpl *template.Template, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", map[string]any{"Page": "abuse"}); err != nil {
			logger.Error("render abuse form", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// HandleAbuseSubmit processes a public abuse report submission.
func HandleAbuseSubmit(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		email := strings.TrimSpace(r.FormValue("reporter_email"))
		name := strings.TrimSpace(r.FormValue("reporter_name"))
		reportType := strings.TrimSpace(r.FormValue("report_type"))
		description := strings.TrimSpace(r.FormValue("description"))
		url := strings.TrimSpace(r.FormValue("url"))

		renderErr := func(msg string) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_ = tmpl.ExecuteTemplate(w, "base", map[string]any{
				"Page":          "abuse",
				"Error":         msg,
				"ReporterEmail": email,
				"ReporterName":  name,
				"ReportType":    reportType,
				"Description":   description,
				"URL":           url,
			})
		}

		if email == "" || !strings.Contains(email, "@") {
			renderErr("A valid email address is required.")
			return
		}
		if !validReportTypes[reportType] {
			renderErr("Please select a report type.")
			return
		}
		if description == "" {
			renderErr("A description of the abuse is required.")
			return
		}
		if len(description) > 5000 {
			renderErr("Description is too long (max 5000 characters).")
			return
		}

		if db == nil {
			renderErr("Service temporarily unavailable.")
			return
		}

		_, err := db.ExecContext(r.Context(),
			`INSERT INTO abuse_reports (reporter_email, reporter_name, report_type, description, url)
			 VALUES ($1, $2, $3, $4, $5)`,
			email, name, reportType, description, url)
		if err != nil {
			logger.Error("insert abuse report", zap.Error(err))
			renderErr("Failed to submit report. Please try again.")
			return
		}

		if notifErr := CreateNotification(r.Context(), db, "system", "New abuse report: "+reportType, ""); notifErr != nil {
			logger.Error("create abuse notification", zap.Error(notifErr))
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tmpl.ExecuteTemplate(w, "base", map[string]any{
			"Page":      "abuse",
			"Submitted": true,
		})
	}
}
