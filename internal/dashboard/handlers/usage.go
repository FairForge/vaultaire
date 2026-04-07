package handlers

import (
	"context"
	"database/sql"
	"html/template"
	"math"
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

// ChartBar represents a single bar in the SVG bandwidth chart.
type ChartBar struct {
	X         float64
	InH       float64 // Ingress bar height
	EgH       float64 // Egress bar height (stacked on top)
	InY       float64
	EgY       float64
	W         float64
	Label     string // Date label (e.g. "Apr 3")
	ShowLabel bool   // Only show every 5th label to avoid clutter
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
	// Current month totals.
	var ingress, egress, requests int64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(ingress_bytes), 0),
		        COALESCE(SUM(egress_bytes), 0),
		        COALESCE(SUM(requests_count), 0)
		 FROM bandwidth_usage_daily
		 WHERE tenant_id = $1 AND date >= date_trunc('month', CURRENT_DATE)`,
		tenantID).Scan(&ingress, &egress, &requests)
	if err != nil {
		ingress, egress, requests = 0, 0, 0
	}

	data["IngressFmt"] = formatBytes(ingress)
	data["EgressFmt"] = formatBytes(egress)
	data["BandwidthTotalFmt"] = formatBytes(ingress + egress)
	data["RequestsCount"] = requests
}

func populateUsageChart(ctx context.Context, db *sql.DB, tenantID string, data map[string]any) {
	rows, err := db.QueryContext(ctx,
		`SELECT date, COALESCE(ingress_bytes, 0), COALESCE(egress_bytes, 0)
		 FROM bandwidth_usage_daily
		 WHERE tenant_id = $1 AND date >= CURRENT_DATE - INTERVAL '30 days'
		 ORDER BY date ASC`, tenantID)
	if err != nil {
		data["ChartBars"] = nil
		data["HasChartData"] = false
		return
	}
	defer func() { _ = rows.Close() }()

	type dayData struct {
		date    time.Time
		ingress int64
		egress  int64
	}
	var days []dayData
	var maxVal int64

	for rows.Next() {
		var d dayData
		if err := rows.Scan(&d.date, &d.ingress, &d.egress); err != nil {
			continue
		}
		total := d.ingress + d.egress
		if total > maxVal {
			maxVal = total
		}
		days = append(days, d)
	}

	if len(days) == 0 {
		data["ChartBars"] = nil
		data["HasChartData"] = false
		return
	}

	// Build SVG bar data. Chart area is 600x200.
	const chartW, chartH = 600.0, 200.0
	barW := chartW / float64(len(days)) * 0.8
	gap := chartW / float64(len(days)) * 0.2

	var bars []ChartBar
	for i, d := range days {
		x := float64(i) * (barW + gap)
		inH, egH := 0.0, 0.0
		if maxVal > 0 {
			inH = float64(d.ingress) / float64(maxVal) * chartH
			egH = float64(d.egress) / float64(maxVal) * chartH
		}
		// Minimum visible height for non-zero values.
		if d.ingress > 0 && inH < 2 {
			inH = 2
		}
		if d.egress > 0 && egH < 2 {
			egH = 2
		}

		showLabel := i%5 == 0 || i == len(days)-1
		bars = append(bars, ChartBar{
			X:         math.Round(x*100) / 100,
			InH:       math.Round(inH*100) / 100,
			EgH:       math.Round(egH*100) / 100,
			InY:       math.Round((chartH-inH)*100) / 100,
			EgY:       math.Round((chartH-inH-egH)*100) / 100,
			W:         math.Round(barW*100) / 100,
			Label:     d.date.Format("Jan 2"),
			ShowLabel: showLabel,
		})
	}

	data["ChartBars"] = bars
	data["HasChartData"] = true
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
