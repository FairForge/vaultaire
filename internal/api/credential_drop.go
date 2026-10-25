package api

import (
	"context"
	"fmt"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/flags"
)

// The contract half of plan 43.7 (Stage 1a Prompt 42 PR C; TRUST.md item 12;
// contract rules 4 and 5): with credential_plaintext_drop ON, no S3 secret is
// written in the clear any more — api_keys, sts_tokens and the tenants mirror
// get NULL where a wrapped form was written — and the key_reencrypt job's
// credential pass nulls the plaintext of every row whose wrapped form opens
// back to it (credential_backfill.go). A dump of the database then holds no
// usable S3 secret without the master key.
//
// The owner turns it on after the F-14 ceremony and one deploy. Turning it on
// is refused (409, nothing written, nothing logged) while this server has no
// master key — nothing could be wrapped — or while any live row still holds
// only the plaintext: the drop must never leave a key that cannot sign.
// Turning it off is always allowed: writers write the plaintext again; what
// was nulled stays null (the wrapped form is the credential).
//
// Once on, a build older than PR B cannot sign any S3 request (it reads only
// the plaintext): vaultaire-switch refuses to start one (docs/DEPLOY.md).

const flagCredentialPlaintextDrop = "credential_plaintext_drop"

// credentialPlaintextDropped reports whether writers drop the plaintext: the
// flag is on AND this server can wrap.
func (s *Server) credentialPlaintextDropped() bool {
	return s.tenantKeys != nil && s.flags != nil && s.flags.Enabled(flagCredentialPlaintextDrop, "")
}

// unwrappedLiveCredentialsSQL counts the live rows holding only the plaintext
// ($1 = one tenant or ” — tests scope it; the shared database holds other
// packages' tenants).
const unwrappedLiveCredentialsSQL = `
	SELECT (SELECT COUNT(*) FROM api_keys
	         WHERE revoked_at IS NULL AND tenant_id IS NOT NULL AND secret_wrapped IS NULL AND secret_key IS NOT NULL
	           AND ($1 = '' OR tenant_id = $1))
	     + (SELECT COUNT(*) FROM sts_tokens
	         WHERE expires_at > NOW() AND secret_wrapped IS NULL AND secret_key IS NOT NULL
	           AND ($1 = '' OR tenant_id = $1))`

// guardCredentialPlaintextDrop refuses turning the flag on before it is safe.
func (s *Server) guardCredentialPlaintextDrop(ctx context.Context, c flags.Change) error {
	if c.Key != flagCredentialPlaintextDrop || c.Clear || !c.Enabled {
		return nil
	}
	if s.tenantKeys == nil {
		return &auth.PlaintextDropRefusal{Code: "master_key_absent"}
	}
	if s.db == nil {
		return fmt.Errorf("credential_plaintext_drop: no database to count unwrapped secrets")
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, unwrappedLiveCredentialsSQL, s.credentialScope).Scan(&n); err != nil {
		return fmt.Errorf("credential_plaintext_drop: count unwrapped secrets: %w", err)
	}
	if n > 0 {
		return &auth.PlaintextDropRefusal{Code: "credentials_not_wrapped", Unwrapped: n}
	}
	return nil
}
