package handlers

import (
	"context"
	"database/sql"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"go.uber.org/zap"
)

type supportResult struct {
	ID          string
	Name        string
	Email       string
	Plan        string
	Status      string
	StatusClass string
}

type timelineEvent struct {
	Type    string
	Summary string
	RelTime string
}

type errorLogEntry struct {
	Operation  string
	Bucket     string
	Key        string
	StatusCode int
	ErrorCode  string
	SourceIP   string
	RelTime    string
}

type adminNote struct {
	Note       string
	AdminEmail string
	RelTime    string
}

// HandleAdminSupport renders the customer support search page.
func HandleAdminSupport(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "admin-support")
		withCSRF(r.Context(), data)
		data["Results"] = []supportResult{}

		q := strings.TrimSpace(r.URL.Query().Get("q"))
		data["Query"] = q
		data["Searched"] = q != ""

		if q != "" && db != nil {
			data["Results"] = searchCustomers(r.Context(), db, q, logger)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin support", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func searchCustomers(ctx context.Context, db *sql.DB, q string, logger *zap.Logger) []supportResult {
	rows, err := db.QueryContext(ctx, `
		SELECT t.id, t.name, t.email, COALESCE(t.plan, 'starter'),
		       t.subscription_status, t.suspended_at
		FROM tenants t
		WHERE t.email ILIKE $1 OR t.id = $2 OR t.access_key = $2
		      OR t.stripe_customer_id = $2
		ORDER BY t.name LIMIT 50
	`, "%"+q+"%", q)
	if err != nil {
		logger.Error("support search", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()

	var results []supportResult
	for rows.Next() {
		var id, name, email, plan, subStatus string
		var suspendedAt sql.NullTime
		if err := rows.Scan(&id, &name, &email, &plan, &subStatus, &suspendedAt); err != nil {
			logger.Error("support search scan", zap.Error(err))
			continue
		}
		status, statusClass := tenantStatus(subStatus, suspendedAt.Valid)
		results = append(results, supportResult{
			ID: id, Name: name, Email: email, Plan: plan,
			Status: status, StatusClass: statusClass,
		})
	}
	return results
}

// HandleCustomerDetail renders the customer detail support page.
func HandleCustomerDetail(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		tenantID := chi.URLParam(r, "id")
		if tenantID == "" {
			http.NotFound(w, r)
			return
		}

		if db == nil {
			http.NotFound(w, r)
			return
		}

		data := sessionData(sd, "admin-support")
		withCSRF(r.Context(), data)
		withFlash(r.Context(), data)

		if !loadTenantDetail(r.Context(), db, tenantID, data, logger) {
			http.NotFound(w, r)
			return
		}

		var stripeCustomerID sql.NullString
		_ = db.QueryRowContext(r.Context(),
			`SELECT stripe_customer_id FROM tenants WHERE id = $1`, tenantID).Scan(&stripeCustomerID)
		if stripeCustomerID.Valid {
			data["StripeCustomerID"] = stripeCustomerID.String
		}

		data["Timeline"] = queryTimeline(r.Context(), db, tenantID, logger)
		data["Errors"] = queryS3Errors(r.Context(), db, tenantID, logger)
		data["Notes"] = queryNotes(r.Context(), db, tenantID, logger)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render customer detail", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func queryTimeline(ctx context.Context, db *sql.DB, tenantID string, logger *zap.Logger) []timelineEvent {
	rows, err := db.QueryContext(ctx, `
		(SELECT type, data::TEXT, created_at FROM events WHERE tenant_id = $1)
		UNION ALL
		(SELECT event_type, COALESCE(data::TEXT, ''), processed_at FROM stripe_events WHERE tenant_id = $1)
		ORDER BY created_at DESC LIMIT 50`, tenantID)
	if err != nil {
		logger.Debug("support timeline query", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()

	var events []timelineEvent
	for rows.Next() {
		var typ, dataStr string
		var createdAt time.Time
		if err := rows.Scan(&typ, &dataStr, &createdAt); err != nil {
			logger.Debug("support timeline scan", zap.Error(err))
			continue
		}
		summary := dataStr
		if len(summary) > 80 {
			summary = summary[:80] + "…"
		}
		events = append(events, timelineEvent{
			Type:    typ,
			Summary: summary,
			RelTime: relativeTime(createdAt),
		})
	}
	return events
}

func queryS3Errors(ctx context.Context, db *sql.DB, tenantID string, logger *zap.Logger) []errorLogEntry {
	rows, err := db.QueryContext(ctx, `
		SELECT operation, bucket, object_key, status_code, error_code, source_ip, logged_at
		FROM s3_access_log
		WHERE tenant_id = $1 AND status_code >= 400
		ORDER BY logged_at DESC LIMIT 20`, tenantID)
	if err != nil {
		logger.Debug("support s3 errors query", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()

	var entries []errorLogEntry
	for rows.Next() {
		var op, bucket, key, errCode, ip string
		var statusCode int
		var loggedAt time.Time
		if err := rows.Scan(&op, &bucket, &key, &statusCode, &errCode, &ip, &loggedAt); err != nil {
			logger.Debug("support s3 errors scan", zap.Error(err))
			continue
		}
		entries = append(entries, errorLogEntry{
			Operation: op, Bucket: bucket, Key: key,
			StatusCode: statusCode, ErrorCode: errCode, SourceIP: ip,
			RelTime: relativeTime(loggedAt),
		})
	}
	return entries
}

func queryNotes(ctx context.Context, db *sql.DB, tenantID string, logger *zap.Logger) []adminNote {
	rows, err := db.QueryContext(ctx, `
		SELECT n.note, u.email, n.created_at
		FROM admin_notes n
		JOIN users u ON u.id = n.admin_user_id
		WHERE n.tenant_id = $1
		ORDER BY n.created_at DESC`, tenantID)
	if err != nil {
		logger.Debug("support notes query", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()

	var notes []adminNote
	for rows.Next() {
		var note, adminEmail string
		var createdAt time.Time
		if err := rows.Scan(&note, &adminEmail, &createdAt); err != nil {
			logger.Debug("support notes scan", zap.Error(err))
			continue
		}
		notes = append(notes, adminNote{
			Note: note, AdminEmail: adminEmail, RelTime: relativeTime(createdAt),
		})
	}
	return notes
}

// HandleAddNote inserts an admin note and redirects back to the customer detail page.
func HandleAddNote(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		tenantID := chi.URLParam(r, "id")
		if tenantID == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		// Redirect target is path-escaped so a crafted id can never break out
		// of the /admin/support/ prefix.
		detailPath := "/admin/support/" + url.PathEscape(tenantID)

		note := strings.TrimSpace(r.FormValue("note"))
		if note == "" {
			middleware.SetFlash(w, "error", "Note cannot be empty.")
			http.Redirect(w, r, detailPath, http.StatusSeeOther)
			return
		}
		if len(note) > 2000 {
			middleware.SetFlash(w, "error", "Note is too long (max 2000 characters).")
			http.Redirect(w, r, detailPath, http.StatusSeeOther)
			return
		}

		if db == nil {
			middleware.SetFlash(w, "error", "Database not available.")
			http.Redirect(w, r, detailPath, http.StatusSeeOther)
			return
		}

		_, err := db.ExecContext(r.Context(),
			`INSERT INTO admin_notes (tenant_id, admin_user_id, note) VALUES ($1, $2, $3)`,
			tenantID, sd.UserID, note)
		if err != nil {
			logger.Error("add admin note", zap.Error(err))
			middleware.SetFlash(w, "error", "Failed to save note.")
			http.Redirect(w, r, detailPath, http.StatusSeeOther)
			return
		}

		middleware.SetFlash(w, "success", "Note added.")
		http.Redirect(w, r, detailPath, http.StatusSeeOther)
	}
}
