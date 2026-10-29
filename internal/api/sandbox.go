package api

// Test mode (WP-DX-1, Stage 1a Prompt 50; plan § WP-STRIPE): a VLT_TEST_
// key resolves to its tenant's sandbox (tenants.sandbox_of, migration 116;
// internal/auth/testkey.go). What a sandbox may do is decided here, once:
//
//   - every write lands on the `local` driver (placeOn's first branch),
//     whole and as received — never chunked, sealed, mirrored or tiered
//     (decidePlacement's first branch), never on a paid backend;
//   - the edge passes a sandbox's requests to the origin (edgeReasonSandbox);
//   - metered, never billed (RP80-1): its downloads are counted under the
//     sandbox's own id (bandwidth_usage_daily, the egress meter) against a
//     fixed allowance (sandboxEgressAllowance) and paced past it like any
//     tenant's — a test key is no free file host; nothing reaches the
//     parent's counters, a payment or an entitlement;
//   - its presigned links live at most an hour (testPresignMaxExpires) and
//     its buckets are never Public (modes.ErrSandboxNeverPublic);
//   - its own tenant_quotas row (the free tier's 5 GiB) is the cap on the
//     local disk, never a charge — a sandbox has no entitlement and no
//     payment provider;
//   - its parent's suspension is its own (isTenantSuspended);
//   - every S3 response says x-stored-mode: test.
//
// Whether a tenant is a sandbox never changes (a sandbox is a new tenants
// row; a live tenant never gains sandbox_of), so the answer is cached for
// the life of the process.

import (
	"context"
	"database/sql"
	"sync"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/usage"
)

// notSandboxO / notSandboxH are the clause a job's candidate query adds so
// a sandbox's rows are never copied onto a paid backend (vault parity's
// leg, the mirror's), for an object_head_cache aliased o / h.
const (
	notSandboxO = `NOT EXISTS (SELECT 1 FROM tenants sbx WHERE sbx.id = o.tenant_id AND sbx.sandbox_of IS NOT NULL)`
	notSandboxH = `NOT EXISTS (SELECT 1 FROM tenants sbx WHERE sbx.id = h.tenant_id AND sbx.sandbox_of IS NOT NULL)`
)

// sandboxBackend is where a sandbox's bytes go.
const sandboxBackend = "local"

// sandboxEgressAllowance is a sandbox's monthly download allowance (the
// one definition is usage's: EgressAllowance answers it for a sandbox).
const sandboxEgressAllowance = usage.SandboxEgressAllowance

// testPresignMaxExpires bounds a sandbox's presigned link (RP80-1).
const testPresignMaxExpires = 3600

// parentOfSandbox is the lookup (a test makes it fail).
var parentOfSandbox = func(ctx context.Context, db *sql.DB, tenantID string) (string, error) {
	return auth.ParentOfSandbox(ctx, db, tenantID)
}

// sandboxCache: tenant id → its parent ("" = a live tenant).
var sandboxCache sync.Map

// sandboxLookup is the parent of tenantID when it is a sandbox, "" for a
// live tenant (or no database); a lookup that fails is its error and is
// not cached. Placement fails closed on it (RP80-5): a write whose tenant
// cannot be told apart is a 503, never a paid backend for a sandbox.
func sandboxLookup(ctx context.Context, db *sql.DB, tenantID string) (string, error) {
	if db == nil || tenantID == "" {
		return "", nil
	}
	if v, ok := sandboxCache.Load(tenantID); ok {
		return v.(string), nil
	}
	p, err := parentOfSandbox(ctx, db, tenantID)
	if err != nil {
		return "", err
	}
	sandboxCache.Store(tenantID, p)
	return p, nil
}

// sandboxParent is sandboxLookup where a failure reads as a live tenant —
// for what is no placement (the header, the events' livemode, the edge's
// pass-through, which the origin then places).
func sandboxParent(ctx context.Context, db *sql.DB, tenantID string) string {
	p, _ := sandboxLookup(ctx, db, tenantID)
	return p
}

// isSandbox reports whether tenantID is a sandbox.
func isSandbox(ctx context.Context, db *sql.DB, tenantID string) bool {
	return sandboxParent(ctx, db, tenantID) != ""
}
