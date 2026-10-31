package handlers

// A scoped key's caps on the API keys page (WP-KEY-1 PR C; F-39): its
// storage cap (the bytes its writes still hold) and its egress cap per UTC
// month, in GiB, set by whoever may write keys (key.write). The primary
// pair is never capped (F-39 (c)). The rule is internal/usage's
// (SetKeyCaps); the page only reads and sets it.

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/usage"
)

// capBytes words a cap in binary units ("512 MiB", "10 GiB").
func capBytes(b int64) string {
	switch {
	case b >= 1<<40 && b%(1<<30) == 0:
		return fmt.Sprintf("%s TiB", strconv.FormatFloat(float64(b)/(1<<40), 'f', -1, 64))
	case b >= 1<<30:
		return fmt.Sprintf("%s GiB", strconv.FormatFloat(math.Round(float64(b)/(1<<30)*100)/100, 'f', -1, 64))
	case b >= 1<<20:
		return fmt.Sprintf("%s MiB", strconv.FormatFloat(math.Round(float64(b)/(1<<20)*100)/100, 'f', -1, 64))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// gibValue is a cap as the form's GiB field shows it ("" = none).
func gibValue(b *int64) string {
	if b == nil {
		return ""
	}
	return strconv.FormatFloat(float64(*b)/(1<<30), 'f', -1, 64)
}

// withKeyCaps adds each key's caps to the page's key rows.
func withKeyCaps(r *http.Request, db *sql.DB, tenantID string, data map[string]any) {
	keys, _ := data["Keys"].([]KeyRow)
	if db == nil || len(keys) == 0 {
		return
	}
	caps, err := usage.ListKeyCaps(r.Context(), db, tenantID)
	if err != nil {
		return
	}
	for i := range keys {
		c, ok := caps[keys[i].ID]
		if !ok {
			continue
		}
		keys[i].StorageGiB, keys[i].EgressGiB = gibValue(c.StorageLimit), gibValue(c.EgressLimit)
		var parts []string
		if c.StorageLimit != nil {
			parts = append(parts, capBytes(*c.StorageLimit)+" stored")
		}
		if c.EgressLimit != nil {
			parts = append(parts, capBytes(*c.EgressLimit)+"/month out")
		}
		keys[i].CapsSummary = "none"
		if len(parts) > 0 {
			keys[i].CapsSummary = strings.Join(parts, " · ")
		}
	}
	data["Keys"] = keys
}

// parseGiB reads a GiB form field ("" = none).
func parseGiB(v string) (*int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) || f > 1<<23 {
		return nil, errors.New("a cap is a number of GiB, 0 or more")
	}
	n := int64(math.Round(f * (1 << 30)))
	return &n, nil
}

// HandleSetKeyCaps handles POST /dashboard/apikeys/{id}/caps.
func HandleSetKeyCaps(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		defer http.Redirect(w, r, "/dashboard/apikeys", http.StatusSeeOther)
		if db == nil || r.ParseForm() != nil {
			middleware.SetFlash(w, "error", "The caps could not be read.")
			return
		}
		storage, sErr := parseGiB(r.PostFormValue("storage_gib"))
		egress, eErr := parseGiB(r.PostFormValue("egress_gib"))
		if sErr != nil || eErr != nil {
			middleware.SetFlash(w, "error", "A cap is a number of GiB, 0 or more (empty for none).")
			return
		}
		id := chi.URLParam(r, "id")
		res, err := usage.NewQuotaManager(db).SetKeyCaps(r.Context(), sd.TenantID, id, usage.Caps{StorageLimit: storage, EgressLimit: egress}, 0)
		switch {
		case errors.Is(err, usage.ErrPrimaryNeverCapped):
			middleware.SetFlash(w, "error", "The primary key pair is never capped. Cap a scoped key instead.")
			return
		case errors.Is(err, usage.ErrNoSuchHolder):
			middleware.SetFlash(w, "error", "That key is not live.")
			return
		case err != nil:
			logger.Warn("set a key's caps", zap.Error(err), zap.String("id", id))
			middleware.SetFlash(w, "error", "The caps could not be set. Try again.")
			return
		}
		audit.Record(r.Context(), db, audit.Entry{TenantID: sd.TenantID, Actor: sd.UserID, Action: "key.caps_set", Resource: "key:" + id,
			Metadata: map[string]any{"storage_limit_bytes": storage, "egress_limit_bytes": egress, "notes": res.Notes, "via": "dashboard"}})
		msg := "Caps saved."
		for _, n := range res.Notes {
			if n == usage.NoteStorageAboveTenant {
				msg += " The storage cap is above your account's: the account's smaller number applies."
			}
		}
		middleware.SetFlash(w, "success", msg)
	}
}
