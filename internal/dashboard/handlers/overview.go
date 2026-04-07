package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

// ActivityRow is a single recent-activity row for the dashboard template.
type ActivityRow struct {
	Operation  string
	ObjectKey  string
	SizeFmt    string
	TimeFmt    string
	BadgeClass string
}

// HandleOverview returns an http.HandlerFunc that renders the dashboard
// overview page with real data from PostgreSQL.
func HandleOverview(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := map[string]any{
			"Email":    sd.Email,
			"Role":     sd.Role,
			"UserID":   sd.UserID,
			"TenantID": sd.TenantID,
			"Page":     "dashboard",
		}

		ctx := r.Context()

		if db != nil {
			populateStorageUsage(ctx, db, sd.TenantID, data)
			populateBandwidth(ctx, db, sd.TenantID, data)
			populateCounts(ctx, db, sd.TenantID, sd.UserID, data)
			populateActivity(ctx, db, sd.TenantID, data)
		} else {
			setDefaults(data)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render dashboard overview", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func populateStorageUsage(ctx context.Context, db *sql.DB, tenantID string, data map[string]any) {
	var used, limit int64
	var tier string

	err := db.QueryRowContext(ctx,
		`SELECT storage_used_bytes, storage_limit_bytes, tier
		 FROM tenant_quotas WHERE tenant_id = $1`, tenantID).Scan(&used, &limit, &tier)
	if err != nil {
		data["StorageUsedFmt"] = "0 B"
		data["StorageLimitFmt"] = "1 TB"
		data["StoragePercent"] = 0
		data["StorageBarClass"] = ""
		data["Tier"] = "starter"
		return
	}

	pct := 0
	if limit > 0 {
		pct = int(used * 100 / limit)
	}

	barClass := ""
	if pct >= 90 {
		barClass = "danger"
	} else if pct >= 75 {
		barClass = "warning"
	}

	data["StorageUsedFmt"] = formatBytes(used)
	data["StorageLimitFmt"] = formatBytes(limit)
	data["StoragePercent"] = pct
	data["StorageBarClass"] = barClass
	data["Tier"] = tier
}

func populateBandwidth(ctx context.Context, db *sql.DB, tenantID string, data map[string]any) {
	var ingress, egress int64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(ingress_bytes), 0), COALESCE(SUM(egress_bytes), 0)
		 FROM bandwidth_usage_daily
		 WHERE tenant_id = $1 AND date >= date_trunc('month', CURRENT_DATE)`,
		tenantID).Scan(&ingress, &egress)
	if err != nil {
		ingress, egress = 0, 0
	}

	data["IngressFmt"] = formatBytes(ingress)
	data["EgressFmt"] = formatBytes(egress)
	data["BandwidthTotalFmt"] = formatBytes(ingress + egress)
}

func populateCounts(ctx context.Context, db *sql.DB, tenantID, userID string, data map[string]any) {
	var bucketCount, objectCount, apiKeyCount int

	// Bucket count: distinct buckets in the head cache for this tenant.
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT bucket) FROM object_head_cache WHERE tenant_id = $1`,
		tenantID).Scan(&bucketCount)

	// Object count.
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`,
		tenantID).Scan(&objectCount)

	// API key count.
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM api_keys WHERE user_id = $1`,
		userID).Scan(&apiKeyCount)

	data["BucketCount"] = bucketCount
	data["ObjectCount"] = objectCount
	data["APIKeyCount"] = apiKeyCount
}

func populateActivity(ctx context.Context, db *sql.DB, tenantID string, data map[string]any) {
	rows, err := db.QueryContext(ctx,
		`SELECT operation, object_key, bytes_delta, timestamp
		 FROM quota_usage_events
		 WHERE tenant_id = $1
		 ORDER BY timestamp DESC
		 LIMIT 5`, tenantID)
	if err != nil {
		data["Activity"] = nil
		return
	}
	defer func() { _ = rows.Close() }()

	var activity []ActivityRow
	for rows.Next() {
		var op, key string
		var delta int64
		var ts time.Time
		if err := rows.Scan(&op, &key, &delta, &ts); err != nil {
			continue
		}

		badge := "default"
		switch op {
		case "PUT", "RESERVE":
			badge = "success"
		case "DELETE":
			badge = "danger"
		}

		// Truncate long keys for display.
		displayKey := key
		if len(displayKey) > 60 {
			displayKey = displayKey[:57] + "..."
		}

		activity = append(activity, ActivityRow{
			Operation:  op,
			ObjectKey:  displayKey,
			SizeFmt:    formatBytes(absInt64(delta)),
			TimeFmt:    relativeTime(ts),
			BadgeClass: badge,
		})
	}
	data["Activity"] = activity
}

func setDefaults(data map[string]any) {
	data["StorageUsedFmt"] = "0 B"
	data["StorageLimitFmt"] = "1 TB"
	data["StoragePercent"] = 0
	data["StorageBarClass"] = ""
	data["Tier"] = "starter"
	data["IngressFmt"] = "0 B"
	data["EgressFmt"] = "0 B"
	data["BandwidthTotalFmt"] = "0 B"
	data["BucketCount"] = 0
	data["ObjectCount"] = 0
	data["APIKeyCount"] = 0
	data["Activity"] = nil
}

// formatBytes returns a human-readable size string (e.g. "1.5 GB").
func formatBytes(b int64) string {
	if b == 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := int(math.Log(float64(b)) / math.Log(1024))
	if i >= len(units) {
		i = len(units) - 1
	}
	val := float64(b) / math.Pow(1024, float64(i))
	if val == math.Trunc(val) {
		return fmt.Sprintf("%.0f %s", val, units[i])
	}
	return fmt.Sprintf("%.1f %s", val, units[i])
}

func relativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		if days < 30 {
			return fmt.Sprintf("%d days ago", days)
		}
		return t.Format("Jan 2, 2006")
	}
}

func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
