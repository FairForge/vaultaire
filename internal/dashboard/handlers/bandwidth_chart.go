package handlers

import (
	"context"
	"database/sql"
	"math"
	"time"
)

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

// BandwidthDay holds one day's bandwidth totals.
type BandwidthDay struct {
	Date    time.Time
	Ingress int64
	Egress  int64
}

// BuildChartBars converts a slice of daily bandwidth data into SVG bar
// coordinates. Chart area is 600x200. Returns nil for empty input.
func BuildChartBars(days []BandwidthDay) []ChartBar {
	if len(days) == 0 {
		return nil
	}

	var maxVal int64
	for _, d := range days {
		total := d.Ingress + d.Egress
		if total > maxVal {
			maxVal = total
		}
	}

	const chartW, chartH = 600.0, 200.0
	barW := chartW / float64(len(days)) * 0.8
	gap := chartW / float64(len(days)) * 0.2

	bars := make([]ChartBar, 0, len(days))
	for i, d := range days {
		x := float64(i) * (barW + gap)
		inH, egH := 0.0, 0.0
		if maxVal > 0 {
			inH = float64(d.Ingress) / float64(maxVal) * chartH
			egH = float64(d.Egress) / float64(maxVal) * chartH
		}
		if d.Ingress > 0 && inH < 2 {
			inH = 2
		}
		if d.Egress > 0 && egH < 2 {
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
			Label:     d.Date.Format("Jan 2"),
			ShowLabel: showLabel,
		})
	}
	return bars
}

// QueryBandwidthDays fetches the last 30 days of bandwidth data for a tenant.
func QueryBandwidthDays(ctx context.Context, db *sql.DB, tenantID string) ([]BandwidthDay, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT date, COALESCE(ingress_bytes, 0), COALESCE(egress_bytes, 0)
		 FROM bandwidth_usage_daily
		 WHERE tenant_id = $1 AND date >= CURRENT_DATE - INTERVAL '30 days'
		 ORDER BY date ASC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var days []BandwidthDay
	for rows.Next() {
		var d BandwidthDay
		if err := rows.Scan(&d.Date, &d.Ingress, &d.Egress); err != nil {
			continue
		}
		days = append(days, d)
	}
	return days, nil
}

// QueryMonthBandwidth returns the current month's ingress, egress, and request totals.
func QueryMonthBandwidth(ctx context.Context, db *sql.DB, tenantID string) (ingress, egress, requests int64, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(ingress_bytes), 0),
		        COALESCE(SUM(egress_bytes), 0),
		        COALESCE(SUM(requests_count), 0)
		 FROM bandwidth_usage_daily
		 WHERE tenant_id = $1 AND date >= date_trunc('month', CURRENT_DATE)`,
		tenantID).Scan(&ingress, &egress, &requests)
	return
}
