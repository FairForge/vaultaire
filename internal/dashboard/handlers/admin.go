package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/payments"
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
		withCSRF(r.Context(), data)
		data["TenantCount"] = 0
		data["TotalStorageFmt"] = "0 B"
		data["ActiveSubCount"] = 0
		data["MonthlyRevenue"] = "$0.00"

		if db != nil {
			kind := tenantKindOf(r)
			data["TenantKind"] = string(kind)
			data["TenantCount"], data["TotalStorageFmt"],
				data["ActiveSubCount"], data["MonthlyRevenue"] = queryAdminStats(r.Context(), db, kind, logger)
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

func queryAdminStats(ctx context.Context, db *sql.DB, kind tenantKind, logger *zap.Logger) (int, string, int, string) {
	var tenantCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenants t WHERE `+tenantKindSQL(kind, "t")).Scan(&tenantCount); err != nil {
		logger.Debug("admin: count tenants", zap.Error(err))
	}

	var totalBytes int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(q.storage_used_bytes), 0) FROM tenant_quotas q
		JOIN tenants t ON t.id = q.tenant_id WHERE `+tenantKindSQL(kind, "t")).Scan(&totalBytes); err != nil {
		logger.Debug("admin: sum storage", zap.Error(err))
	}

	// Active = granted entitlements (WP-PAY-7: the record, not a status).
	activeSubCount, err := payments.CountLiveHouses(ctx, db)
	if err != nil {
		logger.Debug("admin: count live houses", zap.Error(err))
	}

	// Revenue = the live houses at payments.QuoteHouse (WP-PAY-0: one
	// product; the legacy pack prices are gone).
	revenueCents := housesMRR(queryHouses(ctx, db, logger))

	return tenantCount, formatBytes(totalBytes), activeSubCount,
		fmt.Sprintf("$%.2f", float64(revenueCents)/100)
}

func queryRecentUsers(ctx context.Context, db *sql.DB, logger *zap.Logger) []recentUser {
	rows, err := db.QueryContext(ctx, `
		SELECT u.email, COALESCE(u.company, ''),
		       CASE WHEN e.tenant_id IS NOT NULL AND e.lapsed_at IS NULL THEN 'house'
		            WHEN t.plan = 'house' THEN 'starter'
		            ELSE COALESCE(t.plan, 'free') END,
		       u.created_at
		FROM users u
		LEFT JOIN tenants t ON t.email = u.email
		LEFT JOIN entitlements e ON e.tenant_id = t.id
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
	if err := rows.Err(); err != nil {
		return nil
	}
	return users
}
