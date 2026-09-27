package handlers

import (
	"context"
	"database/sql"
	"encoding/csv"
	"html/template"
	"net/http"
	"strconv"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

// waitlistRow is one pre-launch signup for the admin table.
type waitlistRow struct {
	Email      string
	Source     string
	CreatedFmt string
	StdTB      int // the house built on the landing page: TB downstairs (Standard)
	VaultTB    int // ...and in the attic (Vault); both 0 when no house was built
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
			// launch-day demand per floor, from the houses people built
			var std, vault, houses int
			for _, r := range rows {
				std += r.StdTB
				vault += r.VaultTB
				if r.StdTB+r.VaultTB > 0 {
					houses++
				}
			}
			data["TotalStdTB"], data["TotalVaultTB"], data["Houses"] = std, vault, houses
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin waitlist", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// waitlistFilters are the launch-list cuts of the waitlist (dashboard plan
// Phase 4): everyone, everyone who built a house, and per floor.
var waitlistFilters = map[string]string{"": "all", "house": "houses", "downstairs": "downstairs", "attic": "attic"}

// HandleAdminWaitlistExport streams signups as a CSV download.
//
//	?filter=house|downstairs|attic  only signups whose house has that floor
//	?fields=email                   one column, ready to paste into a mailer
func HandleAdminWaitlistExport(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if dashauth.GetSession(r.Context()) == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		filter := r.URL.Query().Get("filter")
		label, ok := waitlistFilters[filter]
		if !ok {
			http.Error(w, "unknown filter", http.StatusBadRequest)
			return
		}
		emailOnly := r.URL.Query().Get("fields") == "email"

		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition",
			`attachment; filename="waitlist-`+label+`-`+time.Now().Format("2006-01-02")+`.csv"`)

		cw := csv.NewWriter(w)
		defer cw.Flush()
		if emailOnly {
			_ = cw.Write([]string{"email"})
		} else {
			_ = cw.Write([]string{"email", "source", "created_at", "downstairs_tb", "attic_tb"})
		}

		if db == nil {
			return
		}
		rows, err := db.QueryContext(r.Context(),
			`SELECT email, source, created_at, plan_std_tb, plan_vault_tb FROM waitlist_signups
			 WHERE ($1 = '' OR ($1 = 'house' AND plan_std_tb + plan_vault_tb > 0)
			        OR ($1 = 'downstairs' AND plan_std_tb > 0) OR ($1 = 'attic' AND plan_vault_tb > 0))
			 ORDER BY created_at DESC`, filter)
		if err != nil {
			logger.Error("waitlist export query", zap.Error(err))
			return
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var email, source string
			var stdTB, vaultTB int
			var created time.Time
			if err := rows.Scan(&email, &source, &created, &stdTB, &vaultTB); err != nil {
				logger.Error("waitlist export scan", zap.Error(err))
				continue
			}
			if emailOnly {
				_ = cw.Write([]string{email})
				continue
			}
			_ = cw.Write([]string{email, source, created.UTC().Format(time.RFC3339), strconv.Itoa(stdTB), strconv.Itoa(vaultTB)})
		}
	}
}

func queryWaitlist(ctx context.Context, db *sql.DB, logger *zap.Logger) ([]waitlistRow, int) {
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM waitlist_signups`).Scan(&count); err != nil {
		logger.Debug("waitlist count", zap.Error(err))
	}

	rows, err := db.QueryContext(ctx,
		`SELECT email, source, created_at, plan_std_tb, plan_vault_tb FROM waitlist_signups ORDER BY created_at DESC LIMIT 1000`)
	if err != nil {
		logger.Error("waitlist list query", zap.Error(err))
		return nil, count
	}
	defer func() { _ = rows.Close() }()

	var out []waitlistRow
	for rows.Next() {
		var email, source string
		var stdTB, vaultTB int
		var created time.Time
		if err := rows.Scan(&email, &source, &created, &stdTB, &vaultTB); err != nil {
			logger.Error("waitlist list scan", zap.Error(err))
			continue
		}
		out = append(out, waitlistRow{
			Email:      email,
			Source:     source,
			CreatedFmt: created.Format("Jan 2, 2006 15:04 MST"),
			StdTB:      stdTB,
			VaultTB:    vaultTB,
		})
	}
	return out, count
}
