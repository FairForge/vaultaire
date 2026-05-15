package handlers

import (
	"context"
	"database/sql"
	"html/template"
	"net/http"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// TopObject is a single row in the "top objects by downloads" table.
type TopObject struct {
	Key       string
	Downloads int64
	Bandwidth string
}

// CountryRow is a single row in the geographic breakdown table.
type CountryRow struct {
	Code      string
	Requests  int64
	Bandwidth string
}

// HandleBucketAnalytics renders the CDN analytics page for a bucket.
func HandleBucketAnalytics(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		bucketName := chi.URLParam(r, "name")
		if bucketName == "" {
			http.Redirect(w, r, "/dashboard/buckets", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "buckets")
		data["BucketName"] = bucketName

		if db != nil {
			ctx := r.Context()
			populateAnalyticsStats(ctx, db, sd.TenantID, bucketName, data)
			populateTopObjects(ctx, db, sd.TenantID, bucketName, data)
			populateBudget(ctx, db, sd.TenantID, bucketName, data)
			populateCountries(ctx, db, sd.TenantID, bucketName, data)
		}

		_, hasDownloads := data["Downloads24h"]
		if !hasDownloads {
			setAnalyticsDefaults(data)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render bucket analytics", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func populateAnalyticsStats(ctx context.Context, db *sql.DB, tenantID, bucket string, data map[string]any) {
	var dl24h, dl7d, dl30d, bw24h, bw7d, bw30d int64

	_ = db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(bytes_sent), 0)
		FROM cdn_stats_daily
		WHERE tenant_id = $1 AND bucket = $2 AND date >= CURRENT_DATE - INTERVAL '1 day'`,
		tenantID, bucket).Scan(&dl24h, &bw24h)

	_ = db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(bytes_sent), 0)
		FROM cdn_stats_daily
		WHERE tenant_id = $1 AND bucket = $2 AND date >= CURRENT_DATE - INTERVAL '7 days'`,
		tenantID, bucket).Scan(&dl7d, &bw7d)

	_ = db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(bytes_sent), 0)
		FROM cdn_stats_daily
		WHERE tenant_id = $1 AND bucket = $2 AND date >= CURRENT_DATE - INTERVAL '30 days'`,
		tenantID, bucket).Scan(&dl30d, &bw30d)

	data["Downloads24h"] = dl24h
	data["Downloads7d"] = dl7d
	data["Downloads30d"] = dl30d
	data["Bandwidth24h"] = formatBytes(bw24h)
	data["Bandwidth7d"] = formatBytes(bw7d)
	data["Bandwidth30d"] = formatBytes(bw30d)
	data["HasData"] = dl30d > 0
}

func populateTopObjects(ctx context.Context, db *sql.DB, tenantID, bucket string, data map[string]any) {
	rows, err := db.QueryContext(ctx, `
		SELECT object_key, COUNT(*) AS downloads, SUM(bytes_sent) AS bandwidth
		FROM cdn_access_log
		WHERE tenant_id = $1 AND bucket = $2
		  AND accessed_at >= NOW() - INTERVAL '30 days'
		GROUP BY object_key
		ORDER BY COUNT(*) DESC
		LIMIT 10`, tenantID, bucket)
	if err != nil {
		data["TopObjects"] = nil
		return
	}
	defer func() { _ = rows.Close() }()

	var objects []TopObject
	for rows.Next() {
		var o TopObject
		var bw int64
		if err := rows.Scan(&o.Key, &o.Downloads, &bw); err != nil {
			continue
		}
		o.Bandwidth = formatBytes(bw)
		objects = append(objects, o)
	}
	data["TopObjects"] = objects
}

func populateBudget(ctx context.Context, db *sql.DB, tenantID, bucket string, data map[string]any) {
	var budgetBytes int64
	err := db.QueryRowContext(ctx,
		`SELECT bandwidth_budget_bytes FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, bucket).Scan(&budgetBytes)
	if err != nil || budgetBytes <= 0 {
		data["HasBudget"] = false
		data["BudgetUsed"] = "0 B"
		data["BudgetLimit"] = "0 B"
		data["BudgetPct"] = 0
		return
	}

	var usedBytes int64
	_ = db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(bytes_sent), 0) FROM cdn_stats_daily
		WHERE tenant_id = $1 AND bucket = $2
		  AND date >= date_trunc('month', CURRENT_DATE)`,
		tenantID, bucket).Scan(&usedBytes)

	pct := 0
	if budgetBytes > 0 {
		pct = int(usedBytes * 100 / budgetBytes)
		if pct > 100 {
			pct = 100
		}
	}

	data["HasBudget"] = true
	data["BudgetUsed"] = formatBytes(usedBytes)
	data["BudgetLimit"] = formatBytes(budgetBytes)
	data["BudgetPct"] = pct
}

func populateCountries(ctx context.Context, db *sql.DB, tenantID, bucket string, data map[string]any) {
	rows, err := db.QueryContext(ctx, `
		SELECT country, COUNT(*) AS requests, SUM(bytes_sent) AS bandwidth
		FROM cdn_access_log
		WHERE tenant_id = $1 AND bucket = $2
		  AND accessed_at >= NOW() - INTERVAL '30 days'
		GROUP BY country
		ORDER BY COUNT(*) DESC
		LIMIT 20`, tenantID, bucket)
	if err != nil {
		data["Countries"] = nil
		return
	}
	defer func() { _ = rows.Close() }()

	var countries []CountryRow
	for rows.Next() {
		var c CountryRow
		var bw int64
		if err := rows.Scan(&c.Code, &c.Requests, &bw); err != nil {
			continue
		}
		if c.Code == "" {
			c.Code = "Unknown"
		}
		c.Bandwidth = formatBytes(bw)
		countries = append(countries, c)
	}
	data["Countries"] = countries
}

func setAnalyticsDefaults(data map[string]any) {
	data["Downloads24h"] = int64(0)
	data["Downloads7d"] = int64(0)
	data["Downloads30d"] = int64(0)
	data["Bandwidth24h"] = "0 B"
	data["Bandwidth7d"] = "0 B"
	data["Bandwidth30d"] = "0 B"
	data["HasData"] = false
	data["TopObjects"] = nil
	data["HasBudget"] = false
	data["BudgetUsed"] = "0 B"
	data["BudgetLimit"] = "0 B"
	data["BudgetPct"] = 0
	data["Countries"] = nil
}
