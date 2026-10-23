package handlers

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/translog/adminread"
)

// "Access to your account" (Stage 1a Prompt 34b; docs/ARCHITECTURE_CONTRACT.md
// rule 7: "customers read outcomes; auditors read the log"). The page lists
// the tenant's own transparency-log entries — every operator read of its
// records and every change to how such reads are recorded for it — found by
// the same adminread.Find as GET /api/v1/manage/account/transparency, and
// says each one in words from adminread.Words (one table, no free text).
// Dark behind translog_customer_view (OFF = 404).

// AccessLog finds a tenant's own entries (adminread.Store).
type AccessLog interface {
	Find(ctx context.Context, tenantID string) ([]adminread.Found, error)
}

// AccessRow is one entry on the page.
type AccessRow struct {
	Index int64
	At    time.Time
	What  string
	// Checkpoint is the size of the first signed checkpoint whose tree
	// includes the entry; 0 = not signed yet.
	Checkpoint int64
}

// HandleAccess serves GET /dashboard/settings/access. enabled is the
// translog_customer_view flag for a tenant (nil = off); recording is
// translog_admin_reads for it (the page says so when reads are not being
// recorded — never "every read is in the log" while they are not); log the
// finder (nil = the log is not on this deployment); checkpointURL the public
// checkpoint (the trust host's /translog/checkpoint); published whether the
// log is served there (translog_public ON globally; nil = not published).
// Unpublished, the page says the entries are in our transparency log, not
// yet published, and links nowhere (RP46-3, contract rule 8).
func HandleAccess(tmpl *template.Template, enabled, recording func(tenantID string) bool, log AccessLog, checkpointURL string, published func() bool, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if sd.TenantID == "" || enabled == nil || !enabled(sd.TenantID) {
			w.Header().Set("Cache-Control", "no-store")
			http.NotFound(w, r)
			return
		}
		data := sessionData(sd, "settings")
		withCSRF(r.Context(), data)
		pub := published != nil && checkpointURL != "" && published()
		data["Published"] = pub
		data["Where"] = adminread.Where(pub)
		if pub {
			data["CheckpointURL"] = checkpointURL
			data["VerifyCommand"] = fmt.Sprintf(adminread.Where(true).Verify, strings.TrimSuffix(checkpointURL, "/checkpoint"))
		}
		data["Recording"] = recording != nil && recording(sd.TenantID)
		rows := []AccessRow{}
		if log == nil {
			data["Unavailable"] = true
		} else {
			found, err := log.Find(r.Context(), sd.TenantID)
			if err != nil {
				logger.Error("access to your account: entries not read", zap.Error(err))
				data["Unavailable"] = true
			}
			for _, f := range found {
				row := AccessRow{Index: f.Index, At: f.At.UTC(), What: adminread.Words(f)}
				if f.CheckpointSize != nil {
					row.Checkpoint = *f.CheckpointSize
				}
				rows = append(rows, row)
			}
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Index > rows[j].Index }) // newest first
		data["AccessRows"] = rows
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			logger.Error("render access to your account", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}
