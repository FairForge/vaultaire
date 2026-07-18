package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"

	"github.com/FairForge/vaultaire/internal/crypto"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

type tenantDedupRow struct {
	Email       string
	TenantID    string
	LogicalFmt  string
	PhysicalFmt string
	SavedFmt    string
	SavedPct    string
	DedupRatio  string
}

func HandleAdminDedup(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "admin-dedup")
		withCSRF(r.Context(), data)

		data["DedupRatio"] = "1.0x"
		data["StorageSaved"] = "0 B"
		data["LogicalBytes"] = "0 B"
		data["PhysicalBytes"] = "0 B"
		data["ChunksProcessed"] = int64(0)
		data["TenantTable"] = []tenantDedupRow{}
		data["CompressionRatio"] = "1.0x"
		data["CompressionSaved"] = "0 B"
		data["ChunksCompressed"] = int64(0)
		data["ChunksUncompressed"] = int64(0)

		if db != nil {
			populateDedupStats(r.Context(), db, data, logger)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin dedup", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func populateDedupStats(ctx context.Context, db *sql.DB, data map[string]any, logger *zap.Logger) {
	gci := crypto.NewGlobalContentIndex(db)

	stats, err := gci.GetGlobalDedupStats(ctx)
	if err != nil {
		logger.Debug("dedup: global stats", zap.Error(err))
		return
	}

	data["DedupRatio"] = fmt.Sprintf("%.1fx", stats.DedupRatio)
	data["StorageSaved"] = formatBytes(stats.BytesSaved)
	data["LogicalBytes"] = formatBytes(stats.BytesLogical)
	data["PhysicalBytes"] = formatBytes(stats.BytesPhysical)
	data["ChunksProcessed"] = stats.ChunksProcessed

	cstats, cerr := gci.GetGlobalCompressionStats(ctx)
	if cerr != nil {
		logger.Debug("dedup: compression stats", zap.Error(cerr))
	} else {
		data["CompressionRatio"] = fmt.Sprintf("%.1fx", cstats.CompressionRatio)
		data["CompressionSaved"] = formatBytes(cstats.BytesSaved)
		data["ChunksCompressed"] = cstats.ChunksCompressed
		data["ChunksUncompressed"] = cstats.ChunksUncompressed
	}

	populateTenantDedup(ctx, db, gci, data, logger)
}

func populateTenantDedup(ctx context.Context, db *sql.DB, gci *crypto.GlobalContentIndex, data map[string]any, logger *zap.Logger) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT t.id, t.email
		FROM tenants t
		INNER JOIN object_metadata om ON om.tenant_id = t.id
		ORDER BY t.email`)
	if err != nil {
		logger.Debug("dedup: query tenants with chunks", zap.Error(err))
		return
	}
	defer func() { _ = rows.Close() }()

	var table []tenantDedupRow
	for rows.Next() {
		var tidStr, email string
		if err := rows.Scan(&tidStr, &email); err != nil {
			logger.Debug("dedup: scan tenant", zap.Error(err))
			continue
		}
		ts, err := gci.GetTenantDedupStats(ctx, tidStr)
		if err != nil {
			logger.Debug("dedup: tenant stats", zap.String("tenant", tidStr), zap.Error(err))
			continue
		}
		savedPct := "0%"
		if ts.BytesLogical > 0 {
			savedPct = fmt.Sprintf("%.0f%%", float64(ts.BytesSaved)/float64(ts.BytesLogical)*100)
		}
		table = append(table, tenantDedupRow{
			Email:       email,
			TenantID:    tidStr,
			LogicalFmt:  formatBytes(ts.BytesLogical),
			PhysicalFmt: formatBytes(ts.BytesPhysical),
			SavedFmt:    formatBytes(ts.BytesSaved),
			SavedPct:    savedPct,
			DedupRatio:  fmt.Sprintf("%.1fx", ts.DedupRatio),
		})
	}

	if len(table) > 0 {
		data["TenantTable"] = table
	}
}
