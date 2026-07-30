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

const (
	geyserFloorCents  = 15500 // $155/mo minimum
	gorillaFixedCents = 0     // set when Gorilla subscription cost is known

	bytesPerTBCost = 1024.0 * 1024 * 1024 * 1024
)

// backendCostPerTBCents maps backend names to their per-TB storage cost in cents.
// Zero means free (contributed, local, or fixed-only).
var backendCostPerTBCents = map[string]int64{
	"geyser":     155, // $1.55/TB
	"idrive":     330, // $3.30/TB
	"lyve":       799, // $7.99/TB — see note in internal/usage/cost_tracker.go
	"hetzner":    381, // ~€3.81/TB
	"permafrost": 0,
	"gorilla":    0,
	"local":      0,
	"edge":       0,
}

// egressCostPerTBCents maps backend names to their per-TB egress cost in cents.
//
// These are MODELLED market rates, not invoices. Lyve's own contract defines no
// egress fee (the words "egress" and "bandwidth" appear nowhere in it) — its
// limit on heavy reads is a fair-use throughput allocation, not a charge. We
// still carry a rate here so the dashboard can answer "what would this traffic
// cost at market rates", which is the bar self-hosted storage has to beat.
// $10/TB ($0.01/GB) matches the iDrive and Quotaless overage rate.
// Switch the dashboard to ?costs=invoiced to see what we are actually billed.
var egressCostPerTBCents = map[string]int64{
	"geyser":     0,
	"idrive":     0,
	"lyve":       1000, // $10/TB modelled — see subsidizedBackends
	"hetzner":    0,
	"permafrost": 0,
	"gorilla":    0,
	"local":      0,
	"edge":       0,
}

// subsidizedBackends are backends that carry a modelled rate above but bill us
// nothing today, so the two cost views diverge. Lyve is $0 under a 1-year SaaS
// promo whose end date we do not yet have in writing; when it ends, the
// modelled figure is what we start paying. The gap between the two views is
// the subsidy we are currently living on.
var subsidizedBackends = map[string]bool{
	"lyve": true,
}

// costMode selects which rate card the admin costs page applies.
type costMode string

const (
	// costModeModelled charges every backend its full rate card, including
	// backends currently on a promo. This is the planning number.
	costModeModelled costMode = "modelled"
	// costModeInvoiced zeroes subsidized backends to show real current spend.
	costModeInvoiced costMode = "invoiced"
)

func parseCostMode(s string) costMode {
	if s == string(costModeInvoiced) {
		return costModeInvoiced
	}
	return costModeModelled
}

// ratesFor returns the storage and egress rate to apply to a backend under the
// given mode. In invoiced mode a subsidized backend costs nothing.
func ratesFor(backend string, mode costMode) (storagePerTB, egressPerTB int64) {
	if mode == costModeInvoiced && subsidizedBackends[backend] {
		return 0, 0
	}
	return backendCostPerTBCents[backend], egressCostPerTBCents[backend]
}

// tierBackend maps a tenant's plan/tier to the intended storage backend.
func tierBackend(plan, tier string) string {
	if tier == "performance" {
		return "idrive"
	}
	if tier == "standard" {
		return "idrive"
	}
	switch plan {
	case "vault1", "vault3", "vault5", "vault10", "vault18", "vault50", "vault100":
		return "geyser"
	case "free", "starter", "":
		return "local"
	default:
		return "local"
	}
}

type backendCostRow struct {
	Backend    string
	StorageTB  float64
	StorageFmt string
	CostCents  int64
	CostFmt    string
	FixedCents int64
	FixedFmt   string
	TotalCents int64
	TotalFmt   string
}

type actualBackendRow struct {
	Backend     string
	ObjectCount int64
	StorageFmt  string
	StorageTB   float64
	// CostFmt is the storage cost of these real bytes under the active mode.
	CostFmt string
	// ModelledFmt is always the full-rate-card cost, regardless of mode, so a
	// subsidized backend still shows what it will cost when the promo ends.
	ModelledFmt string
	Subsidized  bool
	// EgressFmt / EgressCostFmt cover this month's attributed egress
	// (backend_bandwidth_daily); the cost follows the active mode.
	EgressFmt     string
	EgressCostFmt string
}

type marginRow struct {
	Email           string
	Plan            string
	Backend         string
	StorageFmt      string
	RevenueCents    int64
	RevenueFmt      string
	CostCents       int64
	CostFmt         string
	EgressCostCents int64
	EgressCostFmt   string
	MarginCents     int64
	MarginFmt       string
	IsNegative      bool
}

func HandleAdminCosts(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "admin-costs")
		withCSRF(r.Context(), data)

		data["EstSpendFmt"] = "$0.00"
		data["BlendedCOGSFmt"] = "$0.00/TB"
		data["GrossMarginFmt"] = "0%"
		data["NegativeMarginCount"] = 0
		data["ProjectedSpendFmt"] = "$0.00"
		data["ByBackend"] = []backendCostRow{}
		data["MarginTable"] = []marginRow{}
		data["ActualByBackend"] = []actualBackendRow{}
		data["ModelledSpendFmt"] = "$0.00"
		data["InvoicedSpendFmt"] = "$0.00"
		data["SubsidyFmt"] = "$0.00"

		mode := parseCostMode(r.URL.Query().Get("costs"))
		data["CostMode"] = string(mode)
		data["IsInvoicedView"] = mode == costModeInvoiced

		if db != nil {
			populateCosts(r.Context(), db, data, logger)
			populateActualBackends(r.Context(), db, data, logger, mode)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin costs", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func populateCosts(ctx context.Context, db *sql.DB, data map[string]any, logger *zap.Logger) {
	tenants := queryTenantCostData(ctx, db, logger)
	if len(tenants) == 0 {
		return
	}

	// Per-backend aggregation.
	type backendAgg struct {
		storageBytes int64
		costCents    int64
	}
	backends := make(map[string]*backendAgg)

	var margins []marginRow
	var totalCostCents, totalRevenueCents int64
	var totalStorageBytes int64
	var negativeCount int

	for _, t := range tenants {
		backend := tierBackend(t.plan, t.tier)
		perTB := backendCostPerTBCents[backend]
		storageTB := float64(t.storageBytes) / bytesPerTBCost
		storageCostCents := int64(math.Round(storageTB * float64(perTB)))

		egressPerTB := egressCostPerTBCents[backend]
		egressTB := float64(t.egressBytes) / bytesPerTBCost
		egressCostCents := int64(math.Round(egressTB * float64(egressPerTB)))

		costCents := storageCostCents + egressCostCents

		var revenueCents int64
		if t.tier == "standard" || t.tier == "performance" {
			revenueCents = billing.AccruedCents(t.tier, t.storageBytes, t.egressBytes)
		} else {
			revenueCents = planMonthlyCents(t.plan)
		}

		marginCents := revenueCents - costCents
		isNeg := marginCents < 0

		if isNeg {
			negativeCount++
		}

		totalCostCents += costCents
		totalRevenueCents += revenueCents
		totalStorageBytes += t.storageBytes

		// Aggregate per backend.
		agg, ok := backends[backend]
		if !ok {
			agg = &backendAgg{}
			backends[backend] = agg
		}
		agg.storageBytes += t.storageBytes
		agg.costCents += storageCostCents

		displayPlan := t.plan
		if t.tier == "standard" || t.tier == "performance" {
			displayPlan = t.tier
		}

		margins = append(margins, marginRow{
			Email:           t.email,
			Plan:            displayPlan,
			Backend:         backend,
			StorageFmt:      formatBytes(t.storageBytes),
			RevenueCents:    revenueCents,
			RevenueFmt:      formatCents(revenueCents),
			CostCents:       costCents,
			CostFmt:         formatCents(costCents),
			EgressCostCents: egressCostCents,
			EgressCostFmt:   formatCents(egressCostCents),
			MarginCents:     marginCents,
			MarginFmt:       formatSignedCents(marginCents),
			IsNegative:      isNeg,
		})
	}

	// Add fixed costs.
	totalCostCents += geyserFloorCents + gorillaFixedCents

	// Build backend table rows.
	backendOrder := []string{"geyser", "idrive", "lyve", "hetzner", "permafrost", "gorilla", "local", "edge"}
	var byBackend []backendCostRow
	for _, name := range backendOrder {
		agg := backends[name]
		if agg == nil {
			continue
		}
		storageTB := float64(agg.storageBytes) / bytesPerTBCost
		fixedCents := int64(0)
		if name == "geyser" {
			fixedCents = geyserFloorCents
		}
		if name == "gorilla" {
			fixedCents = gorillaFixedCents
		}
		totalCents := agg.costCents + fixedCents

		byBackend = append(byBackend, backendCostRow{
			Backend:    name,
			StorageTB:  math.Round(storageTB*100) / 100,
			StorageFmt: fmt.Sprintf("%.2f TB", storageTB),
			CostCents:  agg.costCents,
			CostFmt:    formatCents(agg.costCents),
			FixedCents: fixedCents,
			FixedFmt:   formatCents(fixedCents),
			TotalCents: totalCents,
			TotalFmt:   formatCents(totalCents),
		})
	}

	// Cards.
	data["EstSpendFmt"] = formatCents(totalCostCents)

	totalTB := float64(totalStorageBytes) / bytesPerTBCost
	if totalTB > 0 {
		blended := float64(totalCostCents) / totalTB
		data["BlendedCOGSFmt"] = fmt.Sprintf("$%.2f/TB", blended/100)
	}

	if totalRevenueCents > 0 {
		marginPct := float64(totalRevenueCents-totalCostCents) / float64(totalRevenueCents) * 100
		data["GrossMarginFmt"] = fmt.Sprintf("%.1f%%", marginPct)
	}

	data["NegativeMarginCount"] = negativeCount
	data["ByBackend"] = byBackend
	data["MarginTable"] = margins

	// Projected month-end spend (linear from current day-of-month).
	now := time.Now().UTC()
	dayOfMonth := now.Day()
	daysInMonth := daysInCurrentMonth(now)
	if dayOfMonth > 0 {
		projected := float64(totalCostCents) * float64(daysInMonth) / float64(dayOfMonth)
		data["ProjectedSpendFmt"] = formatCents(int64(math.Round(projected)))
	}
}

type tenantCostData struct {
	email        string
	plan         string
	tier         string
	storageBytes int64
	egressBytes  int64
}

func queryTenantCostData(ctx context.Context, db *sql.DB, logger *zap.Logger) []tenantCostData {
	rows, err := db.QueryContext(ctx, `
		SELECT t.email, COALESCE(t.plan, ''),
		       COALESCE(tq.tier, ''), COALESCE(tq.storage_used_bytes, 0),
		       COALESCE(bw.egress, 0)
		FROM tenants t
		LEFT JOIN tenant_quotas tq ON tq.tenant_id = t.id
		LEFT JOIN (
			SELECT tenant_id, SUM(egress_bytes) AS egress
			FROM bandwidth_usage_daily
			WHERE date >= date_trunc('month', CURRENT_DATE)
			GROUP BY tenant_id
		) bw ON bw.tenant_id = t.id
		WHERE t.subscription_status = 'active'
		ORDER BY tq.storage_used_bytes DESC NULLS LAST`)
	if err != nil {
		logger.Debug("costs: query tenant data", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()

	var result []tenantCostData
	for rows.Next() {
		var td tenantCostData
		if err := rows.Scan(&td.email, &td.plan, &td.tier, &td.storageBytes, &td.egressBytes); err != nil {
			logger.Debug("costs: scan tenant", zap.Error(err))
			continue
		}
		result = append(result, td)
	}
	return result
}

func formatSignedCents(cents int64) string {
	if cents < 0 {
		return fmt.Sprintf("-$%.2f", float64(-cents)/100)
	}
	return fmt.Sprintf("$%.2f", float64(cents)/100)
}

func populateActualBackends(ctx context.Context, db *sql.DB, data map[string]any, logger *zap.Logger, mode costMode) {
	egress := queryBackendEgress(ctx, db, logger)

	rows, err := db.QueryContext(ctx, `
		SELECT backend_name, COUNT(*), COALESCE(SUM(size_bytes), 0)
		FROM object_locations GROUP BY backend_name
		ORDER BY SUM(size_bytes) DESC`)
	if err != nil {
		logger.Debug("costs: query object_locations", zap.Error(err))
		return
	}
	defer func() { _ = rows.Close() }()

	var actual []actualBackendRow
	var modelledTotal, invoicedTotal int64
	for rows.Next() {
		var r actualBackendRow
		var storageBytes int64
		if err := rows.Scan(&r.Backend, &r.ObjectCount, &storageBytes); err != nil {
			logger.Debug("costs: scan actual backend", zap.Error(err))
			continue
		}
		storageTB := float64(storageBytes) / bytesPerTBCost
		r.StorageTB = math.Round(storageTB*100) / 100
		r.StorageFmt = formatBytes(storageBytes)

		perTB, egressPerTB := ratesFor(r.Backend, mode)
		modelledPerTB := backendCostPerTBCents[r.Backend]
		costCents := int64(math.Round(storageTB * float64(perTB)))
		modelledCents := int64(math.Round(storageTB * float64(modelledPerTB)))

		egressBytes := egress[r.Backend]
		egressTB := float64(egressBytes) / bytesPerTBCost
		egressCostCents := int64(math.Round(egressTB * float64(egressPerTB)))
		egressModelledCents := int64(math.Round(egressTB * float64(egressCostPerTBCents[r.Backend])))

		r.CostFmt = formatCents(costCents)
		r.ModelledFmt = formatCents(modelledCents)
		r.Subsidized = subsidizedBackends[r.Backend]
		r.EgressFmt = formatBytes(egressBytes)
		r.EgressCostFmt = formatCents(egressCostCents)

		modelledTotal += modelledCents + egressModelledCents
		if mode == costModeInvoiced {
			invoicedTotal += costCents + egressCostCents
		} else if !r.Subsidized {
			invoicedTotal += costCents + egressCostCents
		}

		actual = append(actual, r)
	}

	data["ModelledSpendFmt"] = formatCents(modelledTotal)
	data["InvoicedSpendFmt"] = formatCents(invoicedTotal)
	data["SubsidyFmt"] = formatCents(modelledTotal - invoicedTotal)

	if len(actual) > 0 {
		data["ActualByBackend"] = actual
	}
}

// queryBackendEgress returns this month's attributed egress bytes per backend
// from backend_bandwidth_daily (migration 060). Degrades to an empty map when
// the table is missing or the query fails — storage costing must survive.
func queryBackendEgress(ctx context.Context, db *sql.DB, logger *zap.Logger) map[string]int64 {
	out := map[string]int64{}
	rows, err := db.QueryContext(ctx, `
		SELECT backend_name, COALESCE(SUM(egress_bytes), 0)
		FROM backend_bandwidth_daily
		WHERE date >= date_trunc('month', CURRENT_DATE)
		GROUP BY backend_name`)
	if err != nil {
		logger.Debug("costs: query backend egress", zap.Error(err))
		return out
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var bytes int64
		if err := rows.Scan(&name, &bytes); err != nil {
			logger.Debug("costs: scan backend egress", zap.Error(err))
			continue
		}
		out[name] = bytes
	}
	return out
}

func daysInCurrentMonth(t time.Time) int {
	y, m, _ := t.Date()
	return time.Date(y, m+1, 0, 0, 0, 0, 0, t.Location()).Day()
}
