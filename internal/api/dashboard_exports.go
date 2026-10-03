package api

import (
	"context"
	"errors"
	"time"

	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
)

// dashboardExportsAdapter presents the AccountExportService to the dashboard
// as handlers.ExportService (the dashboard package cannot import api).
type dashboardExportsAdapter struct{ svc *AccountExportService }

// dashboardExports wraps the service; a nil service stays a nil interface
// (the dashboard then says export is unavailable).
func dashboardExports(svc *AccountExportService) handlers.ExportService {
	if svc == nil {
		return nil
	}
	return dashboardExportsAdapter{svc: svc}
}

func mapExportErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrExportInFlight):
		return handlers.ErrExportInFlight
	case errors.Is(err, ErrExportNotFound):
		return handlers.ErrExportNotFound
	case errors.Is(err, ErrExportExpired):
		return handlers.ErrExportExpired
	case errors.Is(err, ErrExportNotReady):
		return handlers.ErrExportNotReady
	}
	return err
}

func (a dashboardExportsAdapter) Request(ctx context.Context, userID, tenantID string) (string, error) {
	id, err := a.svc.Request(ctx, userID, tenantID)
	return id, mapExportErr(err)
}

func (a dashboardExportsAdapter) Latest(ctx context.Context, userID string) (*handlers.ExportStatus, error) {
	st, err := a.svc.Latest(ctx, userID)
	if err != nil || st == nil {
		return nil, mapExportErr(err)
	}
	return &handlers.ExportStatus{ID: st.ID, Status: st.Status, SizeBytes: st.SizeBytes, CreatedAt: st.CreatedAt,
		CompletedAt: st.CompletedAt, ExpiresAt: st.ExpiresAt, Error: st.Error, Expired: st.Expired}, nil
}

func (a dashboardExportsAdapter) DownloadURL(ctx context.Context, exportID, userID string) (string, time.Time, error) {
	link, expires, err := a.svc.DownloadURL(ctx, exportID, userID)
	return link, expires, mapExportErr(err)
}
