package handlers

import (
	"context"
	"database/sql"
	"html/template"
	"net/http"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

// Admin traffic page: the cookieless statistics of the public site
// (internal/sitestats → site_stats_daily) next to what they lead to —
// waitlist signups and accounts — so a post, a referrer or a campaign can be
// read as a funnel. Everything on it is a daily total; nothing names a
// visitor.

// trafficDay is one day of the 30-day table.
type trafficDay struct {
	Day      string
	Views    int64 // rendered public pages
	Landing  int64 // of which "/"
	Uniques  int64 // distinct visitors that day (daily-salted hashes)
	Events   int64 // beacon events
	Signups  int64 // waitlist rows created
	Accounts int64 // tenants created
}

// trafficRow is one line of a top-N table.
type trafficRow struct {
	Name string
	N    int64
}

// trafficTop is one top-N table of the window.
type trafficTop struct {
	Title string
	Rows  []trafficRow
}

// trafficFunnel is the all-time funnel from the waitlist to a paying account.
type trafficFunnel struct {
	Waitlist   int64
	Accounts   int64
	WithBucket int64
	WithObject int64
	Paying     int64
}

const trafficWindowDays = 30

// HandleAdminStats renders GET /admin/stats.
func HandleAdminStats(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		data := sessionData(sd, "admin-stats")
		withCSRF(r.Context(), data)
		data["WindowDays"] = trafficWindowDays
		data["Days"] = []trafficDay{}
		data["Funnel"] = trafficFunnel{}
		data["Tops"] = []trafficTop{}
		if db != nil {
			populateTraffic(r.Context(), db, logger, data)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin stats", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// trafficTopQueries are the top-N cuts of the window in display order, one
// static literal each (the schema audit prepares every literal against the
// migrated database).
var trafficTopQueries = []struct{ title, query string }{
	{"Pages", `SELECT name, SUM(n) FROM site_stats_daily WHERE kind = 'view' AND day >= $1 GROUP BY name ORDER BY 2 DESC, 1 LIMIT 12`},
	{"Referrers", `SELECT referrer, SUM(n) FROM site_stats_daily WHERE kind = 'view' AND day >= $1 AND referrer <> '' GROUP BY referrer ORDER BY 2 DESC, 1 LIMIT 12`},
	{"Countries", `SELECT country, SUM(n) FROM site_stats_daily WHERE kind = 'view' AND day >= $1 AND country <> '' GROUP BY country ORDER BY 2 DESC, 1 LIMIT 12`},
	{"Devices", `SELECT device, SUM(n) FROM site_stats_daily WHERE kind = 'view' AND day >= $1 AND device <> '' GROUP BY device ORDER BY 2 DESC, 1 LIMIT 12`},
	{"Sources (utm_source)", `SELECT utm_source, SUM(n) FROM site_stats_daily WHERE kind = 'view' AND day >= $1 AND utm_source <> '' GROUP BY utm_source ORDER BY 2 DESC, 1 LIMIT 12`},
	{"Campaigns (utm_campaign)", `SELECT utm_campaign, SUM(n) FROM site_stats_daily WHERE kind = 'view' AND day >= $1 AND utm_campaign <> '' GROUP BY utm_campaign ORDER BY 2 DESC, 1 LIMIT 12`},
	{"Events", `SELECT name, SUM(n) FROM site_stats_daily WHERE kind = 'event' AND day >= $1 GROUP BY name ORDER BY 2 DESC, 1 LIMIT 30`},
}

func populateTraffic(ctx context.Context, db *sql.DB, logger *zap.Logger, data map[string]any) {
	since := time.Now().UTC().AddDate(0, 0, -(trafficWindowDays - 1)).Format("2006-01-02")

	days := map[string]*trafficDay{}
	order := []string{}
	day := func(d string) *trafficDay {
		if t, ok := days[d]; ok {
			return t
		}
		t := &trafficDay{Day: d}
		days[d] = t
		order = append(order, d)
		return t
	}
	rows, err := db.QueryContext(ctx, `
		SELECT day,
		       COALESCE(SUM(n) FILTER (WHERE kind = 'view'), 0),
		       COALESCE(SUM(n) FILTER (WHERE kind = 'view' AND name = '/'), 0),
		       COALESCE(MAX(n) FILTER (WHERE kind = 'uniques'), 0),
		       COALESCE(SUM(n) FILTER (WHERE kind = 'event'), 0)
		FROM site_stats_daily WHERE day >= $1 GROUP BY day ORDER BY day DESC`, since)
	if err != nil {
		logger.Error("traffic days query", zap.Error(err))
		return
	}
	for rows.Next() {
		var d time.Time
		var views, landing, uniques, events int64
		if err := rows.Scan(&d, &views, &landing, &uniques, &events); err != nil {
			logger.Error("traffic days scan", zap.Error(err))
			break
		}
		t := day(d.Format("2006-01-02"))
		t.Views, t.Landing, t.Uniques, t.Events = views, landing, uniques, events
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		logger.Error("traffic days rows", zap.Error(err))
	}
	perDay := func(query string, set func(t *trafficDay, n int64)) {
		rows, err := db.QueryContext(ctx, query, since)
		if err != nil {
			logger.Error("traffic per-day query", zap.Error(err))
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var d time.Time
			var n int64
			if err := rows.Scan(&d, &n); err != nil {
				logger.Error("traffic per-day scan", zap.Error(err))
				return
			}
			set(day(d.Format("2006-01-02")), n)
		}
		if err := rows.Err(); err != nil {
			logger.Error("traffic per-day rows", zap.Error(err))
		}
	}
	perDay(`SELECT created_at::date, COUNT(*) FROM waitlist_signups WHERE created_at >= $1::date GROUP BY 1`,
		func(t *trafficDay, n int64) { t.Signups = n })
	perDay(`SELECT created_at::date, COUNT(*) FROM tenants WHERE created_at >= $1::date GROUP BY 1`,
		func(t *trafficDay, n int64) { t.Accounts = n })

	// newest first, whatever order the three queries filled the map in
	list := make([]trafficDay, 0, len(order))
	for _, d := range order {
		list = append(list, *days[d])
	}
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Day > list[j-1].Day; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	var views, uniques, landing, events, signups, accounts int64
	for _, t := range list {
		views += t.Views
		uniques += t.Uniques
		landing += t.Landing
		events += t.Events
		signups += t.Signups
		accounts += t.Accounts
	}
	data["Days"] = list
	data["Views"], data["Uniques"], data["Landing"], data["Events"], data["Signups"], data["Accounts"] = views, uniques, landing, events, signups, accounts

	top := func(query string) []trafficRow {
		rows, err := db.QueryContext(ctx, query, since)
		if err != nil {
			logger.Error("traffic top query", zap.Error(err))
			return nil
		}
		defer func() { _ = rows.Close() }()
		var out []trafficRow
		for rows.Next() {
			var r trafficRow
			if err := rows.Scan(&r.Name, &r.N); err != nil {
				logger.Error("traffic top scan", zap.Error(err))
				return out
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			logger.Error("traffic top rows", zap.Error(err))
		}
		return out
	}
	tops := make([]trafficTop, 0, len(trafficTopQueries)+1)
	for _, tq := range trafficTopQueries {
		tops = append(tops, trafficTop{Title: tq.title, Rows: top(tq.query)})
	}
	// where the waitlist itself is from (all time; the country outlives the address)
	var byCountry []trafficRow
	if rows, err := db.QueryContext(ctx, `SELECT country, COUNT(*) FROM waitlist_signups WHERE country <> '' GROUP BY country ORDER BY 2 DESC, 1 LIMIT 12`); err != nil {
		logger.Error("traffic signup countries", zap.Error(err))
	} else {
		for rows.Next() {
			var r trafficRow
			if err := rows.Scan(&r.Name, &r.N); err != nil {
				break
			}
			byCountry = append(byCountry, r)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			logger.Error("traffic signup countries rows", zap.Error(err))
		}
	}
	tops = append(tops, trafficTop{Title: "Waitlist by country (all time)", Rows: byCountry})
	data["Tops"] = tops

	var f trafficFunnel
	if err := db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM waitlist_signups),
		       (SELECT COUNT(*) FROM tenants),
		       (SELECT COUNT(DISTINCT tenant_id) FROM buckets),
		       (SELECT COUNT(DISTINCT tenant_id) FROM object_head_cache),
		       (SELECT COUNT(*) FROM tenants WHERE subscription_status = 'active')`).
		Scan(&f.Waitlist, &f.Accounts, &f.WithBucket, &f.WithObject, &f.Paying); err != nil {
		logger.Error("traffic funnel", zap.Error(err))
	}
	data["Funnel"] = f
}
