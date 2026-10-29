package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/FairForge/vaultaire/internal/tenant"
	"html/template"
	"math"
	"net/http"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/usage"
	"go.uber.org/zap"
)

// FlagHouseOverview draws the house on the overview (dashboard plan Phase
// 2). Registered in api/server.go with default off; per tenant, then global.
const FlagHouseOverview = "house_overview"

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
	"wasabi":    {City: "Hillsboro, Oregon", Country: "US", Lat: 45.52, Lon: -122.99},
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
// overview page with real data from PostgreSQL. fl may be nil (the house
// stays off); eg may be nil (the egress bar reads the recorded month instead
// of the server's live counter).
func HandleOverview(tmpl *template.Template, db *sql.DB, logger *zap.Logger, storageMode string, fl *flags.Service, eg usage.EgressStatusReader) http.HandlerFunc {
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
			populateTestData(ctx, db, sd.TenantID, data)
			populateActivity(ctx, db, sd.TenantID, data)
			populateEmailVerified(ctx, db, sd.UserID, data)
			populateOnboarding(ctx, db, sd.TenantID, r, data)
			populateCarbonBadge(ctx, db, sd.TenantID, data)
			populateHouseOverview(ctx, db, fl, eg, sd.TenantID, data)
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
	// The part of "used" that is multipart uploads in progress: their parts
	// are reserved as they arrive (WP-R10-8 Part C).
	var uploads int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_bytes), 0) FROM multipart_uploads
		WHERE tenant_id = $1 AND status = 'active'`, tenantID).Scan(&uploads); err == nil && uploads > 0 {
		data["StorageUploadsFmt"] = formatBytes(uploads)
	}
	data["StoragePercent"] = pct
	data["StorageBarClass"] = barClass
	data["Tier"] = tier
	data["ShowUpgradeCTA"] = tier == "free" && pct >= 80
}

// populateHouseOverview builds the house (flag house_overview) and the
// egress bar. Floors come from tenant_floor_quotas when the tenant bought a
// house; otherwise the single total quota is the downstairs and there is no
// attic. The egress bar reads the one allowance (internal/usage) and the one
// month counter the throttle uses (WP-R10-9); past the allowance it says
// what is true of this tenant — the rate and the reset date when downloads
// are being paced.
func populateHouseOverview(ctx context.Context, db *sql.DB, fl *flags.Service, eg usage.EgressStatusReader, tenantID string, data map[string]any) {
	var used, limit int64
	if err := db.QueryRowContext(ctx,
		`SELECT storage_used_bytes, storage_limit_bytes FROM tenant_quotas WHERE tenant_id = $1`,
		tenantID).Scan(&used, &limit); err != nil {
		return
	}
	std := FloorState{Floor: usage.FloorStandard, LimitBytes: limit, UsedBytes: used}
	vault := FloorState{Floor: usage.FloorVault}
	floors, err := usage.NewQuotaManager(db).GetFloors(ctx, tenantID)
	if err == nil && len(floors) > 0 {
		std, vault = FloorState{Floor: usage.FloorStandard}, FloorState{Floor: usage.FloorVault}
		for _, f := range floors {
			switch f.Floor {
			case usage.FloorStandard:
				std.LimitBytes, std.UsedBytes = f.LimitBytes, f.UsedBytes
			case usage.FloorVault:
				vault.LimitBytes, vault.UsedBytes = f.LimitBytes, f.UsedBytes
			}
		}
	}
	hasHouse := len(floors) > 0

	populateEgress(ctx, db, eg, tenantID, data)

	if fl == nil || !fl.Enabled(FlagHouseOverview, tenantID) {
		return
	}

	// bucket bytes per floor, for the pieces' names and links
	rows, err := db.QueryContext(ctx, bucketFloorBytesSQL, tenantID)
	if err == nil {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var b, floor string
			var n int64
			if rows.Scan(&b, &floor, &n) != nil {
				continue
			}
			if floor == usage.FloorVault && hasHouse {
				vault.Buckets = append(vault.Buckets, BucketBytes{Name: b, Bytes: n})
			} else {
				std.Buckets = append(std.Buckets, BucketBytes{Name: b, Bytes: n})
			}
		}
		if err := rows.Err(); err != nil {
			data["RowsError"] = err.Error()
		}
	}
	if !hasHouse {
		// A legacy tenant's attic bytes still count on the single quota.
		vault = FloorState{Floor: usage.FloorVault}
	}
	var room string
	_ = db.QueryRowContext(ctx, `SELECT intent_room FROM tenants WHERE id = $1`, tenantID).Scan(&room)

	scene := BuildHouseScene(std, vault, room)
	data["HouseScene"] = scene
	data["HouseOn"] = true
}

func populateBandwidth(ctx context.Context, db *sql.DB, tenantID string, data map[string]any) {
	var ingress, egress int64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(ingress_bytes), 0), COALESCE(SUM(egress_bytes), 0)
		 FROM bandwidth_usage_daily
		 WHERE tenant_id = $1 AND date >= date_trunc('month', NOW() AT TIME ZONE 'UTC')::date`,
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

// populateActivity lists the tenant's five most recent writes from the head
// rows (what is true now: the object, its size, when it was written). It
// read quota_usage_events, whose only rows were keyless 'RESERVE'
// reservations (R10-25, WP-R10-8 Part D). The system bucket is never shown.
func populateActivity(ctx context.Context, db *sql.DB, tenantID string, data map[string]any) {
	rows, err := db.QueryContext(ctx,
		`SELECT bucket, object_key, size_bytes, updated_at
		 FROM object_head_cache
		 WHERE tenant_id = $1 AND bucket <> $2
		 ORDER BY updated_at DESC
		 LIMIT 5`, tenantID, tenant.ExportsBucket)
	if err != nil {
		data["Activity"] = nil
		return
	}
	defer func() { _ = rows.Close() }()

	var activity []ActivityRow
	for rows.Next() {
		var bucket, key string
		var size int64
		var ts time.Time
		if err := rows.Scan(&bucket, &key, &size, &ts); err != nil {
			continue
		}
		// Truncate long keys for display.
		displayKey := bucket + "/" + key
		if len(displayKey) > 60 {
			displayKey = displayKey[:57] + "..."
		}
		activity = append(activity, ActivityRow{
			Operation:  "Upload",
			ObjectKey:  displayKey,
			SizeFmt:    formatBytes(size),
			TimeFmt:    relativeTime(ts),
			BadgeClass: "success",
		})
	}
	if err := rows.Err(); err != nil {
		data["RowsError"] = err.Error()
	}
	data["Activity"] = activity
}

// backendEnergyKWhPerTBMonth maps backends to energy consumption in kWh/TB/month.
var backendEnergyKWhPerTBMonth = map[string]float64{
	"geyser":     0.1, // tape, powered off
	"idrive":     1.0, // spinning disk
	"wasabi":     1.0,
	"s3":         1.0,
	"lyve":       1.0,
	"permafrost": 0.5, // SSD (OneDrive fleet; registration key, R7-10)
	"onedrive":   0.5, // legacy bench rows written under the old key
	"local":      1.0,
}

const (
	baselineEnergyKWhPerTBMonth = 1.0 // all spinning disk
	carbonFactorKgPerKWh        = 0.4
)

// tenantBytesByBackendSQL: where a tenant's bytes are, from the tables of
// record (WP-R6-1: object_locations is no longer written) — whole objects
// on their head row's backend, chunked objects on each chunk's backend.
const tenantBytesByBackendSQL = `
	SELECT backend, COALESCE(SUM(size_bytes), 0)::BIGINT FROM (
		SELECT backend_name AS backend, size_bytes FROM object_head_cache
		 WHERE tenant_id = $1 AND NOT is_chunked AND COALESCE(backend_name, '') <> ''
		UNION ALL
		SELECT g.backend_id, g.size_bytes FROM tenant_chunk_refs r
		  JOIN global_content_index g ON g.dedup_scope = r.dedup_scope AND g.plaintext_hash = r.plaintext_hash
		 WHERE r.tenant_id = $1 AND g.state = 'ready'
	) b GROUP BY backend`

func populateCarbonBadge(ctx context.Context, db *sql.DB, tenantID string, data map[string]any) {
	rows, err := db.QueryContext(ctx, tenantBytesByBackendSQL, tenantID)
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
	if err := rows.Err(); err != nil {
		data["RowsError"] = err.Error()
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

// bucketFloorBytesSQL: a tenant's billed bytes (usage/billed.go) per bucket
// and floor — current objects plus retained older versions, so the house's
// pieces add up to the floors.
const bucketFloorBytesSQL = `SELECT bucket, floor, COALESCE(SUM(size_bytes), 0) FROM (
		     SELECT bucket, floor, size_bytes FROM object_head_cache WHERE tenant_id = $1
		     UNION ALL
		     SELECT v.bucket, ` + usage.RetainedVersionFloor + `, v.size_bytes FROM ` + usage.RetainedVersionsFrom + ` AND v.tenant_id = $1
		 ) billed GROUP BY bucket, floor`

// populateTestData fills the "Test data" card (WP-DX-1) once the tenant has
// a sandbox: what its VLT_TEST_ keys wrote, and how many are live.
func populateTestData(ctx context.Context, db *sql.DB, tenantID string, data map[string]any) {
	var sandbox string
	if err := db.QueryRowContext(ctx, `SELECT id FROM tenants WHERE sandbox_of = $1`, tenantID).Scan(&sandbox); err != nil {
		return // none (or unreadable: the card is not shown)
	}
	var objects, bytes, keys int64
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM object_head_cache WHERE tenant_id = $1`, sandbox).Scan(&objects, &bytes)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1 AND revoked_at IS NULL`, sandbox).Scan(&keys)
	data["TestData"] = map[string]any{"Objects": objects, "BytesFmt": formatBytes(bytes), "Keys": keys}
}
