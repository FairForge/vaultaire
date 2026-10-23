package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/FairForge/vaultaire/internal/audit"
	"go.uber.org/zap"
)

// handleAdminAuditList serves GET /api/v1/admin/audit — the operator audit
// trail written by internal/audit (Review R11-09). Filters: tenant_id,
// user_id, action, event_type; cursor pagination via `cursor` + `limit`.
func (s *Server) handleAdminAuditList(w http.ResponseWriter, r *http.Request) {
	page, err := audit.List(r.Context(), s.db, adminAuditFilter(r))
	if err != nil {
		if errors.Is(err, audit.ErrBadCursor) {
			writeManagementError(w, ErrTypeInvalidRequest, "invalid_cursor", "cursor is not one this endpoint issued", "cursor")
			return
		}
		s.logger.Error("admin audit list", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "db_error", "failed to list audit rows", "")
		return
	}
	items := make([]interface{}, len(page.Rows))
	for i, row := range page.Rows {
		items[i] = row
	}
	writeListResponse(w, items, page.HasMore, page.NextCursor, len(items))
}

// adminAuditFilter is GET /api/v1/admin/audit's filter — the ONE parser the
// handler and its admin-read classifier (apiAuditScope) both read (RP44-1):
// the tenant logged is the tenant filtered on. The ids are trimmed, as the
// dashboard's audit page trims them (an id pasted with a space is the id).
func adminAuditFilter(r *http.Request) audit.Filter {
	q := r.URL.Query()
	f := audit.Filter{
		TenantID:  strings.TrimSpace(q.Get("tenant_id")),
		UserID:    strings.TrimSpace(q.Get("user_id")),
		Action:    q.Get("action"),
		EventType: q.Get("event_type"),
		Cursor:    q.Get("cursor"),
	}
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			f.Limit = n
		}
	}
	return f
}
