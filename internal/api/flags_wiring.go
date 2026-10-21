package api

import (
	"os"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/payments"
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

	// flagSyncBackend lets a tenant's buckets whose tier_preference is
	// `sync` (set by an operator; no customer-facing control offers it)
	// place their objects on the Sync.com WebDAV bridge ("sync" driver).
	// Default OFF and never a global row: Sync's terms forbid reselling the
	// service without its written consent, so customer data lands there
	// only for tenants named one by one (our own, or with that consent).
	// Checked in resolvePutStorageClass via tierPlacementGate.
	flagSyncBackend = "sync_backend"

	// flagBlompBackend lets a tenant's buckets whose tier_preference is
	// `blomp` (operator-set, like `sync`) place their objects on Blomp's
	// Swift storage ("blomp" driver, drivers/swift_README.md) — sealed only:
	// its storage URL is plain HTTP, so without ENCRYPTION_MASTER_KEY such a
	// write is refused (decidePlacement). Default OFF, per tenant first.
	flagBlompBackend = "blomp_backend"

	// flagSiaBackend lets a tenant's buckets whose tier_preference is `sia`
	// (operator-set, like `blomp`) place their objects on Sia through the
	// s3d gateway ("sia" driver, drivers/sia.go) — whole and sealed only:
	// without ENCRYPTION_MASTER_KEY such a write is refused (decidePlacement)
	// unless SIA_ALLOW_UNSEALED. Default OFF, per tenant first.
	flagSiaBackend = "sia_backend"

	// flagVersionedChunking lets a chunked write (PUT, CompleteMultipartUpload,
	// the copy's manifest branch) go into an Enabled or Suspended bucket
	// (WP-VM-1 Part D): since 092 a manifest has an address and a version
	// keeps it. Default OFF, per-tenant rows first (built now, dark until
	// proven); off, versioned buckets store whole objects as before. Reads
	// never depend on it: a chunked version written while it was on reads
	// after it goes off. Checked in decidePlacement.
	flagVersionedChunking = "versioned_chunking"

	// flagEntitlementLapse lets the entitlement_lapse job clear a house whose
	// paid_through + ENTITLEMENT_GRACE has passed (WP-PAY-1). Default OFF,
	// per tenant first: off, the job only counts it (would_clear). WP-PAY-2
	// also moves the Stripe webhook's clearing branch behind it.
	flagEntitlementLapse = payments.FlagEntitlementLapse

	// flagPayStripe / flagPayBTCPay: may a tenant start new business with
	// the provider (WP-PAY-2, docs/PAYMENTS.md § 2.3). Off = checkout and
	// resize 409 provider_disabled; renewals, the webhook route, cancel and
	// the portal are unaffected. pay_stripe defaults to "STRIPE_SECRET_KEY is
	// set"; pay_btcpay defaults OFF — the BTCPay provider (WP-PAY-4) is
	// registered only with its four BTCPAY_* settings, and the flag gates
	// its checkout only (its webhook route stays mounted while configured).
	flagPayStripe = payments.FlagPrefix + payments.ProviderStripe
	flagPayBTCPay = payments.FlagPrefix + payments.ProviderBTCPay
)

// registerFlags registers every flag with its in-code default.
func registerFlags(fl *flags.Service) {
	fl.Register(flagSignups, signupsDefaultFromEnv())
	fl.Register(flagChunking, true)
	fl.Register(flagSmartDemotion, false)
	fl.Register(flagQuotaCheckout, false)
	fl.Register(flagHouseOverview, false)
	fl.Register(flagEgressThrottle, false)
	fl.Register(flagVaultParity, false)
	fl.Register(flagSyncBackend, false)
	fl.Register(flagBlompBackend, false)
	fl.Register(flagSiaBackend, false)
	fl.Register(flagVersionedChunking, false)
	fl.Register(flagParallelGet, false)
	fl.Register(flagEntitlementLapse, false)
	fl.Register(flagPayStripe, os.Getenv("STRIPE_SECRET_KEY") != "")
	fl.Register(flagPayBTCPay, false)
}

// tierGate says whether a tenant may place on a flag-gated tier class
// (SYNC, BLOMP, SIA); nil = never.
type tierGate func(class, tenantID string) bool

// tierPlacementGate is resolvePutStorageClass's per-tenant check of the
// flag-gated tiers: SYNC needs `sync_backend`, BLOMP `blomp_backend`, SIA
// `sia_backend`; nil
// (no flag service) = never.
func tierPlacementGate(fl *flags.Service) tierGate {
	if fl == nil {
		return nil
	}
	return func(class, tenantID string) bool {
		switch class {
		case "SYNC":
			return fl.Enabled(flagSyncBackend, tenantID)
		case "BLOMP":
			return fl.Enabled(flagBlompBackend, tenantID)
		case "SIA":
			return fl.Enabled(flagSiaBackend, tenantID)
		}
		return false
	}
}

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

// allowUnsealedCleartextFromEnv reads BLOMP_ALLOW_UNSEALED (`true`/`1`):
// the owner's switch for testing (2026-10-21) that lets a cleartext-
// transport backend take unsealed bytes. Set = one Warn line at boot.
func allowUnsealedCleartextFromEnv(getenv func(string) string, logger *zap.Logger) bool {
	v := strings.TrimSpace(getenv("BLOMP_ALLOW_UNSEALED"))
	on := v == "true" || v == "1"
	if v != "" && !on && v != "false" && v != "0" {
		logger.Warn("BLOMP_ALLOW_UNSEALED: not true/false — unsealed writes stay refused", zap.String("value", v))
	}
	if on {
		logger.Warn("BLOMP_ALLOW_UNSEALED is set: objects of blomp-tier buckets and Vault parity shards may cross Blomp's plain-HTTP link unsealed (testing only)")
	}
	return on
}

// allowUnsealedSiaFromEnv reads SIA_ALLOW_UNSEALED (`true`/`1`): lets the
// SIA tier (and a `sia` Vault parity leg) take unsealed bytes. Set = one
// Warn line at boot.
func allowUnsealedSiaFromEnv(getenv func(string) string, logger *zap.Logger) bool {
	v := strings.TrimSpace(getenv("SIA_ALLOW_UNSEALED"))
	on := v == "true" || v == "1"
	if v != "" && !on && v != "false" && v != "0" {
		logger.Warn("SIA_ALLOW_UNSEALED: not true/false — unsealed writes stay refused", zap.String("value", v))
	}
	if on {
		logger.Warn("SIA_ALLOW_UNSEALED is set: objects of sia-tier buckets and Vault parity shards may reach s3d and the Sia hosts unsealed (testing only)")
	}
	return on
}
