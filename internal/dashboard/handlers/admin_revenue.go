package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"sort"

	"github.com/FairForge/vaultaire/internal/billing"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/usage"
	"go.uber.org/zap"
)

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

// populateRevenue prices houses only (WP-PAY-0, one product): every live
// house at billing.QuoteHouse for its period — the sheet the customer saw.
// The legacy pack prices and the metered estimate are gone; a tenant on a
// pack label without a house (Bench2) earns nothing. The month trend read
// the subscriptions table, which nothing writes (R10-14): it is gone until
// the payments ledger feeds one (WP-PAY-3).
func populateRevenue(ctx context.Context, db *sql.DB, data map[string]any, logger *zap.Logger) {
	houses := queryHouses(ctx, db, logger)
	data["MRRFmt"] = formatCents(housesMRR(houses))

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

	data["ByTier"] = houseRevenueLines(houses)
	data["TopCustomers"] = topHouseCustomers(houses, 10)
}

// houseRevenue is one live house, priced.
type houseRevenue struct {
	tenantID, email string
	std, vault, pin int
	period          billing.Period
	usedBytes       int64
	quote           billing.Quote
}

// queryHouses reads every live house (dashboard plan Phase 4): the floor
// quotas and pin-hot are the Stripe items' quantities, so the MRR is
// billing.QuoteHouse at the tenant's period.
func queryHouses(ctx context.Context, db *sql.DB, logger *zap.Logger) []houseRevenue {
	rows, err := db.QueryContext(ctx, `
		SELECT t.id, t.email, t.house_period, f.floor, f.storage_limit_bytes,
		       COALESCE(q.pin_hot_bytes, 0), COALESCE(q.storage_used_bytes, 0)
		FROM tenants t
		JOIN tenant_floor_quotas f ON f.tenant_id = t.id
		LEFT JOIN tenant_quotas q ON q.tenant_id = t.id
		WHERE t.plan = 'house' AND t.subscription_status IN ('active', 'past_due', 'trialing')
		ORDER BY t.id`)
	if err != nil {
		logger.Debug("revenue: houses", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()

	byID := map[string]*houseRevenue{}
	var order []string
	for rows.Next() {
		var id, email, period, floor string
		var limit, pin, used int64
		if err := rows.Scan(&id, &email, &period, &floor, &limit, &pin, &used); err != nil {
			continue
		}
		h, ok := byID[id]
		if !ok {
			h = &houseRevenue{tenantID: id, email: email, period: billing.Period(period), pin: int(pin / usage.TB), usedBytes: used}
			if h.period != billing.PeriodMonthly {
				h.period = billing.PeriodAnnual
			}
			byID[id] = h
			order = append(order, id)
		}
		switch floor {
		case usage.FloorStandard:
			h.std = int(limit / usage.TB)
		case usage.FloorVault:
			h.vault = int(limit / usage.TB)
		}
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	out := make([]houseRevenue, 0, len(order))
	for _, id := range order {
		h := byID[id]
		h.quote = billing.QuoteHouse(h.std, h.vault, h.pin, h.period)
		out = append(out, *h)
	}
	return out
}

// housesMRR is the houses' monthly total in cents.
func housesMRR(houses []houseRevenue) int64 {
	var total int64
	for _, h := range houses {
		total += h.quote.MonthlyCents
	}
	return total
}

// houseQuoteByTenant is each house's monthly cents by tenant id (the costs
// page's revenue side).
func houseQuoteByTenant(houses []houseRevenue) map[string]int64 {
	m := make(map[string]int64, len(houses))
	for _, h := range houses {
		m[h.tenantID] = h.quote.MonthlyCents
	}
	return m
}

// houseRevenueLines is one revenue line per floor/add-on.
func houseRevenueLines(houses []houseRevenue) []tierRevenue {
	var stdCents, vaultCents, pinCents int64
	var stdN, vaultN, pinN int
	for _, h := range houses {
		for _, l := range h.quote.Lines {
			switch l.Label {
			case "Downstairs":
				stdCents += l.Cents
				stdN++
			case "Attic":
				vaultCents += l.Cents
				vaultN++
			case "Pin-hot":
				pinCents += l.Cents
				pinN++
			}
		}
	}
	lines := []tierRevenue{}
	if stdN > 0 {
		lines = append(lines, tierRevenue{Tier: "downstairs (Standard)", Count: stdN, MRRCents: stdCents, MRRFmt: formatCents(stdCents)})
	}
	if vaultN > 0 {
		lines = append(lines, tierRevenue{Tier: "attic (Vault)", Count: vaultN, MRRCents: vaultCents, MRRFmt: formatCents(vaultCents)})
	}
	if pinN > 0 {
		lines = append(lines, tierRevenue{Tier: "pin-hot", Count: pinN, MRRCents: pinCents, MRRFmt: formatCents(pinCents)})
	}
	return lines
}

// topHouseCustomers is the n houses with the highest MRR.
func topHouseCustomers(houses []houseRevenue, n int) []custRevenue {
	sorted := append([]houseRevenue(nil), houses...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].quote.MonthlyCents > sorted[j].quote.MonthlyCents })
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	out := []custRevenue{}
	for _, h := range sorted {
		out = append(out, custRevenue{
			Email: h.email, Plan: "house (" + string(h.period) + ")",
			MRRCents: h.quote.MonthlyCents, MRRFmt: formatCents(h.quote.MonthlyCents),
			StorageFmt: formatBytes(h.usedBytes),
		})
	}
	return out
}

func queryCountSince(ctx context.Context, db *sql.DB, logger *zap.Logger, query string) int {
	var count int
	if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		logger.Debug("revenue: count query", zap.Error(err))
	}
	return count
}
