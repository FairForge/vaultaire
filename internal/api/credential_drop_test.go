package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Stage 1a Prompt 42 PR C (plan 43.7; TRUST.md item 12): the
// credential_plaintext_drop flag. Every test runs on its own tenant; the
// flag's state comes from an in-memory flags service (a global row on the
// shared database would turn it on for every package's servers).

// withPlaintextDrop gives the server a flags service whose
// credential_plaintext_drop is on (or off) with no database row.
func withPlaintextDrop(on bool) func(*Server) {
	return func(s *Server) {
		fl := flags.New(nil, zap.NewNop())
		fl.Register(flagCredentialPlaintextDrop, on)
		s.flags = fl
	}
}

// plaintextOf reads a row's plaintext column (nil = NULL).
func (f *lifecycleFixture) plaintextOf(table, idCol, keyID string) *string {
	f.t.Helper()
	var p sql.NullString
	require.NoError(f.t, f.db.QueryRow(`SELECT secret_key FROM `+table+` WHERE `+idCol+` = $1`, keyID).Scan(&p))
	if !p.Valid {
		return nil
	}
	return &p.String
}

func (f *lifecycleFixture) mirrorOf() *string {
	return f.plaintextOf("tenants", "id", f.tenant.ID)
}

func flagSetRequest(t *testing.T, s *Server, enabled bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/v1/admin/flags/"+flagCredentialPlaintextDrop, strings.NewReader(fmt.Sprintf(`{"enabled":%t}`, enabled)))
	rc := chi.NewRouteContext()
	rc.URLParams.Add("key", flagCredentialPlaintextDrop)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rc))
	rr := httptest.NewRecorder()
	s.handleAdminFlagSet(rr, req)
	return rr
}

func TestCredentialPlaintextDrop_RefusedWithoutKeyOrWithUnwrappedRows(t *testing.T) {
	f := setupLifecycleFixture(t) // registered by a server WITHOUT a master key: plaintext only
	ctx := context.Background()
	fl := flags.New(f.db, zap.NewNop())
	registerFlags(fl)
	f.srv.useFlags(fl)
	f.srv.credentialScope = f.tenant.ID
	// Only this test may write the flag's rows; a row it left (a guard that
	// let a write through) would turn the drop on for every package's
	// servers on the shared database.
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM feature_flags WHERE flag_key = $1`, flagCredentialPlaintextDrop) })
	globalRow := func() bool {
		var n int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM feature_flags WHERE flag_key = $1`, flagCredentialPlaintextDrop).Scan(&n))
		return n > 0
	}
	require.False(t, globalRow())

	// No master key: 409 master_key_absent, nothing written.
	rr := flagSetRequest(t, f.srv, true)
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "master_key_absent", body["error"])
	assert.False(t, globalRow(), "a refused change writes nothing")

	// A master key, and the tenant's primary still unwrapped: 409 with the
	// count.
	f.srv.tenantKeys = testTenantKeys(t, f.db, keyTestMasterA)
	rr = flagSetRequest(t, f.srv, true)
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "credentials_not_wrapped", body["error"])
	assert.Equal(t, float64(1), body["unwrapped"])
	assert.False(t, globalRow())
	// The same refusal through the flags service (the dashboard's path).
	err := fl.Set(ctx, flagCredentialPlaintextDrop, "", true, "test")
	var refusal *auth.PlaintextDropRefusal
	require.True(t, errors.As(err, &refusal), "%v", err)
	assert.Equal(t, int64(1), refusal.Unwrapped)

	// A tenant row is refused (global only).
	assert.ErrorIs(t, fl.Set(ctx, flagCredentialPlaintextDrop, f.tenant.ID, true, "test"), flags.ErrGlobalOnly)

	// After the job wrapped every live row, turning it on passes the guard
	// (asked directly: a global row on the shared database would reach
	// every other package's servers).
	_, err = f.credentialRunner(f.srv.tenantKeys).RunOnce(ctx)
	require.NoError(t, err)
	assert.NoError(t, f.srv.guardCredentialPlaintextDrop(ctx, flags.Change{Key: flagCredentialPlaintextDrop, TenantID: flags.GlobalTenant, Enabled: true}))

	// Turning it off, or clearing it, is never refused — even with no key.
	f.srv.tenantKeys = nil
	assert.NoError(t, f.srv.guardCredentialPlaintextDrop(ctx, flags.Change{Key: flagCredentialPlaintextDrop, TenantID: flags.GlobalTenant, Enabled: false}))
	assert.NoError(t, f.srv.guardCredentialPlaintextDrop(ctx, flags.Change{Key: flagCredentialPlaintextDrop, TenantID: flags.GlobalTenant, Clear: true}))
	// Another flag is not this guard's business.
	assert.NoError(t, f.srv.guardCredentialPlaintextDrop(ctx, flags.Change{Key: flagChunking, TenantID: flags.GlobalTenant, Enabled: true}))
}

func TestCredentialPlaintextDrop_WritersStopWritingPlaintext(t *testing.T) {
	f := setupLifecycleFixtureWith(t, keyTestMasterA, withPlaintextDrop(true))
	ctx := context.Background()

	// Registration: the primary and the mirror carry no plaintext.
	w, v, _ := f.wrappedRow("api_keys", "key_id", f.key.Key)
	require.NotEmpty(t, w)
	require.NotNil(t, v)
	assert.Nil(t, f.plaintextOf("api_keys", "key_id", f.key.Key), "registration drops the plaintext")
	assert.Nil(t, f.mirrorOf(), "and the tenants mirror")
	assert.Equal(t, "", f.put(f.key.Key, f.key.Secret, "dropped.txt"), "the wrapped form signs")

	// A new key, a rotation (and its mirror), an STS token.
	extra, err := f.svc.GenerateAPIKey(ctx, f.user.ID, "extra", nil)
	require.NoError(t, err)
	assert.Nil(t, f.plaintextOf("api_keys", "key_id", extra.Key))
	rotated, err := f.svc.RotateAPIKey(ctx, f.user.TenantID, f.key.ID)
	require.NoError(t, err)
	assert.Nil(t, f.plaintextOf("api_keys", "key_id", rotated.Key))
	assert.Nil(t, f.mirrorOf(), "the rotation's mirror")
	sts, err := auth.GenerateSTSToken(ctx, f.db, f.srv.credentialKeys(), f.tenant.ID, rotated.Key, &auth.KeyScope{Permissions: []string{"*"}}, auth.STSRequest{TTL: 600})
	require.NoError(t, err)
	assert.Nil(t, f.plaintextOf("sts_tokens", "access_key", sts.AccessKey))
	for _, pair := range [][2]string{{extra.Key, extra.Secret}, {rotated.Key, rotated.Secret}, {sts.AccessKey, sts.SecretKey}} {
		assert.Equal(t, "", f.put(pair[0], pair[1], "dropped.txt"), pair[0])
	}

	// The flag off again: the writers write the plaintext again.
	f.srv.flags = nil
	again, err := f.svc.RotateAPIKey(ctx, f.user.TenantID, extra.ID)
	require.NoError(t, err)
	assert.NotNil(t, f.plaintextOf("api_keys", "key_id", again.Key))
}

// A server with the flag on but no master key (a slot booted without its
// credential) never stores a secret nowhere: the plaintext is written.
func TestCredentialPlaintextDrop_NoKeysStillWritesThePlaintext(t *testing.T) {
	f := setupLifecycleFixtureWith(t, "", withPlaintextDrop(true))
	assert.NotNil(t, f.plaintextOf("api_keys", "key_id", f.key.Key))
	assert.NotNil(t, f.mirrorOf())
	assert.Equal(t, "", f.put(f.key.Key, f.key.Secret, "plain.txt"))
}

func TestCredentialPlaintextDrop_NullsOnlyRowsThatOpen(t *testing.T) {
	f := setupLifecycleFixtureWith(t, keyTestMasterA) // the flag off: plaintext + wrap
	ctx := context.Background()
	extra, err := f.svc.GenerateAPIKey(ctx, f.user.ID, "extra", nil)
	require.NoError(t, err)
	// A live row the job has not wrapped yet (written by an older build).
	f.exec(`UPDATE api_keys SET secret_wrapped = NULL, secret_key_version = NULL WHERE key_id = $1`, f.key.Key)
	// A damaged wrap: its plaintext must stay (the row already answers 500).
	f.exec(`UPDATE api_keys SET secret_wrapped = set_byte(secret_wrapped, 20, get_byte(secret_wrapped, 20) # 1) WHERE key_id = $1`, extra.Key)
	// Dead rows: a revoked key and an expired token, plaintext only.
	revokedID := "VKREVOKED" + f.tenant.ID[7:15]
	f.exec(`INSERT INTO api_keys (user_id, tenant_id, name, key_id, secret_hash, secret_key, revoked_at)
	        VALUES ($1, $2, 'revoked', $3, 'x', 'revoked-secret', NOW())`, f.user.ID, f.tenant.ID, revokedID)
	expiredID := "ASIAEXPIRED" + f.tenant.ID[7:15]
	f.exec(`INSERT INTO sts_tokens (access_key, secret_key, tenant_id, parent_key_id, expires_at)
	        VALUES ($1, 'expired-secret', $2, $3, NOW() - INTERVAL '1 minute')`, expiredID, f.tenant.ID, f.key.Key)

	k := f.credentialRunner(f.srv.tenantKeys)
	dropOn := false
	k.dropPlaintext = func() bool { return dropOn }

	// The flag off: the job wraps, nulls nothing.
	res, err := k.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.CredentialsWrapped, "the primary")
	assert.Equal(t, 0, res.CredentialsPlaintextDropped)
	assert.NotNil(t, f.plaintextOf("api_keys", "key_id", f.key.Key))

	// The flag on: every row whose wrap opens back to its plaintext, and
	// every dead row, loses the plaintext; the damaged one keeps it.
	dropOn = true
	res, err = k.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, res.CredentialsPlaintextDropped, "the primary, the revoked key, the expired token")
	assert.Equal(t, 1, res.CredentialFailures, "the damaged wrap")
	assert.Equal(t, 1, res.MirrorsPlaintextDropped)
	assert.Nil(t, f.plaintextOf("api_keys", "key_id", f.key.Key))
	assert.Nil(t, f.plaintextOf("api_keys", "key_id", revokedID))
	assert.Nil(t, f.plaintextOf("sts_tokens", "access_key", expiredID))
	assert.Nil(t, f.mirrorOf())
	kept := f.plaintextOf("api_keys", "key_id", extra.Key)
	require.NotNil(t, kept, "a wrap that does not open never costs its plaintext")
	assert.Equal(t, extra.Secret, *kept)

	assert.Equal(t, "", f.put(f.key.Key, f.key.Secret, "after-drop.txt"))

	// A plaintext that differs from what the wrap opens to is kept too.
	f.exec(`UPDATE api_keys SET secret_key = 'not-the-wrapped-secret' WHERE key_id = $1`, f.key.Key)
	res, err = k.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, res.CredentialsPlaintextDropped)
	assert.NotNil(t, f.plaintextOf("api_keys", "key_id", f.key.Key))
}

// Plan 43.7's own test: after the drop, a dump of the database holds no
// usable S3 secret of the tenant; with the master key the request signs,
// without it the server refuses (500), never in the clear.
func TestDump_RestoredWithoutTheMasterKeyHasNoUsableSecret(t *testing.T) {
	f := setupLifecycleFixtureWith(t, keyTestMasterA, withPlaintextDrop(true))
	ctx := context.Background()
	extra, err := f.svc.GenerateAPIKey(ctx, f.user.ID, "extra", nil)
	require.NoError(t, err)
	sts, err := auth.GenerateSTSToken(ctx, f.db, f.srv.credentialKeys(), f.tenant.ID, f.key.Key, &auth.KeyScope{Permissions: []string{"*"}}, auth.STSRequest{TTL: 600})
	require.NoError(t, err)
	require.Equal(t, "", f.put(f.key.Key, f.key.Secret, "dump.txt"))
	k := f.credentialRunner(f.srv.tenantKeys)
	k.dropPlaintext = func() bool { return true }
	_, err = k.RunOnce(ctx)
	require.NoError(t, err)
	secrets := []string{f.key.Secret, extra.Secret, sts.SecretKey}

	// The dump as text: every row of every table in the schema.
	var tables []string
	rows, err := f.db.Query(`SELECT quote_ident(table_name) FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	require.NoError(t, err)
	for rows.Next() {
		var tbl string
		require.NoError(t, rows.Scan(&tbl))
		tables = append(tables, tbl)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Greater(t, len(tables), 50)
	for _, tbl := range tables {
		for _, s := range secrets {
			var hits int
			require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM `+tbl+` t WHERE strpos(t::text, $1) > 0`, s).Scan(&hits), tbl)
			assert.Zero(t, hits, "table %s holds a secret in the clear", tbl)
		}
	}
	// And pg_dump itself, when this machine has one that can read the server.
	if path, err := exec.LookPath("pg_dump"); err == nil {
		out, err := exec.Command(path, "--data-only", "--table=api_keys", "--table=sts_tokens", "--table=tenants", "--table=audit_logs", testutil.DSN()).Output()
		if err != nil {
			t.Logf("pg_dump could not dump this server (%v): the table walk above is the proof", err)
		} else {
			for _, s := range secrets {
				assert.NotContains(t, string(out), s, "pg_dump holds a secret in the clear")
			}
		}
	}

	// With the master key the request signs; a server restored without it
	// cannot open the secret and says so (500), never signs in the clear.
	assert.Equal(t, "", f.put(f.key.Key, f.key.Secret, "dump-after.txt"))
	f.srv.tenantKeys = nil
	assert.Equal(t, ErrInternalError, f.put(f.key.Key, f.key.Secret, "nokey.txt"))
	assert.Equal(t, ErrInternalError, f.put(sts.AccessKey, sts.SecretKey, "nokey.txt"))
}
