package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"go.uber.org/zap"
)

type notificationRow struct {
	ID         int64
	Type       string
	Message    string
	TenantID   string
	IsRead     bool
	BadgeClass string
	RelTime    string
}

func HandleAdminNotifications(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "admin-notifications")
		withCSRF(r.Context(), data)
		withFlash(r.Context(), data)
		data["Notifications"] = []notificationRow{}
		data["UnreadCount"] = 0

		if db != nil {
			notifs, unread := queryAdminNotifications(r.Context(), db, logger)
			data["Notifications"] = notifs
			data["UnreadCount"] = unread
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin notifications", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func queryAdminNotifications(ctx context.Context, db *sql.DB, logger *zap.Logger) ([]notificationRow, int) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, type, message, tenant_id, read_at, created_at
		 FROM admin_notifications ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		logger.Error("admin notifications query", zap.Error(err))
		return nil, 0
	}
	defer func() { _ = rows.Close() }()

	var notifs []notificationRow
	var unread int
	for rows.Next() {
		var id int64
		var typ, message string
		var tenantID sql.NullString
		var readAt sql.NullTime
		var createdAt time.Time
		if err := rows.Scan(&id, &typ, &message, &tenantID, &readAt, &createdAt); err != nil {
			logger.Error("admin notifications scan", zap.Error(err))
			continue
		}
		isRead := readAt.Valid
		if !isRead {
			unread++
		}
		notifs = append(notifs, notificationRow{
			ID:         id,
			Type:       typ,
			Message:    message,
			TenantID:   tenantID.String,
			IsRead:     isRead,
			BadgeClass: notifBadgeClass(typ),
			RelTime:    relativeTime(createdAt),
		})
	}
	return notifs, unread
}

func HandleMarkRead(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if dashauth.GetSession(r.Context()) == nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		idStr := chi.URLParam(r, "id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		if db == nil {
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}

		_, err = db.ExecContext(r.Context(),
			`UPDATE admin_notifications SET read_at = NOW() WHERE id = $1 AND read_at IS NULL`, id)
		if err != nil {
			logger.Error("mark notification read", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		var typ, message string
		var tenantID sql.NullString
		var createdAt time.Time
		err = db.QueryRowContext(r.Context(),
			`SELECT type, message, tenant_id, created_at FROM admin_notifications WHERE id = $1`, id).
			Scan(&typ, &message, &tenantID, &createdAt)
		if err != nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		renderNotifRow(w, id, typ, message, tenantID.String, createdAt, true)
	}
}

func HandleMarkAllRead(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if dashauth.GetSession(r.Context()) == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		if db != nil {
			_, err := db.ExecContext(r.Context(),
				`UPDATE admin_notifications SET read_at = NOW() WHERE read_at IS NULL`)
			if err != nil {
				logger.Error("mark all notifications read", zap.Error(err))
				middleware.SetFlash(w, "error", "Failed to mark notifications as read.")
				http.Redirect(w, r, "/admin/notifications", http.StatusSeeOther)
				return
			}
		}

		middleware.SetFlash(w, "success", "All notifications marked as read.")
		http.Redirect(w, r, "/admin/notifications", http.StatusSeeOther)
	}
}

func HandleNotifCount(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if dashauth.GetSession(r.Context()) == nil {
			w.WriteHeader(http.StatusOK)
			return
		}

		var count int
		if db != nil {
			if err := db.QueryRowContext(r.Context(),
				`SELECT COUNT(*) FROM admin_notifications WHERE read_at IS NULL`).Scan(&count); err != nil {
				logger.Debug("notification count", zap.Error(err))
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if count > 0 {
			_, _ = fmt.Fprintf(w, `<span class="notif-badge">%d</span>`, count)
		}
	}
}

// CreateNotification inserts an admin notification. Safe to call with nil db.
func CreateNotification(ctx context.Context, db *sql.DB, notifType, message, tenantID string) error {
	if db == nil {
		return nil
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO admin_notifications (type, message, tenant_id) VALUES ($1, $2, NULLIF($3, ''))`,
		notifType, message, tenantID)
	return err
}

func notifBadgeClass(typ string) string {
	switch typ {
	case "signup":
		return "success"
	case "payment":
		return "danger"
	case "quota":
		return "warning"
	case "subscription":
		return "info"
	default:
		return "default"
	}
}

func renderNotifRow(w http.ResponseWriter, id int64, typ, message, tenantID string, createdAt time.Time, isRead bool) {
	readClass := ""
	if isRead {
		readClass = " notif-row--read"
	}
	_, _ = fmt.Fprintf(w, `<div class="notif-row%s" id="notif-%d">`, readClass, id)
	_, _ = fmt.Fprint(w, `<div style="display:flex;justify-content:space-between;align-items:start">`)
	_, _ = fmt.Fprint(w, `<div>`)
	_, _ = fmt.Fprintf(w, `<span class="badge badge-%s">%s</span>`, notifBadgeClass(typ), html.EscapeString(typ))
	_, _ = fmt.Fprintf(w, `<span style="margin-left:0.5rem">%s</span>`, html.EscapeString(message))
	if tenantID != "" {
		_, _ = fmt.Fprintf(w, ` <a href="/admin/support/%s" style="font-size:0.85rem;color:#60a5fa">%s</a>`,
			html.EscapeString(tenantID), html.EscapeString(tenantID))
	}
	_, _ = fmt.Fprint(w, `</div>`)
	_, _ = fmt.Fprint(w, `<div style="display:flex;align-items:center;gap:0.75rem;white-space:nowrap">`)
	_, _ = fmt.Fprintf(w, `<span style="font-size:0.85rem;color:#94a3b8">%s</span>`, relativeTime(createdAt))
	if !isRead {
		_, _ = fmt.Fprintf(w, `<button hx-post="/admin/notifications/%d/read" hx-target="#notif-%d" hx-swap="outerHTML" class="btn btn-sm">Mark read</button>`, id, id)
	}
	_, _ = fmt.Fprint(w, `</div></div></div>`)
}
