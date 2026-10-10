package handlers

import (
	"html/template"
	"net/http"
	"sort"
	"strconv"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// Admin feature-flags page (1.13 live-iteration kit): a table of every
// registered flag (default, global state, per-tenant overrides) with toggle
// buttons and a tenant-override form. Mutations go through the flag
// service's write-through Set/Unset, so a toggle is live on the next
// request — no deploy, no restart.

// flagInfo is what the flags page says about each registered flag: the
// runbook's risk tier (1.13) and what flipping it does. Tier 1 = a feature
// that ships dark and is opened per tenant, then globally; Tier 2 =
// billing, auth or the data path — never flipped at chat speed, always
// after a low-traffic window and a plan to flip back.
type flagInfo struct {
	Tier  int
	About string
}

var flagInfos = map[string]flagInfo{
	"signups":         {1, "Public account creation (web form, /auth/register, OAuth signup). Off = existing users keep signing in, nobody new gets in."},
	"quota_checkout":  {2, "Whole-TB house checkout on the billing page (Phase 1). Needs the six STRIPE_PRICE_* env vars verified at boot; the webhook applies bought houses whether or not this is on."},
	"house_overview":  {1, "Draws the customer's house on the dashboard overview (Phase 2) in place of the storage gauge."},
	"chunking":        {2, "Content-defined chunking + dedup on the PUT path. Off = plain whole-object PUTs; reads keep working either way."},
	"smart_demotion":  {2, "The Smart-tier demotion job: idle downstairs objects move to tape behind the scenes and come back hot on read."},
	"vault_parity":    {2, "The Vault parity second copy: the vault_parity job writes RS 4+4 parity shards of every vault-floor object to the parity leg (sync, else permafrost, else lyve — VAULT_PARITY_LEGS) and a read whose tape backend fails is rebuilt from them. Off = no shards are written and none are read."},
	"sync_backend":    {2, "Lets this tenant's buckets with tier_preference 'sync' (operator-set, SQL only) store on the Sync.com WebDAV bridge. Per tenant only, never a global row: Sync's terms forbid reselling the service without its written consent — our own data, or a customer Sync agreed to in writing."},
	"parallel_get":    {2, "Large whole-object downloads (64 MiB and up) on iDrive/Wasabi are read as 8 parallel 16 MiB ranges with hedging behind the one client stream (2–4× a single backend connection). Off = one backend GET per client GET. Per tenant first, then global."},
	"egress_throttle": {2, "The egress allowance as a rate cap: a tenant past its monthly allowance has GetObject and /cdn bodies paced (never billed). Off = nothing is slowed, the decision is only counted (vaultaire_egress_would_throttle_total). With the global row on, a tenant row with the flag off exempts that tenant."},
}

// flagOrder puts the launch levers first.
var flagOrder = []string{"signups", "quota_checkout", "house_overview", "egress_throttle", "chunking", "smart_demotion", "vault_parity", "sync_backend", "parallel_get"}

// FlagView is a resolved flag plus what the page says about it.
type FlagView struct {
	flags.Flag
	Tier  int
	About string
}

// flagViews orders the resolved flags (launch levers first, then the rest
// alphabetically) and attaches their descriptions.
func flagViews(resolved []flags.Flag) []FlagView {
	rank := map[string]int{}
	for i, k := range flagOrder {
		rank[k] = i
	}
	out := make([]FlagView, 0, len(resolved))
	for _, f := range resolved {
		info := flagInfos[f.Key]
		out = append(out, FlagView{Flag: f, Tier: info.Tier, About: info.About})
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, iok := rank[out[i].Key]
		rj, jok := rank[out[j].Key]
		switch {
		case iok && jok:
			return ri < rj
		case iok:
			return true
		case jok:
			return false
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// HandleAdminFlags renders the flags dashboard page.
func HandleAdminFlags(tmpl *template.Template, svc *flags.Service, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "admin-flags")
		withCSRF(r.Context(), data)
		data["Flags"] = flagViews(svc.Resolved())

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin flags", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// HandleAdminFlagSet handles POST /admin/flags/{key}/set — form fields
// `enabled` (bool) and optional `tenant_id` (empty = global row).
func HandleAdminFlagSet(svc *flags.Service, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		key := chi.URLParam(r, "key")
		enabled, err := strconv.ParseBool(r.FormValue("enabled"))
		if err != nil {
			http.Error(w, "enabled must be true or false", http.StatusBadRequest)
			return
		}

		if err := svc.Set(r.Context(), key, r.FormValue("tenant_id"), enabled, sd.Email); err != nil {
			logger.Error("dashboard flag set failed",
				zap.String("flag", key), zap.Error(err))
			http.Error(w, "failed to set flag", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/admin/flags", http.StatusSeeOther)
	}
}

// HandleAdminFlagClear handles POST /admin/flags/{key}/clear — removes the
// row for `tenant_id` (empty = the global row), reverting to global/default.
func HandleAdminFlagClear(svc *flags.Service, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		key := chi.URLParam(r, "key")
		if err := svc.Unset(r.Context(), key, r.FormValue("tenant_id")); err != nil {
			logger.Error("dashboard flag clear failed",
				zap.String("flag", key), zap.Error(err))
			http.Error(w, "failed to clear flag", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/admin/flags", http.StatusSeeOther)
	}
}
