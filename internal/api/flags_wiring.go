package api

import (
	"os"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/dashboard"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/modes"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/produce"
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

	// flagEdgeIngest lets a tenant's PutObject and UploadPart leave SLC
	// (WP-EDGE-1 Part A, plan 40.10 slice 1): the Worker on s3.stored.cloud
	// asks the control plane to admit a whole unsealed write and streams
	// the body straight to the backend on a presigned link; the control
	// plane commits it as a PUT through SLC would. Off = every request
	// passes through to the origin. Default OFF, per tenant first (tenant
	// zero); it does nothing on a process without EDGE_SERVICE_TOKEN. Keep
	// it OFF until the build that knows edge uploads is the active slot
	// (docs/DEPLOY.md).
	flagEdgeIngest = "edge_ingest"

	// flagEdgeCodec lets a tenant's chunked-eligible PutObject / UploadPart
	// be chunked, keyed-hashed and SEALED by the data-plane host (WP-EDGE-2,
	// plan 40.10 slice 2): the admit answers the tenant's codec public key
	// instead of a presigned PUT, the host asks /api/v1/edge/index per
	// chunk, uploads the misses as 0x06 blobs and commits the manifest.
	// Needs edge_ingest's gate AND a tenant key version: on a process with
	// no master key the admit answers Part A's whole PUT (reason no_key) —
	// never a refusal and never a plaintext chunked write at the edge.
	// Default OFF, per tenant first; dark on prod until the F-14 ceremony.
	flagEdgeCodec = "edge_codec"

	// flagDataPlaneSLC sends a tenant's edge-eligible PutObject, UploadPart
	// and GetObject through the ORIGIN as a data-plane host (host=slc,
	// WP-EDGE-3): admit and commit in-process, the bytes through
	// internal/dataplane on this box — every byte labelled by host, the rows
	// the one shape. Needs edge_ingest's gate too (the admit's). Default
	// OFF, per tenant first: OFF = the handlers as today.
	flagDataPlaneSLC = "data_plane_slc"

	// flagStandardSealedDefault seals every Standard-mode write (WP-EDGE-4
	// PR C, Stage 1a Prompt 48; sealed_default.go): at the origin a
	// sub-threshold body is placed whole and SSE-S3 sealed, a chunked one
	// chunk-encrypted; at the edge it implies edge_codec for every tenant.
	// A write that cannot be sealed is 503, never plaintext. Global only,
	// default OFF; turning it on is refused (409) without a master key or
	// while a tenant has no key version. Flipped after the F-14 ceremony.
	flagStandardSealedDefault = "standard_sealed_default"

	// flagEdgeMirror gives a tenant's edge writes a SECOND copy (WP-EDGE-4,
	// plan 40.10 slice 3): the write admit answers two links — the
	// placement's backend and the first registered leg of MIRROR_LEGS that
	// is not it — the host sends the body to both, the commit records the
	// copy in blob_copies (pending when the mirror PUT failed: the
	// blob_copies job writes it); a codec write mirrors its stored chunks;
	// the blob_copies job also copies the tenant's other objects (origin
	// writes, multipart uploads, everything written before) and a read
	// falls back to the mirror before the origin. Default OFF, per tenant.
	flagEdgeMirror = "edge_mirror"

	// flagRequestLog writes every S3 request of the tenant to the request
	// log (WP-LOG-1, request_log.go: the Developers page, GET
	// /api/v1/manage/requests, 7 days). Default ON; a global OFF row is the
	// switch for a write path that misbehaves; per tenant.
	flagRequestLog = "request_log"

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
	fl.Register(flagEdgeIngest, false)
	fl.Register(flagEdgeCodec, false)
	fl.Register(flagDataPlaneSLC, false)
	fl.Register(flagEdgeMirror, false)
	fl.Register(flagRequestLog, true)
	fl.Register(flagEdgeRS, false)
	fl.Register(flagParallelGet, false)
	fl.Register(flagEntitlementLapse, false)
	fl.Register(flagPayStripe, os.Getenv("STRIPE_SECRET_KEY") != "")
	fl.Register(flagPayBTCPay, false)
	// K3 PR 3: the mode copy from modes.json — the dashboard's bucket
	// labels per tenant, the privacy page's per-mode table globally (the
	// owner flips it after reading: a legal page). Default OFF.
	fl.Register(modes.FlagModesCopy, false)
	// K4: the transparency log's public read side (GET /translog/…).
	// Default OFF, global: flipped when trust.stored.cloud is decided.
	fl.Register(flagTranslogPublic, false)
	fl.Register(flagTranslogAdminReads, false)   // K4c-1: per subject tenant first; the owner flips it globally
	fl.Register(flagTranslogCustomerView, false) // K4c-1: GET /api/v1/manage/account/transparency, per tenant first
	// RP44-2 (Stage 1a Prompt 34a): a tenant row can only turn the record of
	// admin reads — and the tenant's view of it — ON; translog_public is
	// read globally only. Every write of these rows is a log_policy entry
	// (the server's write guard, translog_flags.go).
	fl.OnWins(flagTranslogAdminReads)
	fl.OnWins(flagTranslogCustomerView)
	fl.GlobalOnly(flagTranslogPublic)
	// K5: the recovery vault's token minter (POST /api/v1/recovery/tokens).
	// Default OFF, global: a 404 to everyone until the owner opens it. Read
	// before authentication, so a tenant row could open nothing: refused.
	fl.Register(flagRecoveryVault, false)
	fl.GlobalOnly(flagRecoveryVault)
	// 43.4: the law-enforcement policy and the "what we can produce" page
	// (/legal/law-enforcement, /legal/what-we-can-produce) on the canonical
	// origin and the trust host. Default OFF = 404 on both, global only: the
	// owner opens them once counsel has written the policy (F-36).
	fl.Register(produce.FlagLegalLEPages, false)
	fl.GlobalOnly(produce.FlagLegalLEPages)
	// Stage 1a Prompt 39: /legal/refunds and /legal/dmca on the web host.
	// Default OFF = the web 404, global only: every commitment on them is a
	// marked DRAFT until the owner's words land (F-28 refunds, F-37 the DMCA
	// agent).
	fl.Register(dashboard.FlagLegalDraftPages, false)
	fl.GlobalOnly(dashboard.FlagLegalDraftPages)
	// Stage 1a Prompt 42 PR C (plan 43.7): the S3 secrets' plaintext column
	// stops being written, and the key_reencrypt job nulls it on every row
	// whose wrapped form opens back to it. Default OFF, global only; turning
	// it on is refused while there is no master key or a live row is
	// unwrapped (credential_drop.go). Once on, a build older than PR B cannot
	// sign any S3 request (vaultaire-switch refuses one).
	fl.Register(flagCredentialPlaintextDrop, false)
	fl.GlobalOnly(flagCredentialPlaintextDrop)
	fl.Register(flagStandardSealedDefault, false)
	fl.GlobalOnly(flagStandardSealedDefault)
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

// allowUnsealedTapeFromEnv reads GEYSER_ALLOW_UNSEALED (`true`/`1`): lets
// the archive classes (GLACIER, DEEP_ARCHIVE) and smart demotion put bytes
// on tape that are not sealed. Labs only — a byte on tape is never erased.
// Anything else is false (a Warn for a value that is neither).
func allowUnsealedTapeFromEnv(getenv func(string) string, logger *zap.Logger) bool {
	v := strings.TrimSpace(getenv("GEYSER_ALLOW_UNSEALED"))
	on := v == "true" || v == "1"
	if v != "" && !on && v != "false" && v != "0" {
		logger.Warn("GEYSER_ALLOW_UNSEALED: not true/false — unsealed writes to tape stay refused", zap.String("value", v))
	}
	if on {
		logger.Warn("GEYSER_ALLOW_UNSEALED is set: archive-class writes and smart demotions may reach tape unsealed, where a delete never erases them (testing only)")
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
