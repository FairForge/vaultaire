package handlers

import (
	"context"
	"database/sql"
	"encoding/csv"
	"html/template"
	"net/http"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

// waitlistRow is one pre-launch signup for the admin table.
type waitlistRow struct {
	Email      string
	Source     string
	CreatedFmt string
}

// HandleAdminWaitlist renders the admin waitlist page: total count + recent signups.
func HandleAdminWaitlist(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "admin-waitlist")
		withCSRF(r.Context(), data)
		data["SignupCount"] = 0
		data["Signups"] = []waitlistRow{}

		if db != nil {
			rows, count := queryWaitlist(r.Context(), db, logger)
			data["Signups"] = rows
			data["SignupCount"] = count
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin waitlist", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// HandleAdminWaitlistExport streams all signups as a CSV download.
func HandleAdminWaitlistExport(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if dashauth.GetSession(r.Context()) == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition",
			`attachment; filename="waitlist-`+time.Now().Format("2006-01-02")+`.csv"`)

		cw := csv.NewWriter(w)
		defer cw.Flush()
		_ = cw.Write([]string{"email", "source", "created_at"})

		if db == nil {
			return
		}
		rows, err := db.QueryContext(r.Context(),
			`SELECT email, source, created_at FROM waitlist_signups ORDER BY created_at DESC`)
		if err != nil {
			logger.Error("waitlist export query", zap.Error(err))
			return
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var email, source string
			var created time.Time
			if err := rows.Scan(&email, &source, &created); err != nil {
				logger.Error("waitlist export scan", zap.Error(err))
				continue
			}
			_ = cw.Write([]string{email, source, created.UTC().Format(time.RFC3339)})
		}
	}
}

func queryWaitlist(ctx context.Context, db *sql.DB, logger *zap.Logger) ([]waitlistRow, int) {
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM waitlist_signups`).Scan(&count); err != nil {
		logger.Debug("waitlist count", zap.Error(err))
	}

	rows, err := db.QueryContext(ctx,
		`SELECT email, source, created_at FROM waitlist_signups ORDER BY created_at DESC LIMIT 1000`)
	if err != nil {
		logger.Error("waitlist list query", zap.Error(err))
		return nil, count
	}
	defer func() { _ = rows.Close() }()

	var out []waitlistRow
	for rows.Next() {
		var email, source string
		var created time.Time
		if err := rows.Scan(&email, &source, &created); err != nil {
			logger.Error("waitlist list scan", zap.Error(err))
			continue
		}
		out = append(out, waitlistRow{
			Email:      email,
			Source:     source,
			CreatedFmt: created.Format("Jan 2, 2006 15:04 MST"),
		})
	}
	return out, count
}
