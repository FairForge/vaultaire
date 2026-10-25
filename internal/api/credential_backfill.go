package api

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"

	"github.com/FairForge/vaultaire/internal/auth"
	"go.uber.org/zap"
)

// The key_reencrypt job's credential pass (Stage 1a Prompt 42 PR B, plan
// 43.7; docs/surfaces/TRUST.md item 12): every live S3 secret ends up wrapped
// under its tenant's NEWEST key version.
//
//  1. wrap: a live row (an api_keys row not revoked, an sts_tokens row not
//     expired) holding only the plaintext — written before migration 108,
//     or by a server with no master key — is sealed;
//  2. re-wrap: a live wrapped row below its tenant's newest version (a
//     tenant key rotation moves credentials as it moves chunks and objects)
//     is opened with its version and sealed with the newest.
//
// Each seal is opened again and compared with the secret before anything is
// written, and the write is guarded on the value the scan read (NULL or the
// old wrap): a row another process wrapped meanwhile is left as it is
// (CredentialsRaced). A revoked key or an expired token never authenticates,
// so it is never wrapped; credential_plaintext_drop (PR C) nulls only rows
// whose wrap opens.
//
// Why here and not at lookup (the plan's "lazy backfill at lookup"): a write
// on the SigV4 path would add a database write, a seal and a failure mode to
// the hottest read the server has, and a key that is never presented would
// never be wrapped — the drop could then never prove "every live row is
// wrapped". The job wraps every row within a few runs of the master key's
// arrival, whether or not it is used.

// defaultCredentialBatch bounds the rows one run seals (wrap + re-wrap).
const defaultCredentialBatch = 1000

// credentialStore is what the pass needs of the key store (crypto.TenantKeys;
// tests inject a sabotaged seal).
type credentialStore = auth.CredentialKeys

type credentialRow struct {
	table, keyID, tenantID string
	plaintext              sql.NullString
	wrapped                []byte
	version                sql.NullInt64
}

// credentialCandidatesSQL reads, oldest first, the live rows the pass owes:
// unwrapped ones and wrapped ones below their tenant's newest version. $1 =
// one tenant or ” (tests scope it), $2 = the batch.
const credentialCandidatesSQL = `
	WITH cur AS (
		SELECT tenant_id, MAX(key_version) AS v FROM tenant_key_versions
		 WHERE ($1 = '' OR tenant_id = $1) GROUP BY tenant_id
	)
	SELECT * FROM (
		SELECT 'api_keys' AS tbl, ak.key_id, ak.tenant_id, ak.secret_key, ak.secret_wrapped, ak.secret_key_version, ak.created_at
		  FROM api_keys ak LEFT JOIN cur ON cur.tenant_id = ak.tenant_id
		 WHERE ak.revoked_at IS NULL AND ak.tenant_id IS NOT NULL AND ($1 = '' OR ak.tenant_id = $1)
		   AND ((ak.secret_wrapped IS NULL AND ak.secret_key IS NOT NULL)
		        OR (ak.secret_wrapped IS NOT NULL AND ak.secret_key_version < cur.v))
		UNION ALL
		SELECT 'sts_tokens', s.access_key, s.tenant_id, s.secret_key, s.secret_wrapped, s.secret_key_version, s.created_at
		  FROM sts_tokens s LEFT JOIN cur ON cur.tenant_id = s.tenant_id
		 WHERE s.expires_at > NOW() AND ($1 = '' OR s.tenant_id = $1)
		   AND ((s.secret_wrapped IS NULL AND s.secret_key IS NOT NULL)
		        OR (s.secret_wrapped IS NOT NULL AND s.secret_key_version < cur.v))
	) c ORDER BY created_at LIMIT $2`

// wrapCredentials is the pass. A scan that fails fails the run; a row that
// fails is a note and stays a candidate.
func (k *KeyReencryptRunner) wrapCredentials(ctx context.Context, res *KeyReencryptResult) error {
	keys := k.credKeys
	if keys == nil && k.enc != nil {
		if tk := k.enc.Keys(); tk != nil {
			keys = tk // never a typed nil in the interface
		}
	}
	if keys == nil {
		return nil
	}
	batch := k.credentialBatch
	if batch <= 0 {
		batch = defaultCredentialBatch
	}
	rows, err := k.db.QueryContext(ctx, credentialCandidatesSQL, k.onlyScope, batch)
	if err != nil {
		return fmt.Errorf("read credentials to wrap: %w", err)
	}
	var due []credentialRow
	for rows.Next() {
		var r credentialRow
		var created sql.NullTime
		if err := rows.Scan(&r.table, &r.keyID, &r.tenantID, &r.plaintext, &r.wrapped, &r.version, &created); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan credential: %w", err)
		}
		due = append(due, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read credentials to wrap: %w", err)
	}
	_ = rows.Close()

	for _, r := range due {
		if ctx.Err() != nil {
			return nil // the rest waits for the next run
		}
		rewrap := r.wrapped != nil
		wrote, err := k.wrapOne(ctx, keys, r)
		switch {
		case err != nil:
			res.CredentialFailures++
			res.fail("credential (%s) of %s: %v", r.table, r.tenantID, err)
			// The key id is a credential identifier: never logged.
			k.logger.Warn("key re-encrypt: a credential was not wrapped (it stays a candidate)",
				zap.String("table", r.table), zap.String("tenant_id", r.tenantID), zap.Bool("rewrap", rewrap), zap.Error(err))
		case !wrote:
			res.CredentialsRaced++
		case rewrap:
			res.CredentialsRewrapped++
		default:
			res.CredentialsWrapped++
		}
	}
	if k.dropPlaintext != nil && k.dropPlaintext() {
		return k.dropPlaintextPass(ctx, keys, batch, res)
	}
	return nil
}

// dropCandidatesSQL reads the rows whose plaintext the drop owes: a wrapped
// row still holding it, and a dead row (a revoked key, a key with no tenant,
// an expired token — none ever authenticates: the lookup refuses it before
// any secret is read) holding it with or without a wrap.
const dropCandidatesSQL = `
	SELECT * FROM (
		SELECT 'api_keys' AS tbl, key_id, COALESCE(tenant_id, '') AS tenant_id, secret_key, secret_wrapped, secret_key_version,
		       (revoked_at IS NOT NULL OR tenant_id IS NULL) AS dead
		  FROM api_keys
		 WHERE secret_key IS NOT NULL AND ($1 = '' OR tenant_id = $1)
		   AND (secret_wrapped IS NOT NULL OR revoked_at IS NOT NULL OR tenant_id IS NULL)
		UNION ALL
		SELECT 'sts_tokens', access_key, tenant_id, secret_key, secret_wrapped, secret_key_version, expires_at <= NOW()
		  FROM sts_tokens
		 WHERE secret_key IS NOT NULL AND ($1 = '' OR tenant_id = $1)
		   AND (secret_wrapped IS NOT NULL OR expires_at <= NOW())
	) c LIMIT $2`

// dropPlaintextPass is the PR C half of the pass: a live row's plaintext is
// nulled only after its wrapped form opened back to exactly it; the write is
// guarded on the plaintext and the wrap the check saw. Then the tenants
// mirror is nulled where the tenant's live primary has no plaintext left.
func (k *KeyReencryptRunner) dropPlaintextPass(ctx context.Context, keys credentialStore, batch int, res *KeyReencryptResult) error {
	rows, err := k.db.QueryContext(ctx, dropCandidatesSQL, k.onlyScope, batch)
	if err != nil {
		return fmt.Errorf("read plaintext secrets to drop: %w", err)
	}
	type dropRow struct {
		credentialRow
		dead bool
	}
	var due []dropRow
	for rows.Next() {
		var r dropRow
		if err := rows.Scan(&r.table, &r.keyID, &r.tenantID, &r.plaintext, &r.wrapped, &r.version, &r.dead); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan plaintext secret: %w", err)
		}
		due = append(due, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read plaintext secrets to drop: %w", err)
	}
	_ = rows.Close()

	for _, r := range due {
		if ctx.Err() != nil {
			return nil
		}
		dropped, err := k.dropOne(ctx, keys, r.credentialRow, r.dead)
		switch {
		case err != nil:
			res.CredentialFailures++
			res.fail("plaintext (%s) of %s kept: %v", r.table, r.tenantID, err)
			k.logger.Warn("key re-encrypt: a plaintext secret was kept (its wrap does not open back to it)",
				zap.String("table", r.table), zap.String("tenant_id", r.tenantID), zap.Error(err))
		case dropped:
			res.CredentialsPlaintextDropped++
		default:
			res.CredentialsRaced++
		}
	}

	out, err := k.db.ExecContext(ctx, `
		UPDATE tenants SET secret_key = NULL
		 WHERE id IN (SELECT t.id FROM tenants t
		               WHERE t.secret_key IS NOT NULL AND ($1 = '' OR t.id = $1)
		                 AND EXISTS (SELECT 1 FROM api_keys ak
		                              WHERE ak.tenant_id = t.id AND ak.is_primary AND ak.revoked_at IS NULL
		                                AND ak.secret_wrapped IS NOT NULL AND ak.secret_key IS NULL)
		               LIMIT $2)`, k.onlyScope, batch)
	if err != nil {
		return fmt.Errorf("drop the tenants mirrors: %w", err)
	}
	n, err := out.RowsAffected()
	if err != nil {
		return fmt.Errorf("drop the tenants mirrors: %w", err)
	}
	res.MirrorsPlaintextDropped += int(n)
	return nil
}

// dropOne nulls one row's plaintext: a live row only once its wrap opened
// back to exactly that plaintext; a dead row (it never authenticates) as it
// is. dropped = false: the row changed first.
func (k *KeyReencryptRunner) dropOne(ctx context.Context, keys credentialStore, r credentialRow, dead bool) (bool, error) {
	if r.wrapped != nil && !dead {
		if !r.version.Valid {
			return false, fmt.Errorf("wrapped without a key version")
		}
		back, err := keys.OpenCredential(ctx, r.tenantID, r.keyID, int(r.version.Int64), r.wrapped)
		if err != nil {
			return false, fmt.Errorf("the wrap does not open: %w", err)
		}
		if !bytes.Equal(back, []byte(r.plaintext.String)) {
			return false, fmt.Errorf("the wrap opens to another secret")
		}
	}
	var q string
	switch r.table {
	case "api_keys":
		q = `UPDATE api_keys SET secret_key = NULL
		      WHERE key_id = $1 AND secret_key = $2 AND secret_wrapped IS NOT DISTINCT FROM $3`
	case "sts_tokens":
		q = `UPDATE sts_tokens SET secret_key = NULL
		      WHERE access_key = $1 AND secret_key = $2 AND secret_wrapped IS NOT DISTINCT FROM $3`
	default:
		return false, fmt.Errorf("unknown credential table %q", r.table)
	}
	var wrapped any
	if r.wrapped != nil {
		wrapped = r.wrapped
	}
	out, err := k.db.ExecContext(ctx, q, r.keyID, r.plaintext.String, wrapped)
	if err != nil {
		return false, fmt.Errorf("null the plaintext: %w", err)
	}
	n, err := out.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("null the plaintext: %w", err)
	}
	return n == 1, nil
}

// wrapOne seals one row's secret under the tenant's newest version, checks
// that it opens back to the secret, and writes it guarded on what the scan
// read. wrote = false: another process changed the row first.
func (k *KeyReencryptRunner) wrapOne(ctx context.Context, keys credentialStore, r credentialRow) (bool, error) {
	var secret []byte
	if r.wrapped != nil {
		if !r.version.Valid {
			return false, fmt.Errorf("wrapped without a key version")
		}
		s, err := keys.OpenCredential(ctx, r.tenantID, r.keyID, int(r.version.Int64), r.wrapped)
		if err != nil {
			return false, fmt.Errorf("open the old wrap: %w", err)
		}
		secret = s
	} else {
		secret = []byte(r.plaintext.String)
	}
	wrapped, version, err := keys.SealCredential(ctx, r.tenantID, r.keyID, secret)
	if err != nil {
		return false, err
	}
	if r.wrapped != nil && int64(version) <= r.version.Int64 {
		// The store's current version is cached (≤ 1 min): the rotation
		// is not visible to it yet. The next run moves the row.
		return false, fmt.Errorf("the key store still names v%d as current", version)
	}
	back, err := keys.OpenCredential(ctx, r.tenantID, r.keyID, version, wrapped)
	if err != nil {
		return false, fmt.Errorf("the new wrap does not open: %w", err)
	}
	if !bytes.Equal(back, secret) {
		return false, fmt.Errorf("the new wrap opens to another secret")
	}
	if k.beforeCredentialWrite != nil {
		k.beforeCredentialWrite(r.table, r.keyID)
	}

	var q string
	switch r.table {
	case "api_keys":
		q = `UPDATE api_keys SET secret_wrapped = $1, secret_key_version = $2
		      WHERE key_id = $3 AND secret_wrapped IS NOT DISTINCT FROM $4`
	case "sts_tokens":
		q = `UPDATE sts_tokens SET secret_wrapped = $1, secret_key_version = $2
		      WHERE access_key = $3 AND secret_wrapped IS NOT DISTINCT FROM $4`
	default:
		return false, fmt.Errorf("unknown credential table %q", r.table)
	}
	var old any
	if r.wrapped != nil {
		old = r.wrapped
	}
	out, err := k.db.ExecContext(ctx, q, wrapped, version, r.keyID, old)
	if err != nil {
		return false, fmt.Errorf("write the wrap: %w", err)
	}
	n, err := out.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("write the wrap: %w", err)
	}
	return n == 1, nil
}
