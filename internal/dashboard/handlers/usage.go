package handlers

import (
	"context"
	"database/sql"
	"html/template"
	"net/http"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

// UsageDayRow is one day's bandwidth for the usage history table.
type UsageDayRow struct {
	Date       string
	IngressFmt string
	EgressFmt  string
	TotalFmt   string
	Requests   int64
}

// HandleUsage renders the usage page with storage, bandwidth, and a
// 30-day SVG bar chart. Supports htmx partial refresh.
func HandleUsage(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "usage")
		withCSRF(r.Context(), data)
		ctx := r.Context()

		if db != nil {
			populateUsageStorage(ctx, db, sd.TenantID, data)
			populateUsageBandwidth(ctx, db, sd.TenantID, data)
			populateUsageChart(ctx, db, sd.TenantID, data)
			populateUsageHistory(ctx, db, sd.TenantID, data)
		} else {
			setUsageDefaults(data)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render usage page", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func populateUsageStorage(ctx context.Context, db *sql.DB, tenantID string, data map[string]any) {
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
		data["StorageUsedBytes"] = int64(0)
		data["StorageLimitBytes"] = int64(1099511627776)
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
	data["StorageUsedBytes"] = used
	data["StorageLimitBytes"] = limit
	data["Tier"] = tier
}

func populateUsageBandwidth(ctx context.Context, db *sql.DB, tenantID string, data map[string]any) {
	ingress, egress, requests, err := QueryMonthBandwidth(ctx, db, tenantID)
	if err != nil {
		ingress, egress, requests = 0, 0, 0
	}

	data["IngressFmt"] = formatBytes(ingress)
	data["EgressFmt"] = formatBytes(egress)
	data["BandwidthTotalFmt"] = formatBytes(ingress + egress)
	data["RequestsCount"] = requests
}

func populateUsageChart(ctx context.Context, db *sql.DB, tenantID string, data map[string]any) {
	days, err := QueryBandwidthDays(ctx, db, tenantID)
	if err != nil || len(days) == 0 {
		data["ChartBars"] = nil
		data["HasChartData"] = false
		return
	}

	bars := BuildChartBars(days)
	data["ChartBars"] = bars
	data["HasChartData"] = true

	// Compute max for the chart label.
	var maxVal int64
	for _, d := range days {
		total := d.Ingress + d.Egress
		if total > maxVal {
			maxVal = total
		}
	}
	data["ChartMaxLabel"] = formatBytes(maxVal)
}

func populateUsageHistory(ctx context.Context, db *sql.DB, tenantID string, data map[string]any) {
	rows, err := db.QueryContext(ctx,
		`SELECT date,
		        COALESCE(ingress_bytes, 0),
		        COALESCE(egress_bytes, 0),
		        COALESCE(requests_count, 0)
		 FROM bandwidth_usage_daily
		 WHERE tenant_id = $1 AND date >= CURRENT_DATE - INTERVAL '30 days'
		 ORDER BY date DESC`, tenantID)
	if err != nil {
		data["UsageHistory"] = nil
		return
	}
	defer func() { _ = rows.Close() }()

	var history []UsageDayRow
	for rows.Next() {
		var date time.Time
		var ingress, egress, reqs int64
		if err := rows.Scan(&date, &ingress, &egress, &reqs); err != nil {
			continue
		}
		history = append(history, UsageDayRow{
			Date:       date.Format("Jan 2, 2006"),
			IngressFmt: formatBytes(ingress),
			EgressFmt:  formatBytes(egress),
			TotalFmt:   formatBytes(ingress + egress),
			Requests:   reqs,
		})
	}
	data["UsageHistory"] = history
}

func setUsageDefaults(data map[string]any) {
	data["StorageUsedFmt"] = "0 B"
	data["StorageLimitFmt"] = "1 TB"
	data["StoragePercent"] = 0
	data["StorageBarClass"] = ""
	data["StorageUsedBytes"] = int64(0)
	data["StorageLimitBytes"] = int64(1099511627776)
	data["Tier"] = "starter"
	data["IngressFmt"] = "0 B"
	data["EgressFmt"] = "0 B"
	data["BandwidthTotalFmt"] = "0 B"
	data["RequestsCount"] = int64(0)
	data["ChartBars"] = nil
	data["HasChartData"] = false
	data["ChartMaxLabel"] = "0 B"
	data["UsageHistory"] = nil
}
