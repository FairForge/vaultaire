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
	"geyser":   155, // $1.55/TB
	"idrive":   330, // $3.30/TB
	"hetzner":  381, // ~€3.81/TB
	"onedrive": 0,
	"gorilla":  0,
	"local":    0,
	"edge":     0,
}

// egressCostPerTBCents maps backend names to their per-TB egress cost in cents.
// Currently $0 across the board — will matter once BYOB/edge nodes are wired.
var egressCostPerTBCents = map[string]int64{
	"geyser":   0,
	"idrive":   0,
	"hetzner":  0,
	"onedrive": 0,
	"gorilla":  0,
	"local":    0,
	"edge":     0,
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

		if db != nil {
			populateCosts(r.Context(), db, data, logger)
			populateActualBackends(r.Context(), db, data, logger)
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
	backendOrder := []string{"geyser", "idrive", "hetzner", "onedrive", "gorilla", "local", "edge"}
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

func populateActualBackends(ctx context.Context, db *sql.DB, data map[string]any, logger *zap.Logger) {
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
		actual = append(actual, r)
	}

	if len(actual) > 0 {
		data["ActualByBackend"] = actual
	}
}

func daysInCurrentMonth(t time.Time) int {
	y, m, _ := t.Date()
	return time.Date(y, m+1, 0, 0, 0, 0, 0, t.Location()).Day()
}
