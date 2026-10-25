package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// S3 secrets wrapped under the tenant's key (Stage 1a Prompt 42, plan 43.7;
// docs/surfaces/TRUST.md item 12; contract rules 4 and 5). api_keys and
// sts_tokens carry secret_wrapped + secret_key_version (migration 108) beside
// the plaintext secret_key: every writer fills the wrapped form when the
// server has a master key (and, until the credential_plaintext_drop flag,
// still the plaintext), and every reader follows ONE rule — openStoredSecret:
//
//   - wrapped present → it is opened, and only it: a failure is an Error
//     line, vaultaire_credential_unwrap_failures_total{table} and the request
//     refused as OUR failure (ErrCredentialUnavailable) — never a fallback to
//     the plaintext column, which the drop removes;
//   - wrapped absent → the plaintext column (a row written before 108 or on a
//     server without a master key; the key_reencrypt job wraps it).

// CredentialKeys is the tenant key store's credential seam
// (crypto.TenantKeys.SealCredential / OpenCredential). nil = no master key:
// writers write the plaintext only, and a wrapped row cannot be opened.
type CredentialKeys interface {
	SealCredential(ctx context.Context, tenantID, keyID string, secret []byte) ([]byte, int, error)
	OpenCredential(ctx context.Context, tenantID, keyID string, version int, wrapped []byte) ([]byte, error)
}

// ErrCredentialUnavailable: the presented key exists and its wrapped secret
// could not be opened — no master key on this server, a key version that does
// not unwrap, a damaged row. Ours, not the caller's: the S3 layer answers 500,
// never AccessDenied and never a plaintext fallback.
var ErrCredentialUnavailable = errors.New("credential secret could not be opened")

// CredentialUnwrapFailures counts wrapped secrets that did not open, by table.
// Registered by the api package; both series exist from boot.
var CredentialUnwrapFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_credential_unwrap_failures_total",
	Help: "Wrapped S3 secrets (api_keys / sts_tokens.secret_wrapped, plan 43.7) that did not open on a credential lookup — no master key, a key version that does not unwrap, a damaged row. Each one refused a request with a 500; never a plaintext fallback.",
}, []string{"table"})

func init() {
	for _, table := range []string{"api_keys", "sts_tokens"} {
		CredentialUnwrapFailures.WithLabelValues(table)
	}
}

// openStoredSecret is the ONE reader rule. keyID is the row's access key id
// (part of the wrap's AAD); it comes from the request, so it is never logged.
func openStoredSecret(ctx context.Context, keys CredentialKeys, logger *zap.Logger, table, tenantID, keyID, plaintext string,
	wrapped []byte, version sql.NullInt64) (string, error) {
	if wrapped == nil {
		return plaintext, nil
	}
	fail := func(cause error) (string, error) {
		CredentialUnwrapFailures.WithLabelValues(table).Inc()
		if logger != nil {
			logger.Error("a wrapped S3 secret did not open — the request is refused (500), never served from the plaintext column",
				zap.String("table", table), zap.String("tenant_id", tenantID), zap.Error(cause))
		}
		return "", fmt.Errorf("%w (%s): %w", ErrCredentialUnavailable, table, cause)
	}
	if keys == nil {
		return fail(errors.New("no master key on this server (ENCRYPTION_MASTER_KEY)"))
	}
	if !version.Valid {
		return fail(errors.New("wrapped secret without a key version"))
	}
	secret, err := keys.OpenCredential(ctx, tenantID, keyID, int(version.Int64), wrapped)
	if err != nil {
		return fail(err)
	}
	return string(secret), nil
}

// sealSecret wraps a secret for a writer: nil, 0 (write the plaintext only)
// without keys; an error when keys exist and the seal fails — the write is
// refused rather than stored unwrapped.
func sealSecret(ctx context.Context, keys CredentialKeys, tenantID, keyID, secret string) ([]byte, sql.NullInt64, error) {
	if keys == nil {
		return nil, sql.NullInt64{}, nil
	}
	wrapped, version, err := keys.SealCredential(ctx, tenantID, keyID, []byte(secret))
	if err != nil {
		return nil, sql.NullInt64{}, fmt.Errorf("wrap the secret of a new key: %w", err)
	}
	return wrapped, sql.NullInt64{Int64: int64(version), Valid: true}, nil
}

// nullBytes is a BYTEA argument: SQL NULL for no bytes (lib/pq writes a nil
// []byte as an empty bytea, which is not "absent").
func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
