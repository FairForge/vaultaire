package handlers

import (
	"database/sql"
	"html/template"
	"net/http"
	"net/mail"
	"strings"

	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
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

// Abuse-form limits (pre-launch checklist item 9: both prod rows were crypto
// spam from throwaway addresses). The form is public and unauthenticated, so
// the defences are cheap and silent: a honeypot field bots fill in, bounds on
// every field, a URL that must be http(s), and a per-reporter daily cap on
// top of the 5/min per-IP limiter in router.go.
const (
	abuseMaxEmail       = 320
	abuseMaxName        = 200
	abuseMaxURL         = 2048
	abuseMaxDescription = 5000
	abusePerReporterDay = 3
	// abuseHoneypotField is a hidden input humans never see (CSS-hidden,
	// autocomplete off). Anything in it means a bot filled every field.
	abuseHoneypotField = "website"
)

// HandleAbuseSubmit processes a public abuse report submission.
func HandleAbuseSubmit(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		email := strings.TrimSpace(r.FormValue("reporter_email"))
		name := strings.TrimSpace(r.FormValue("reporter_name"))
		reportType := strings.TrimSpace(r.FormValue("report_type"))
		description := strings.TrimSpace(r.FormValue("description"))
		url := strings.TrimSpace(r.FormValue("url"))

		renderErr := func(status int, msg string) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(status)
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
		thanks := func() {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = tmpl.ExecuteTemplate(w, "base", map[string]any{"Page": "abuse", "Submitted": true})
		}

		// Honeypot: pretend success, store nothing, leave a log line.
		if strings.TrimSpace(r.FormValue(abuseHoneypotField)) != "" {
			logger.Info("abuse report dropped: honeypot filled", zap.String("ip", middleware.ClientIP(r)))
			thanks()
			return
		}

		addr, aErr := mail.ParseAddress(email)
		if aErr != nil || len(email) > abuseMaxEmail || addr.Address != email {
			renderErr(http.StatusBadRequest, "A valid email address is required.")
			return
		}
		email = strings.ToLower(email)
		if len(name) > abuseMaxName {
			renderErr(http.StatusBadRequest, "Name is too long.")
			return
		}
		if !validReportTypes[reportType] {
			renderErr(http.StatusBadRequest, "Please select a report type.")
			return
		}
		if description == "" {
			renderErr(http.StatusBadRequest, "A description of the abuse is required.")
			return
		}
		if len(description) > abuseMaxDescription {
			renderErr(http.StatusBadRequest, "Description is too long (max 5000 characters).")
			return
		}
		if url != "" {
			isHTTP := strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://")
			if len(url) > abuseMaxURL || !isHTTP {
				renderErr(http.StatusBadRequest, "The content URL must start with http:// or https://.")
				return
			}
		}

		if db == nil {
			renderErr(http.StatusServiceUnavailable, "Service temporarily unavailable.")
			return
		}

		// Per-reporter daily cap: one address cannot flood the queue even
		// from many IPs.
		var today int
		if err := db.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM abuse_reports WHERE reporter_email = $1 AND created_at > NOW() - INTERVAL '24 hours'`,
			email).Scan(&today); err == nil && today >= abusePerReporterDay {
			renderErr(http.StatusTooManyRequests, "You have reached today's limit of reports from this address. Email abuse@stored.ge if this is urgent.")
			return
		}

		_, err := db.ExecContext(r.Context(),
			`INSERT INTO abuse_reports (reporter_email, reporter_name, report_type, description, url)
			 VALUES ($1, $2, $3, $4, $5)`,
			email, name, reportType, description, url)
		if err != nil {
			logger.Error("insert abuse report", zap.Error(err))
			renderErr(http.StatusInternalServerError, "Failed to submit report. Please try again.")
			return
		}

		if notifErr := CreateNotification(r.Context(), db, "system", "New abuse report: "+reportType, ""); notifErr != nil {
			logger.Error("create abuse notification", zap.Error(notifErr))
		}

		thanks()
	}
}
