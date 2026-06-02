package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/billing"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

// planMonthlyCents returns the monthly price in cents for a fixed-price Vault
// plan. Metered tiers (standard, performance) return 0 — their revenue is
// calculated from live storage usage. Prices from VAULT_SERIES_ECONOMICS.md.
func planMonthlyCents(plan string) int64 {
	switch plan {
	case "vault1":
		return 499
	case "vault3":
		return 799
	case "vault5":
		return 999
	case "vault10":
		return 1299
	case "vault18":
		return 1799
	case "vault50":
		return 4499
	case "vault100":
		return 8499
	default:
		return 0
	}
}

func formatCents(cents int64) string {
	return fmt.Sprintf("$%.2f", float64(cents)/100)
}

type tierRevenue struct {
	Tier     string
	Count    int
	MRRCents int64
	MRRFmt   string
}

type custRevenue struct {
	Email      string
	Plan       string
	MRRCents   int64
	MRRFmt     string
	StorageFmt string
}

type trendBar struct {
	Label string
	Cents int64
	Fmt   string
	X     float64
	Y     float64
	W     float64
	H     float64
}

func HandleAdminRevenue(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "admin-revenue")
		withCSRF(r.Context(), data)

		data["MRRFmt"] = "$0.00"
		data["ActiveSubs"] = 0
		data["NewThisMonth"] = 0
		data["ChurnedThisMonth"] = 0
		data["ChurnRateFmt"] = "0%"
		data["ByTier"] = []tierRevenue{}
		data["TopCustomers"] = []custRevenue{}
		data["TrendBars"] = []trendBar{}
		data["TrendMaxFmt"] = "$0"

		if db != nil {
			populateRevenue(r.Context(), db, data, logger)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin revenue", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func populateRevenue(ctx context.Context, db *sql.DB, data map[string]any, logger *zap.Logger) {
	fixedCents := queryFixedMRR(ctx, db, logger)
	meteredCents := queryMeteredMRR(ctx, db, logger)
	totalMRR := fixedCents + meteredCents

	data["MRRFmt"] = formatCents(totalMRR)

	var activeSubs int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tenants WHERE subscription_status = 'active'`).Scan(&activeSubs); err != nil {
		logger.Debug("revenue: count active subs", zap.Error(err))
	}
	data["ActiveSubs"] = activeSubs

	newThisMonth := queryCountSince(ctx, db, logger,
		`SELECT COUNT(*) FROM subscriptions WHERE created_at >= date_trunc('month', CURRENT_DATE)`)
	churnedThisMonth := queryCountSince(ctx, db, logger,
		`SELECT COUNT(*) FROM subscriptions WHERE canceled_at >= date_trunc('month', CURRENT_DATE)`)
	activeAtStart := queryCountSince(ctx, db, logger,
		`SELECT COUNT(*) FROM subscriptions
		 WHERE created_at < date_trunc('month', CURRENT_DATE)
		   AND (canceled_at IS NULL OR canceled_at >= date_trunc('month', CURRENT_DATE))`)

	data["NewThisMonth"] = newThisMonth
	data["ChurnedThisMonth"] = churnedThisMonth
	if activeAtStart > 0 {
		rate := float64(churnedThisMonth) / float64(activeAtStart) * 100
		data["ChurnRateFmt"] = fmt.Sprintf("%.1f%%", rate)
	}

	data["ByTier"] = queryRevenueByTier(ctx, db, logger)
	data["TopCustomers"] = queryTopCustomers(ctx, db, logger)

	bars := queryMRRTrend(ctx, db, logger)
	data["TrendBars"] = bars
	if len(bars) > 0 {
		var maxCents int64
		for _, b := range bars {
			if b.Cents > maxCents {
				maxCents = b.Cents
			}
		}
		data["TrendMaxFmt"] = formatCents(maxCents)
	}
}

func queryFixedMRR(ctx context.Context, db *sql.DB, logger *zap.Logger) int64 {
	rows, err := db.QueryContext(ctx,
		`SELECT COALESCE(plan, '') FROM tenants WHERE subscription_status = 'active'`)
	if err != nil {
		logger.Debug("revenue: query fixed plans", zap.Error(err))
		return 0
	}
	defer func() { _ = rows.Close() }()

	var total int64
	for rows.Next() {
		var plan string
		if err := rows.Scan(&plan); err != nil {
			continue
		}
		total += planMonthlyCents(plan)
	}
	return total
}

func queryMeteredMRR(ctx context.Context, db *sql.DB, logger *zap.Logger) int64 {
	rows, err := db.QueryContext(ctx, `
		SELECT tq.tier, tq.storage_used_bytes, COALESCE(bw.egress, 0)
		FROM tenant_quotas tq
		LEFT JOIN (
			SELECT tenant_id, SUM(egress_bytes) AS egress
			FROM bandwidth_usage_daily
			WHERE date >= date_trunc('month', CURRENT_DATE)
			GROUP BY tenant_id
		) bw ON bw.tenant_id = tq.tenant_id
		WHERE tq.tier IN ('standard', 'performance')`)
	if err != nil {
		logger.Debug("revenue: query metered tenants", zap.Error(err))
		return 0
	}
	defer func() { _ = rows.Close() }()

	var total int64
	for rows.Next() {
		var tier string
		var storageBytes, egressBytes int64
		if err := rows.Scan(&tier, &storageBytes, &egressBytes); err != nil {
			continue
		}
		total += billing.AccruedCents(tier, storageBytes, egressBytes)
	}
	return total
}

func queryCountSince(ctx context.Context, db *sql.DB, logger *zap.Logger, query string) int {
	var count int
	if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		logger.Debug("revenue: count query", zap.Error(err))
	}
	return count
}

func queryRevenueByTier(ctx context.Context, db *sql.DB, logger *zap.Logger) []tierRevenue {
	// Fixed-plan tiers from tenants.
	fixedRows, err := db.QueryContext(ctx, `
		SELECT COALESCE(plan, 'free'), COUNT(*)
		FROM tenants
		WHERE subscription_status = 'active'
		GROUP BY plan
		ORDER BY COUNT(*) DESC`)
	if err != nil {
		logger.Debug("revenue: by tier fixed", zap.Error(err))
		return nil
	}
	defer func() { _ = fixedRows.Close() }()

	var tiers []tierRevenue
	for fixedRows.Next() {
		var plan string
		var count int
		if err := fixedRows.Scan(&plan, &count); err != nil {
			continue
		}
		cents := planMonthlyCents(plan) * int64(count)
		if plan == "" {
			plan = "free"
		}
		tiers = append(tiers, tierRevenue{
			Tier:     plan,
			Count:    count,
			MRRCents: cents,
			MRRFmt:   formatCents(cents),
		})
	}

	// Metered tiers from tenant_quotas.
	meteredRows, err := db.QueryContext(ctx, `
		SELECT tq.tier, COUNT(*), COALESCE(SUM(tq.storage_used_bytes), 0)
		FROM tenant_quotas tq
		WHERE tq.tier IN ('standard', 'performance')
		GROUP BY tq.tier
		ORDER BY tq.tier`)
	if err != nil {
		logger.Debug("revenue: by tier metered", zap.Error(err))
		return tiers
	}
	defer func() { _ = meteredRows.Close() }()

	for meteredRows.Next() {
		var tier string
		var count int
		var totalStorage int64
		if err := meteredRows.Scan(&tier, &count, &totalStorage); err != nil {
			continue
		}
		cents := billing.AccruedCents(tier, totalStorage, 0)
		tiers = append(tiers, tierRevenue{
			Tier:     tier,
			Count:    count,
			MRRCents: cents,
			MRRFmt:   formatCents(cents),
		})
	}
	return tiers
}

func queryTopCustomers(ctx context.Context, db *sql.DB, logger *zap.Logger) []custRevenue {
	rows, err := db.QueryContext(ctx, `
		SELECT t.email, COALESCE(t.plan, 'free'),
			   COALESCE(tq.tier, ''), COALESCE(tq.storage_used_bytes, 0)
		FROM tenants t
		LEFT JOIN tenant_quotas tq ON tq.tenant_id = t.id
		WHERE t.subscription_status = 'active'
		ORDER BY tq.storage_used_bytes DESC NULLS LAST
		LIMIT 10`)
	if err != nil {
		logger.Debug("revenue: top customers", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()

	var customers []custRevenue
	for rows.Next() {
		var email, plan, tier string
		var storageBytes int64
		if err := rows.Scan(&email, &plan, &tier, &storageBytes); err != nil {
			continue
		}
		var cents int64
		if tier == "standard" || tier == "performance" {
			cents = billing.AccruedCents(tier, storageBytes, 0)
		} else {
			cents = planMonthlyCents(plan)
		}
		displayPlan := plan
		if tier == "standard" || tier == "performance" {
			displayPlan = tier
		}
		customers = append(customers, custRevenue{
			Email:      email,
			Plan:       displayPlan,
			MRRCents:   cents,
			MRRFmt:     formatCents(cents),
			StorageFmt: formatBytes(storageBytes),
		})
	}
	return customers
}

func queryMRRTrend(ctx context.Context, db *sql.DB, logger *zap.Logger) []trendBar {
	rows, err := db.QueryContext(ctx, `
		SELECT date_trunc('month', created_at) AS month, plan, COUNT(*)
		FROM subscriptions
		WHERE created_at >= date_trunc('month', CURRENT_DATE) - INTERVAL '11 months'
		GROUP BY month, plan
		ORDER BY month`)
	if err != nil {
		logger.Debug("revenue: mrr trend", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()

	// Accumulate per-month MRR.
	type monthMRR struct {
		month time.Time
		cents int64
	}
	monthMap := make(map[string]*monthMRR)
	var orderedKeys []string
	for rows.Next() {
		var month time.Time
		var plan string
		var count int
		if err := rows.Scan(&month, &plan, &count); err != nil {
			continue
		}
		key := month.Format("2006-01")
		if _, ok := monthMap[key]; !ok {
			monthMap[key] = &monthMRR{month: month}
			orderedKeys = append(orderedKeys, key)
		}
		monthMap[key].cents += planMonthlyCents(plan) * int64(count)
	}

	if len(orderedKeys) == 0 {
		return nil
	}

	var maxCents int64
	for _, k := range orderedKeys {
		if monthMap[k].cents > maxCents {
			maxCents = monthMap[k].cents
		}
	}

	const chartW, chartH = 600.0, 160.0
	n := float64(len(orderedKeys))
	barW := chartW / n * 0.75
	gap := chartW / n * 0.25

	bars := make([]trendBar, 0, len(orderedKeys))
	for i, key := range orderedKeys {
		m := monthMap[key]
		h := 0.0
		if maxCents > 0 {
			h = float64(m.cents) / float64(maxCents) * chartH
		}
		if m.cents > 0 && h < 3 {
			h = 3
		}
		bars = append(bars, trendBar{
			Label: m.month.Format("Jan"),
			Cents: m.cents,
			Fmt:   formatCents(m.cents),
			X:     math.Round(float64(i)*(barW+gap)*100) / 100,
			Y:     math.Round((chartH-h)*100) / 100,
			W:     math.Round(barW*100) / 100,
			H:     math.Round(h*100) / 100,
		})
	}
	return bars
}
