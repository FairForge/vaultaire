package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"go.uber.org/zap"
)

// The GDPR export on the dashboard (WP-R10-3b). The page used to render the
// whole export in the request — its own collector, every head row of the
// tenant in a []map, one MarshalIndent (R12-17). It now asks the one export
// service (internal/api.AccountExportService, through ExportService) to
// record the request; a background job renders it within a minute; the
// settings page shows "being prepared" → "ready until <date>, download
// (N MB)" → "expired"; the download link is a redirect to a presigned URL
// minted for this click (never rendered into the page, never logged).

// ExportStatus is the state of the user's latest export as the page shows it.
type ExportStatus struct {
	ID          string
	Status      string // pending | completed | failed | expired
	SizeBytes   int64
	CreatedAt   time.Time
	CompletedAt time.Time
	ExpiresAt   time.Time
	Error       string
	// Expired: the object is gone or due to go (no download).
	Expired bool
}

// ExportService is what the dashboard needs of the export service.
type ExportService interface {
	// Request records an export; ErrExportInFlight when one is pending.
	Request(ctx context.Context, userID, tenantID string) (string, error)
	// Latest is the user's most recent export (nil when none).
	Latest(ctx context.Context, userID string) (*ExportStatus, error)
	// DownloadURL mints a presigned link for a completed export;
	// ErrExportNotReady / ErrExportExpired / ErrExportNotFound otherwise.
	DownloadURL(ctx context.Context, exportID, userID string) (string, time.Time, error)
}

var (
	// ErrExportInFlight: the user already has a pending export.
	ErrExportInFlight = errors.New("an export is already being prepared")
	// ErrExportNotFound: no such export for this user.
	ErrExportNotFound = errors.New("export not found")
	// ErrExportExpired: the export object has been removed.
	ErrExportExpired = errors.New("export expired")
	// ErrExportNotReady: the export has no object yet.
	ErrExportNotReady = errors.New("export not ready")
)

// ExportRequestedMessage is the flash after a request.
const ExportRequestedMessage = "Your export is being prepared. It usually takes about a minute; reload this page to see when it is ready. The file is kept for 7 days and counts against your storage like any object."

// HandleExportData — POST /dashboard/settings/export: records the request
// and comes back to the settings page with a flash.
func HandleExportData(exports ExportService, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if exports == nil {
			middleware.SetFlash(w, "error", "Data export is not available right now.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}
		id, err := exports.Request(r.Context(), sd.UserID, sd.TenantID)
		switch {
		case errors.Is(err, ErrExportInFlight):
			middleware.SetFlash(w, "error", "An export is already being prepared. Reload this page in a minute to download it.")
		case err != nil:
			logger.Error("request data export", zap.Error(err))
			middleware.SetFlash(w, "error", "Failed to request the export. Please try again.")
		default:
			audit.Record(r.Context(), db, audit.Entry{UserID: sd.UserID, TenantID: sd.TenantID, EventType: "account",
				Action: "account.export_requested", Resource: "export:" + id, Metadata: map[string]any{"via": "dashboard"}})
			middleware.SetFlash(w, "success", ExportRequestedMessage)
		}
		http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
	}
}

// HandleExportDownload — GET /dashboard/settings/export/download: redirects
// the signed-in owner to a presigned URL of their latest completed export.
// The URL lives in the Location header of this one response and nowhere
// else.
func HandleExportDownload(exports ExportService, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if exports == nil {
			middleware.SetFlash(w, "error", "Data export is not available right now.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}
		latest, err := exports.Latest(r.Context(), sd.UserID)
		if err != nil {
			logger.Error("latest data export", zap.Error(err))
		}
		if latest == nil || latest.Status != "completed" || latest.Expired {
			middleware.SetFlash(w, "error", "There is no export ready to download. Request one below.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}
		link, _, err := exports.DownloadURL(r.Context(), latest.ID, sd.UserID)
		if err != nil {
			if !errors.Is(err, ErrExportExpired) && !errors.Is(err, ErrExportNotReady) {
				logger.Error("data export download url", zap.Error(err))
			}
			middleware.SetFlash(w, "error", "This export is no longer available. Request a new one below.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}
		// Location only: http.Redirect would also print the URL in an HTML
		// body for a GET.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Location", link)
		w.WriteHeader(http.StatusFound)
	}
}

// exportView is what the settings template renders.
type exportView struct {
	Pending    bool
	Ready      bool
	Failed     bool
	Expired    bool
	Requested  string
	ReadyUntil string
	Size       string
	Error      string
}

// populateExportStatus adds the latest export's state to the page data.
func populateExportStatus(ctx context.Context, exports ExportService, userID string, data map[string]any) {
	data["ExportAvailable"] = exports != nil
	if exports == nil {
		return
	}
	latest, err := exports.Latest(ctx, userID)
	if err != nil || latest == nil {
		return
	}
	v := exportView{Requested: latest.CreatedAt.Format("January 2, 2006 15:04 UTC")}
	switch {
	case latest.Status == "pending":
		v.Pending = true
	case latest.Status == "completed" && !latest.Expired:
		v.Ready = true
		v.ReadyUntil = latest.ExpiresAt.UTC().Format("January 2, 2006 15:04 UTC")
		v.Size = humanMB(latest.SizeBytes)
	case latest.Status == "completed" || latest.Status == "expired":
		v.Expired = true
	case latest.Status == "failed":
		v.Failed = true
		v.Error = latest.Error
	}
	data["Export"] = v
}

func humanMB(n int64) string {
	mb := float64(n) / (1024 * 1024)
	if mb < 0.1 {
		return fmt.Sprintf("%d KB", (n+1023)/1024)
	}
	return fmt.Sprintf("%.1f MB", mb)
}
