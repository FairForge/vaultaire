package api

// Test mode (WP-DX-1, Stage 1a Prompt 50; plan § WP-STRIPE): a VLT_TEST_
// key resolves to its tenant's sandbox (tenants.sandbox_of, migration 116;
// internal/auth/testkey.go). What a sandbox may do is decided here, once:
//
//   - every write lands on the `local` driver (placeOn's first branch),
//     whole and as received — never chunked, sealed, mirrored or tiered
//     (decidePlacement's first branch), never on a paid backend;
//   - the edge passes a sandbox's requests to the origin (edgeReasonSandbox);
//   - never metered: the bandwidth tracker and the egress meter skip it;
//   - never billed: its own tenant_quotas row (the free tier's 5 GiB) is
//     the cap on the local disk, never a charge — a sandbox has no
//     entitlement and no payment provider;
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

// sandboxCache: tenant id → its parent ("" = a live tenant).
var sandboxCache sync.Map

// sandboxParent is the parent of tenantID when it is a sandbox, "" for a
// live tenant (or no database). A lookup that fails is "" and not cached:
// the write goes where a live tenant's would — never `local` by mistake for
// a live tenant.
func sandboxParent(ctx context.Context, db *sql.DB, tenantID string) string {
	if db == nil || tenantID == "" {
		return ""
	}
	if v, ok := sandboxCache.Load(tenantID); ok {
		return v.(string)
	}
	p, err := auth.ParentOfSandbox(ctx, db, tenantID)
	if err != nil {
		return ""
	}
	sandboxCache.Store(tenantID, p)
	return p
}

// isSandbox reports whether tenantID is a sandbox.
func isSandbox(ctx context.Context, db *sql.DB, tenantID string) bool {
	return sandboxParent(ctx, db, tenantID) != ""
}
