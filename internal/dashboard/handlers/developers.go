package handlers

import (
	"database/sql"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/requestlog"
	"go.uber.org/zap"
)

// HandleDevelopers renders the Developers page (WP-LOG-1, Stage 1a Prompt
// 49): the tenant's S3 requests of the last 7 days from the request log,
// newest first, the error code's suggestion inline, filtered by key and
// status and searchable by request id (the id a support ticket quotes),
// paged by the reader's keyset cursor. ?mode=test shows the requests of the
// tenant's test keys (its sandbox's rows, RB8-2).
func HandleDevelopers(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		data := sessionData(sd, "developers")
		withCSRF(r.Context(), data)
		v := r.URL.Query()
		filter := struct{ Key, Status, RequestID string }{
			Key: strings.TrimSpace(v.Get("key")), Status: strings.TrimSpace(v.Get("status")), RequestID: strings.TrimSpace(v.Get("request_id")),
		}
		data["Filter"] = filter
		// Live / Test (RB8-2): a test key's requests are the sandbox's rows
		// (tenants.sandbox_of); test mode reads them.
		mode := "live"
		logTenant := sd.TenantID
		if v.Get("mode") == "test" {
			mode, logTenant = "test", ""
			if db != nil {
				if err := db.QueryRowContext(r.Context(), `SELECT id FROM tenants WHERE sandbox_of = $1`, sd.TenantID).Scan(&logTenant); err != nil && !errors.Is(err, sql.ErrNoRows) {
					logger.Error("developers page: sandbox lookup failed", zap.Error(err))
					data["FlashError"] = "The request log could not be read. Try again in a minute."
				}
			}
		}
		data["Mode"] = mode
		q := requestlog.Query{TenantID: logTenant, KeyID: filter.Key, RequestID: filter.RequestID,
			Since: time.Now().Add(-7 * 24 * time.Hour), Limit: 50}
		var err error
		if q.StatusMin, q.StatusMax, err = requestlog.ParseStatus(filter.Status); err != nil {
			data["FlashError"] = err.Error()
		}
		if q.After, err = requestlog.ParseCursor(v.Get("cursor")); err != nil {
			data["FlashError"] = err.Error()
		}
		if db != nil && data["FlashError"] == nil && logTenant != "" {
			rows, next, rErr := requestlog.Read(r.Context(), db, q)
			if rErr != nil {
				logger.Error("developers page: request log read failed", zap.Error(rErr))
				data["FlashError"] = "The request log could not be read. Try again in a minute."
			}
			data["Requests"] = rows
			if next != "" {
				older := url.Values{"cursor": {next}}
				if mode == "test" {
					older.Set("mode", "test")
				}
				for k, val := range map[string]string{"key": filter.Key, "status": filter.Status, "request_id": filter.RequestID} {
					if val != "" {
						older.Set(k, val)
					}
				}
				data["OlderURL"] = "/dashboard/developers?" + older.Encode()
			}
			data["Keys"] = developerKeys(r, db, logTenant, logger)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render developers page", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// developerKey is one access key in the page's key filter.
type developerKey struct{ ID, Name string }

// developerKeys are the tenant's access keys (ids and names, never a
// secret), the newest first.
func developerKeys(r *http.Request, db *sql.DB, tenantID string, logger *zap.Logger) []developerKey {
	rows, err := db.QueryContext(r.Context(), `SELECT key_id, COALESCE(name, '') FROM api_keys WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 100`, tenantID)
	if err != nil {
		logger.Warn("developers page: keys not read", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []developerKey
	for rows.Next() {
		var k developerKey
		if rows.Scan(&k.ID, &k.Name) == nil {
			out = append(out, k)
		}
	}
	if err := rows.Err(); err != nil {
		logger.Warn("developers page: keys not read", zap.Error(err))
	}
	return out
}
