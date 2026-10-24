package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/recovery"
	"github.com/FairForge/vaultaire/internal/recovery/client"
	"github.com/FairForge/vaultaire/internal/recovery/realm"
)

// RP52-3 (the WP-P37 review): an account erasure reaches the recovery
// realms. Before it, the runner never talked to a realm, so an erased user's
// key shares and encrypted secret stayed at every realm for ever under the
// HMAC subject.
//
// The realms here are internal/recovery/realm behind a minimal Noise NK
// /req endpoint (the handshake path a Delete rides — HTTPTransport, the
// probe's § Noise), so the runner's HTTP transport and token minting are
// the real ones. The realm package is linked into this test only, never
// into cmd/vaultaire (TestRecoveryRealm_NotLinkedIntoVaultaire).

const testRealmTenant = "stored"

type httpTestRealm struct {
	id             recovery.RealmID
	secret, public [32]byte
	tokenKey       []byte
	store          *realm.MemoryStore
	realm          *realm.Realm
	srv            *httptest.Server
	down           atomic.Bool  // answer Unavailable (503), as a realm whose store fails
	deletes        atomic.Int32 // Delete requests answered OK
	lifetime       atomic.Int64 // exp - nbf of the last token accepted
}

func startHTTPTestRealm(t *testing.T, idByte byte) *httpTestRealm {
	t.Helper()
	r := &httpTestRealm{store: realm.NewMemoryStore(), tokenKey: make([]byte, 32)}
	for i := range r.id {
		r.id[i] = idByte
	}
	_, err := io.ReadFull(rand.Reader, r.secret[:])
	require.NoError(t, err)
	_, err = io.ReadFull(rand.Reader, r.tokenKey)
	require.NoError(t, err)
	r.public, err = recovery.NoisePublicKey(r.secret)
	require.NoError(t, err)
	r.realm = &realm.Realm{Store: r.store}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *httpTestRealm) reply(w http.ResponseWriter, code int, resp recovery.ClientResponse) {
	body, _ := resp.MarshalCBOR()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// serve answers a SecretsRequest that rides the handshake (Delete,
// Register1, Recover1), the way cmd/recovery-realm's handshake path does.
func (r *httpTestRealm) serve(w http.ResponseWriter, hr *http.Request) {
	if r.down.Load() {
		r.reply(w, http.StatusServiceUnavailable, recovery.ClientResponse{Status: recovery.ClientUnavailable})
		return
	}
	raw, _ := io.ReadAll(hr.Body)
	req, err := recovery.UnmarshalClientRequest(raw)
	if err != nil || req.Handshake == nil || req.Kind != recovery.KindSecretsRequest || req.Realm != r.id {
		r.reply(w, http.StatusBadRequest, recovery.ClientResponse{Status: recovery.ClientDecodingError})
		return
	}
	who, err := r.validate(req.AuthToken)
	if err != nil {
		r.reply(w, http.StatusUnauthorized, recovery.ClientResponse{Status: recovery.ClientInvalidAuth})
		return
	}
	hs, payload, err := recovery.NoiseServerStart(rand.Reader, r.secret, r.public, *req.Handshake)
	if err != nil {
		r.reply(w, http.StatusBadRequest, recovery.ClientResponse{Status: recovery.ClientSessionError})
		return
	}
	sreq, err := recovery.UnmarshalSecretsRequest(payload)
	if err != nil || sreq.NeedsForwardSecrecy() {
		r.reply(w, http.StatusBadRequest, recovery.ClientResponse{Status: recovery.ClientDecodingError})
		return
	}
	resp, err := r.realm.Handle(hr.Context(), who, sreq)
	if err != nil {
		r.reply(w, http.StatusServiceUnavailable, recovery.ClientResponse{Status: recovery.ClientUnavailable})
		return
	}
	padded, err := resp.MarshalPadded()
	if err != nil {
		r.reply(w, http.StatusServiceUnavailable, recovery.ClientResponse{Status: recovery.ClientUnavailable})
		return
	}
	_, hsResp, err := hs.Finish(padded)
	if err != nil {
		r.reply(w, http.StatusBadRequest, recovery.ClientResponse{Status: recovery.ClientSessionError})
		return
	}
	if sreq.Op == recovery.OpDelete {
		r.deletes.Add(1)
	}
	r.reply(w, http.StatusOK, recovery.ClientResponse{Handshake: &hsResp, SessionLifetime: 5 * time.Minute})
}

// validate is the realm's token rule (cmd/recovery-realm/auth.go), short:
// HS256 under the tenant key, aud = this realm, scope user, ≤ 24 h.
func (r *httpTestRealm) validate(raw string) (realm.Auth, error) {
	var c struct {
		jwt.RegisteredClaims
		Scope string `json:"scope"`
	}
	_, err := jwt.NewParser(jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(r.id.String()),
		jwt.WithExpirationRequired()).ParseWithClaims(raw, &c, func(tok *jwt.Token) (any, error) {
		if tok.Header["kid"] != testRealmTenant+":1" {
			return nil, fmt.Errorf("unknown kid %v", tok.Header["kid"])
		}
		return r.tokenKey, nil
	})
	switch {
	case err != nil:
		return realm.Auth{}, err
	case c.Issuer != testRealmTenant || c.Subject == "" || c.Scope != "user":
		return realm.Auth{}, fmt.Errorf("claims")
	case c.NotBefore == nil || c.ExpiresAt.Sub(c.NotBefore.Time) > 24*time.Hour:
		return realm.Auth{}, fmt.Errorf("lifetime")
	}
	r.lifetime.Store(int64(c.ExpiresAt.Sub(c.NotBefore.Time)))
	return realm.Auth{Tenant: c.Issuer, Subject: c.Subject}, nil
}

// holds reports whether the realm still keeps secret material for sub.
func (r *httpTestRealm) holds(t *testing.T, sub string) bool {
	t.Helper()
	rec, _, err := r.store.Load(context.Background(), testRealmTenant, sub)
	require.NoError(t, err)
	return rec.State != realm.NotRegistered || rec.HoldsSecretMaterial()
}

// testRecoveryVault is the minter configured with the given realms, through
// loadRecoveryVault (the production parser).
func testRecoveryVault(t *testing.T, realms ...*httpTestRealm) *recoveryVault {
	t.Helper()
	var entries, keys []string
	for _, r := range realms {
		entries = append(entries, fmt.Sprintf(`{"id":%q,"address":%q,"public_key":%q,"kid":"%s:1"}`,
			r.id.String(), r.srv.URL, hex.EncodeToString(r.public[:]), testRealmTenant))
		keys = append(keys, fmt.Sprintf(`%q: %q`, r.id.String(), hex.EncodeToString(r.tokenKey)))
	}
	path := filepath.Join(t.TempDir(), "realms.json")
	require.NoError(t, os.WriteFile(path, []byte("["+strings.Join(entries, ",")+"]"), 0o600))
	env := map[string]string{
		envRecoverySubjectKey: recoverySubjectKeyHex,
		envRecoveryTokenKeys:  "{" + strings.Join(keys, ",") + "}",
		envRecoveryRealmsFile: path,
	}
	v, err := loadRecoveryVault(func(k string) string { return env[k] }, os.ReadFile)
	require.NoError(t, err)
	require.NotNil(t, v)
	return v
}

// directRealms carries a request straight to realm.Handle: the test's way to
// REGISTER (Register2 needs an established session over HTTP).
type directRealms struct {
	sub    string
	realms map[recovery.RealmID]*realm.Realm
}

func (d directRealms) Send(ctx context.Context, rm client.Realm, req recovery.SecretsRequest) (recovery.SecretsResponse, error) {
	return d.realms[rm.ID].Handle(ctx, realm.Auth{Tenant: testRealmTenant, Subject: d.sub}, req)
}

// registerAt registers a secret for userID's subject at every realm.
func registerAt(t *testing.T, v *recoveryVault, userID string, realms ...*httpTestRealm) string {
	t.Helper()
	sub := v.subject(userID)
	d := directRealms{sub: sub, realms: map[recovery.RealmID]*realm.Realm{}}
	cfg := client.Config{RegisterThreshold: len(realms), RecoverThreshold: len(realms)/2 + 1,
		PINHashing: recovery.FastInsecure, Transport: d}
	for _, r := range realms {
		d.realms[r.id] = r.realm
		cfg.Realms = append(cfg.Realms, client.Realm{ID: r.id, Address: r.srv.URL, PublicKey: r.public[:]})
	}
	require.NoError(t, client.Register(context.Background(), cfg, []byte("2580"), []byte("the recovery key"), []byte(sub), 5))
	for _, r := range realms {
		require.True(t, r.holds(t, sub), "the fixture registered at realm %s", r.id)
	}
	return sub
}

func TestAccountDeletionRunner_ErasesTheUsersRecordAtEveryRealm(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	a, b := startHTTPTestRealm(t, 0x1a), startHTTPTestRealm(t, 0x1b)
	v := testRecoveryVault(t, a, b)
	sub := registerAt(t, v, f.userID, a, b)
	f.runner.Recovery = v

	res, err := f.runner.RunOnce(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Tenants, 1)
	te := res.Tenants[0]
	assert.Equal(t, outcomeErased, te.Outcome)
	assert.False(t, a.holds(t, sub), "realm a holds nothing for the subject")
	assert.False(t, b.holds(t, sub), "realm b holds nothing for the subject")
	assert.Equal(t, int32(1), a.deletes.Load())
	assert.Equal(t, int32(1), b.deletes.Load())
	assert.Equal(t, 2, te.RealmsDeleted)
	assert.Zero(t, te.RealmsDeferred)
	assert.Equal(t, int64(recoveryTokenLifetime), a.lifetime.Load(), "the erasure's token has the minter's life, never longer")

	var meta string
	require.NoError(t, f.db.QueryRow(`SELECT metadata::text FROM audit_logs WHERE tenant_id = $1 AND action = 'account.erased'`, f.tenantID).Scan(&meta))
	assert.Contains(t, meta, `"realms_deleted": 2`)
	assert.Contains(t, meta, `"realms_deferred": 0`)
	assert.NotContains(t, meta, sub, "the audit trail never carries the opaque subject")
}

// A realm that answers Unavailable defers the tenant (the run fails — the
// job's contract — and nothing of the rows goes); the other realm's record
// is deleted all the same; the next run, with the realm back, erases.
func TestAccountDeletionRunner_RealmDownDefersTheTenantAndRetries(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	a, b := startHTTPTestRealm(t, 0x2a), startHTTPTestRealm(t, 0x2b)
	v := testRecoveryVault(t, a, b)
	sub := registerAt(t, v, f.userID, a, b)
	f.runner.Recovery = v
	b.down.Store(true)

	res, err := f.runner.RunOnce(context.Background())
	require.Error(t, err, "a deferred tenant fails the run")
	require.Len(t, res.Tenants, 1)
	te := res.Tenants[0]
	assert.Equal(t, outcomeDeferred, te.Outcome)
	assert.Contains(t, te.Error, "recovery realms")
	assert.Contains(t, te.Error, b.id.String(), "the error names the realm")
	assert.Equal(t, 1, te.RealmsDeleted)
	assert.Equal(t, 1, te.RealmsDeferred)
	assert.False(t, a.holds(t, sub), "the reachable realm's record is deleted")
	assert.True(t, b.holds(t, sub), "the down realm still holds it")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1`, f.userID), "rows not erased while a realm may hold the record")
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'account.erased'`, f.tenantID))

	b.down.Store(false)
	res, err = f.runner.RunOnce(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Tenants, 1)
	assert.Equal(t, outcomeErased, res.Tenants[0].Outcome)
	assert.Equal(t, 2, res.Tenants[0].RealmsDeleted, "realm a's NotRegistered counts as done")
	assert.False(t, b.holds(t, sub))
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1`, f.userID))
}

// A realm nobody answers at (the box is down) defers like a 5xx.
func TestAccountDeletionRunner_UnreachableRealmDefers(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	a := startHTTPTestRealm(t, 0x3a)
	v := testRecoveryVault(t, a)
	registerAt(t, v, f.userID, a)
	f.runner.Recovery = v
	a.srv.Close()

	res, err := f.runner.RunOnce(context.Background())
	require.Error(t, err)
	require.Len(t, res.Tenants, 1)
	assert.Equal(t, outcomeDeferred, res.Tenants[0].Outcome)
	assert.Equal(t, 1, res.Tenants[0].RealmsDeferred)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id::text = $1`, f.userID))
}

// A realm token minted in the last lifetime may still Register after the
// runner's Delete: the tenant waits for the next run.
func TestAccountDeletionRunner_RecentRealmTokenDefers(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	a := startHTTPTestRealm(t, 0x4a)
	f.runner.Recovery = testRecoveryVault(t, a)
	f.exec(`INSERT INTO audit_logs (user_id, tenant_id, event_type, action, result) VALUES ($1, $2, 'recovery', 'recovery.tokens_issued', 'success')`, f.userID, f.tenantID)

	res, err := f.runner.RunOnce(context.Background())
	require.Error(t, err)
	require.Len(t, res.Tenants, 1)
	assert.Equal(t, outcomeDeferred, res.Tenants[0].Outcome)
	assert.Contains(t, res.Tenants[0].Error, "realm token")
	assert.Zero(t, a.deletes.Load(), "no realm asked while a token may still register")

	f.exec(`UPDATE audit_logs SET timestamp = NOW() - INTERVAL '11 minutes' WHERE user_id = $1 AND action = 'recovery.tokens_issued'`, f.userID)
	res, err = f.runner.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, outcomeErased, res.Tenants[0].Outcome)
}

// Not configured: no realm can be asked; the erasure goes on and the run
// says so in its note.
func TestAccountDeletionRunner_RecoveryNotConfiguredIsANote(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	require.Nil(t, f.runner.Recovery)

	res, err := f.runner.RunOnce(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Tenants, 1)
	assert.Equal(t, outcomeErased, res.Tenants[0].Outcome)
	assert.True(t, res.Tenants[0].RealmsNotConfigured)
	require.NotEmpty(t, res.Notes)
	assert.Contains(t, strings.Join(res.Notes, "; "), "recovery vault not configured")

	var meta string
	require.NoError(t, f.db.QueryRow(`SELECT metadata::text FROM audit_logs WHERE tenant_id = $1 AND action = 'account.erased'`, f.tenantID).Scan(&meta))
	assert.Contains(t, meta, `"realms_not_configured": true`)
}
