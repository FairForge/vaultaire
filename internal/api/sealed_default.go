package api

// standard_sealed_default (WP-EDGE-4 PR C, Stage 1a Prompt 48; plan 40.10
// slice 3 (b)): the switch that makes modes.json's Standard sentence true on
// every backend. ON = every Standard-mode write — not SSE-C (Private, the
// customer's key already seals it), not a public bucket (Public is meant to
// be read) — is placed sealed: decidePlacement treats it as an encryption
// requirement (a sub-threshold body is placeWholeSealed, a chunked one
// chunk-encrypted), and at the edge it implies edge_codec for every tenant
// (the host seals). A write that cannot be sealed — no master key on this
// process — is 503 SlowDown, nothing stored, never plaintext.
//
// Global only, default OFF. Turning it on is refused (409, nothing written)
// while this server has no master key or while any tenant has no key
// version yet (the first write of a tenant creates one; the owner's order
// after the F-14 ceremony: every tenant's version exists, then this flag).
// Turning it off is always allowed: new writes are placed as before; what
// was sealed stays sealed.

import (
	"context"
	"fmt"

	"github.com/FairForge/vaultaire/internal/flags"
)

// sealedByDefault reports whether the flag is on.
func (a *S3ToEngine) sealedByDefault() bool {
	return a.flags != nil && a.flags.Enabled(flagStandardSealedDefault, "")
}

// edgeCodecFor: the tenant's edge writes are the codec's — its own
// edge_codec flag, or standard_sealed_default for every tenant.
func (s *Server) edgeCodecFor(tenantID string) bool {
	return s.flags != nil && (s.flags.Enabled(flagEdgeCodec, tenantID) || s.flags.Enabled(flagStandardSealedDefault, ""))
}

// tenantsWithoutKeysSQL counts tenants with no key version ($1 = one
// tenant, or ” — tests scope it on the shared database).
const tenantsWithoutKeysSQL = `
	SELECT COUNT(*) FROM tenants t
	WHERE ($1 = '' OR t.id = $1)
	  AND NOT EXISTS (SELECT 1 FROM tenant_key_versions k WHERE k.tenant_id = t.id)`

// guardStandardSealedDefault refuses turning the flag on before it is safe.
func (s *Server) guardStandardSealedDefault(ctx context.Context, c flags.Change) error {
	if c.Key != flagStandardSealedDefault || c.Clear || !c.Enabled {
		return nil
	}
	if s.tenantKeys == nil {
		return &flags.Refusal{Flag: flagStandardSealedDefault, Code: "master_key_absent",
			Msg: "this server has no master key, so nothing could be sealed"}
	}
	if s.db == nil {
		return fmt.Errorf("standard_sealed_default: no database to count tenants without a key version")
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, tenantsWithoutKeysSQL, s.credentialScope).Scan(&n); err != nil {
		return fmt.Errorf("standard_sealed_default: count tenants without a key version: %w", err)
	}
	if n > 0 {
		return &flags.Refusal{Flag: flagStandardSealedDefault, Code: "tenant_keys_missing", Count: n,
			Msg: fmt.Sprintf("%d tenant(s) have no key version yet", n)}
	}
	return nil
}
