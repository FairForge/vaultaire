package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

// HandleAdminOverview renders the admin dashboard overview page.
func HandleAdminOverview(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "admin-overview")
		data["TenantCount"] = 0
		data["TotalStorageFmt"] = "0 B"
		data["ActiveSubCount"] = 0
		data["MonthlyRevenue"] = "$0.00"

		if db != nil {
			data["TenantCount"], data["TotalStorageFmt"],
				data["ActiveSubCount"], data["MonthlyRevenue"] = queryAdminStats(r.Context(), db, logger)
			data["RecentUsers"] = queryRecentUsers(r.Context(), db, logger)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin overview", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

type recentUser struct {
	Email         string
	Company       string
	Plan          string
	RegisteredFmt string
}

func queryAdminStats(ctx context.Context, db *sql.DB, logger *zap.Logger) (int, string, int, string) {
	var tenantCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenants`).Scan(&tenantCount); err != nil {
		logger.Debug("admin: count tenants", zap.Error(err))
	}

	var totalBytes int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(used_bytes), 0) FROM tenant_quotas`).Scan(&totalBytes); err != nil {
		logger.Debug("admin: sum storage", zap.Error(err))
	}

	var activeSubCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tenants WHERE subscription_status = 'active'`).Scan(&activeSubCount); err != nil {
		logger.Debug("admin: count active subs", zap.Error(err))
	}

	// Estimate revenue from active Vault subscriptions.
	var revenueCents int64
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE
			WHEN plan = 'vault3' THEN 299
			WHEN plan = 'vault9' THEN 900
			WHEN plan = 'vault18' THEN 1800
			WHEN plan = 'vault36' THEN 3600
			ELSE 0
		END), 0) FROM tenants WHERE subscription_status = 'active'
	`).Scan(&revenueCents); err != nil {
		logger.Debug("admin: estimate revenue", zap.Error(err))
	}

	return tenantCount, formatBytes(totalBytes), activeSubCount,
		fmt.Sprintf("$%.2f", float64(revenueCents)/100)
}

func queryRecentUsers(ctx context.Context, db *sql.DB, logger *zap.Logger) []recentUser {
	rows, err := db.QueryContext(ctx, `
		SELECT u.email, COALESCE(u.company, ''), COALESCE(t.plan, 'free'), u.created_at
		FROM users u
		LEFT JOIN tenants t ON t.id = u.tenant_id
		ORDER BY u.created_at DESC
		LIMIT 10
	`)
	if err != nil {
		logger.Debug("admin: recent users", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()

	var users []recentUser
	for rows.Next() {
		var u recentUser
		var created sql.NullTime
		if err := rows.Scan(&u.Email, &u.Company, &u.Plan, &created); err != nil {
			continue
		}
		if created.Valid {
			u.RegisteredFmt = relativeTime(created.Time)
		}
		users = append(users, u)
	}
	return users
}
