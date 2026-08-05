package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/FairForge/vaultaire/internal/common"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// dashboardRestoreDays: restored copies stay readable this long on the
// staging disk. Matches the V18.2 plan default (Days=2) — long enough to
// download, short enough that staging doesn't fill with thawed data.
const dashboardRestoreDays = 2

// objectRestorerFor resolves the archive backend holding an object, or nil
// when the object is on hot storage (mirrors api.objectRestorer).
func objectRestorerFor(ctx context.Context, eng *engine.CoreEngine, db *sql.DB, tenantID, bucket, key string) (engine.Restorer, error) {
	var backendName string
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(backend_name, '') FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key).Scan(&backendName)
	if err != nil {
		return nil, err
	}
	if eng == nil {
		return nil, nil
	}
	drv, ok := eng.GetDriver(backendName)
	if !ok {
		return nil, nil
	}
	restorer, _ := drv.(engine.Restorer)
	return restorer, nil
}

// archiveContainer builds the engine container name for a tenant's bucket
// (the tenant.Tenant.NamespaceContainer format) without needing a full
// tenant object in the dashboard session.
func archiveContainer(tenantID, bucket string) string {
	return fmt.Sprintf("%s_%s", tenantID, bucket)
}

// HandleRestoreObject handles POST /dashboard/buckets/{name}/restore — the
// V18.2 dashboard Restore button. Requests a tape recall and redirects back
// to the object browser with a flash.
func HandleRestoreObject(eng *engine.CoreEngine, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		bucket := chi.URLParam(r, "name")
		key := r.FormValue("key")
		backURL := "/dashboard/buckets/" + url.PathEscape(bucket)
		if prefix := r.FormValue("prefix"); prefix != "" {
			backURL += "?prefix=" + url.QueryEscape(prefix)
		}
		if key == "" || db == nil {
			middleware.SetFlash(w, "error", "Missing object key.")
			http.Redirect(w, r, backURL, http.StatusSeeOther)
			return
		}

		restorer, err := objectRestorerFor(r.Context(), eng, db, sd.TenantID, bucket, key)
		if err != nil || restorer == nil {
			middleware.SetFlash(w, "error", "This object is not on the archive tier — it is directly downloadable.")
			http.Redirect(w, r, backURL, http.StatusSeeOther)
			return
		}

		ctx := context.WithValue(r.Context(), common.TenantIDKey, sd.TenantID)
		if restoreErr := restorer.RestoreObject(ctx, archiveContainer(sd.TenantID, bucket), key, dashboardRestoreDays); restoreErr != nil {
			if errors.Is(restoreErr, engine.ErrRestoreAlreadyInProgress) {
				middleware.SetFlash(w, "success", "A restore for this object is already running — check back in a few minutes.")
			} else {
				logger.Error("dashboard restore failed",
					zap.Error(restoreErr), zap.String("bucket", bucket), zap.String("key", key))
				middleware.SetFlash(w, "error", "Restore request failed — please retry or contact support.")
			}
			http.Redirect(w, r, backURL, http.StatusSeeOther)
			return
		}

		middleware.SetFlash(w, "success",
			"Restore started for "+key+" — restores typically begin within minutes. The object stays downloadable for "+
				fmt.Sprintf("%d", dashboardRestoreDays)+" days once ready.")
		http.Redirect(w, r, backURL, http.StatusSeeOther)
	}
}

// HandleObjectRestoreStatus handles GET /dashboard/buckets/{name}/restore-status?key=
// — an htmx fragment showing an archived object's live recall state.
func HandleObjectRestoreStatus(eng *engine.CoreEngine, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		bucket := chi.URLParam(r, "name")
		key := r.URL.Query().Get("key")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if key == "" || db == nil {
			_, _ = w.Write([]byte(`<span class="text-muted">on tape</span>`))
			return
		}

		restorer, err := objectRestorerFor(r.Context(), eng, db, sd.TenantID, bucket, key)
		if err != nil || restorer == nil {
			_, _ = w.Write([]byte(`<span class="text-muted">on tape</span>`))
			return
		}

		ctx := context.WithValue(r.Context(), common.TenantIDKey, sd.TenantID)
		st, stErr := restorer.RestoreStatus(ctx, archiveContainer(sd.TenantID, bucket), key)
		if stErr != nil {
			logger.Debug("dashboard restore status failed", zap.Error(stErr))
			_, _ = w.Write([]byte(`<span class="text-muted">on tape</span>`))
			return
		}
		switch {
		case strings.Contains(st.Restore, `ongoing-request="true"`):
			_, _ = w.Write([]byte(`<span title="Tape recall running — typically minutes">restoring&hellip;</span>`))
		case strings.Contains(st.Restore, `ongoing-request="false"`):
			_, _ = w.Write([]byte(`<span title="Restored copy is readable until it re-freezes">restored &#10003;</span>`))
		default:
			_, _ = w.Write([]byte(`<span class="text-muted" title="Request a restore to read this object">on tape</span>`))
		}
	}
}
