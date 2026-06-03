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

// BackendLocation maps a storage backend to its physical location.
type BackendLocation struct {
	City    string
	Country string
	Lat     float64
	Lon     float64
}

var backendLocations = map[string]BackendLocation{
	"local":     {City: "Salt Lake City", Country: "US", Lat: 40.76, Lon: -111.89},
	"s3":        {City: "US East (Virginia)", Country: "US", Lat: 39.04, Lon: -77.49},
	"quotaless": {City: "EU (Germany)", Country: "DE", Lat: 50.11, Lon: 8.68},
	"geyser":    {City: "London", Country: "GB", Lat: 51.51, Lon: -0.13},
	"idrive":    {City: "Los Angeles", Country: "US", Lat: 34.05, Lon: -118.24},
	"lyve":      {City: "US West (Phoenix)", Country: "US", Lat: 33.45, Lon: -112.07},
}

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
func HandleOverview(tmpl *template.Template, db *sql.DB, logger *zap.Logger, storageMode string) http.HandlerFunc {
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
		withCSRF(r.Context(), data)

		ctx := r.Context()

		if db != nil {
			populateStorageUsage(ctx, db, sd.TenantID, data)
			populateBandwidth(ctx, db, sd.TenantID, data)
			populateCounts(ctx, db, sd.TenantID, sd.UserID, data)
			populateActivity(ctx, db, sd.TenantID, data)
			populateEmailVerified(ctx, db, sd.UserID, data)
			populateOnboarding(ctx, db, sd.TenantID, r, data)
			populateCarbonBadge(ctx, db, sd.TenantID, data)
		} else {
			setDefaults(data)
		}

		populateLocality(storageMode, data)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render dashboard overview", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func populateLocality(storageMode string, data map[string]any) {
	loc, ok := backendLocations[storageMode]
	if !ok {
		loc = backendLocations["local"]
	}

	data["LocalityCity"] = loc.City
	data["LocalityCountry"] = loc.Country
	data["LocalityLat"] = loc.Lat
	data["LocalityLon"] = loc.Lon
	data["LocalityLabel"] = fmt.Sprintf("%s, %s", loc.City, loc.Country)
	data["LocalityMultiRegion"] = false

	data["LocalityDotX"] = math.Round((loc.Lon + 180.0) / 360.0 * 200.0)
	data["LocalityDotY"] = math.Round((90.0 - loc.Lat) / 180.0 * 100.0)
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
	data["ShowUpgradeCTA"] = tier == "free" && pct >= 80
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

// backendEnergyKWhPerTBMonth maps backends to energy consumption in kWh/TB/month.
var backendEnergyKWhPerTBMonth = map[string]float64{
	"geyser":   0.1, // tape, powered off
	"idrive":   1.0, // spinning disk
	"s3":       1.0,
	"lyve":     1.0,
	"onedrive": 0.5, // SSD
	"local":    1.0,
}

const (
	baselineEnergyKWhPerTBMonth = 1.0 // all spinning disk
	carbonFactorKgPerKWh        = 0.4
)

func populateCarbonBadge(ctx context.Context, db *sql.DB, tenantID string, data map[string]any) {
	rows, err := db.QueryContext(ctx,
		`SELECT backend_name, COALESCE(SUM(size_bytes), 0)
		 FROM object_locations WHERE tenant_id = $1
		 GROUP BY backend_name`, tenantID)
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()

	var totalBytes int64
	var actualKWh float64
	for rows.Next() {
		var backend string
		var sizeBytes int64
		if err := rows.Scan(&backend, &sizeBytes); err != nil {
			continue
		}
		totalBytes += sizeBytes
		energy := baselineEnergyKWhPerTBMonth
		if e, ok := backendEnergyKWhPerTBMonth[backend]; ok {
			energy = e
		}
		tb := float64(sizeBytes) / (1024 * 1024 * 1024 * 1024)
		actualKWh += tb * energy
	}

	if totalBytes == 0 {
		return
	}

	totalTB := float64(totalBytes) / (1024 * 1024 * 1024 * 1024)
	baselineKWh := totalTB * baselineEnergyKWhPerTBMonth
	savingsKWh := baselineKWh - actualKWh
	if savingsKWh <= 0 {
		return
	}

	co2SavedKg := savingsKWh * carbonFactorKgPerKWh
	pct := int(savingsKWh / baselineKWh * 100)

	data["HasCarbonData"] = true
	data["CarbonSavedKg"] = fmt.Sprintf("%.1f", co2SavedKg)
	data["CarbonSavedPercent"] = pct
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
