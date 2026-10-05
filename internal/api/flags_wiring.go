package api

import (
	"os"
	"strconv"

	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
)

// Day-one feature flags (1.13 live-iteration kit). Adding a flag is a key
// constant + a Register call in NewServer + a call site — no schema change.
const (
	// flagSignups gates public account creation. Global-only (tenant IDs are
	// meaningless for signup), checked via the auth chokepoint
	// CreateUserWithTenant. Its in-code default reads SIGNUPS_ENABLED so a
	// deploy never reopens signups; a DB row overrides env with no deploy.
	flagSignups = "signups"

	// flagChunking is the kill-switch + per-tenant override for the
	// content-defined chunking/dedup PUT path, gated at the handleChunkedPut
	// entry check. Off ⇒ plain whole-object PUTs; reads are unaffected
	// (manifests are self-describing, chunked GETs keep working).
	flagChunking = "chunking"

	// flagSmartDemotion enables the Smart-tier demotion job (Phase 5.15.8)
	// per tenant (tenant row) or globally ('*' row). Default OFF: the job
	// ships flag-dark and is enabled tenant-by-tenant first. Checked per
	// tenant inside SmartDemotionRunner.RunOnce.
	flagSmartDemotion = "smart_demotion"

	// flagQuotaCheckout opens the whole-TB house checkout on the billing
	// page (dashboard plan Phase 1; Tier 2 in the runbook). Default OFF:
	// ships dark, opened per tenant from /admin/flags, then globally for
	// launch. The Stripe webhook is NOT gated — a bought house is always
	// applied. Checked in dashboard/handlers (billing_house.go).
	flagQuotaCheckout = handlers.FlagQuotaCheckout

	// flagHouseOverview draws the customer's house (pieces per floor from
	// the quota, fill from usage) on the dashboard overview in place of the
	// storage gauge (dashboard plan Phase 2). Default OFF, per tenant first.
	flagHouseOverview = handlers.FlagHouseOverview

	// flagEgressThrottle turns the egress allowance into a rate cap
	// (WP-R10-9, decision D-25): a tenant past its monthly allowance has its
	// GetObject and /cdn bodies paced. Default OFF: the same decision is only
	// counted (vaultaire_egress_would_throttle_total). On globally, a tenant
	// row with enabled=false is the exemption (synthetic check, demos).
	flagEgressThrottle = "egress_throttle"

	// flagVaultParity enables the Vault parity second copy (WP-VAULT-1):
	// the `vault_parity` job writes RS 4+4 parity shards of every
	// vault-floor object to the free leg, and a read whose backend fails
	// is rebuilt from them. Default OFF, per tenant first. Checked per
	// tenant in VaultParity.RunOnce and VaultParity.Open.
	flagVaultParity = "vault_parity"
)

// signupsDefaultFromEnv is the `signups` flag's in-code default: the
// SIGNUPS_ENABLED env var (unset or unparsable = enabled, matching the
// pre-1.13 behavior). The env value only seeds the DEFAULT — a feature_flags
// row overrides it in either direction at runtime.
func signupsDefaultFromEnv() bool {
	if v := os.Getenv("SIGNUPS_ENABLED"); v != "" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			return enabled
		}
	}
	return true
}
