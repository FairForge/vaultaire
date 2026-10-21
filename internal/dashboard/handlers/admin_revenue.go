package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"sort"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/lib/pq"
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
// house at payments.QuoteHouse for its period — the sheet the customer saw.
// Since WP-PAY-7 every number on the page comes from the entitlement record
// (payments.LiveHouses / CountLiveHouses / HouseMonthOf): a house is live
// when its row is granted, whatever tenants.plan or a provider's status
// says; new / churned are first grants / lapses in the UTC month.
func populateRevenue(ctx context.Context, db *sql.DB, data map[string]any, logger *zap.Logger) {
	houses := queryHouses(ctx, db, logger)
	data["MRRFmt"] = formatCents(housesMRR(houses))

	activeSubs, err := payments.CountLiveHouses(ctx, db)
	if err != nil {
		logger.Debug("revenue: count live houses", zap.Error(err))
	}
	data["ActiveSubs"] = activeSubs

	m, err := payments.HouseMonthOf(ctx, db)
	if err != nil {
		logger.Debug("revenue: house month", zap.Error(err))
	}
	data["NewThisMonth"] = m.New
	data["ChurnedThisMonth"] = m.Lapsed
	if m.LiveAtStart > 0 {
		rate := float64(m.Lapsed) / float64(m.LiveAtStart) * 100
		data["ChurnRateFmt"] = fmt.Sprintf("%.1f%%", rate)
	}

	data["ByTier"] = houseRevenueLines(houses)
	data["TopCustomers"] = topHouseCustomers(houses, 10)
}

// houseRevenue is one live house, priced.
type houseRevenue struct {
	tenantID, email string
	std, vault, pin int
	period          payments.Period
	usedBytes       int64
	quote           payments.Quote
}

// queryHouses reads every live house from the entitlement record — the
// house as bought (its lines and period), priced at payments.QuoteHouse —
// with its owner's e-mail and stored bytes.
func queryHouses(ctx context.Context, db *sql.DB, logger *zap.Logger) []houseRevenue {
	live, err := payments.LiveHouses(ctx, db)
	if err != nil {
		logger.Debug("revenue: houses", zap.Error(err))
		return nil
	}
	if len(live) == 0 {
		return nil
	}
	ids := make([]string, len(live))
	for i, e := range live {
		ids[i] = e.TenantID
	}
	type owner struct {
		email string
		used  int64
	}
	owners := map[string]owner{}
	rows, err := db.QueryContext(ctx, `
		SELECT t.id, t.email, COALESCE(q.storage_used_bytes, 0)
		  FROM tenants t LEFT JOIN tenant_quotas q ON q.tenant_id = t.id
		 WHERE t.id = ANY($1)`, pq.Array(ids))
	if err != nil {
		logger.Debug("revenue: house owners", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var o owner
		if err := rows.Scan(&id, &o.email, &o.used); err != nil {
			continue
		}
		owners[id] = o
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	out := make([]houseRevenue, 0, len(live))
	for _, e := range live {
		o := e.Order
		h := houseRevenue{tenantID: e.TenantID, email: owners[e.TenantID].email, usedBytes: owners[e.TenantID].used,
			std: o.Std, vault: o.Vault, pin: o.PinHot, period: o.Period}
		h.quote = payments.QuoteHouse(h.std, h.vault, h.pin, h.period)
		out = append(out, h)
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
