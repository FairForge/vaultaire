package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/recovery"
	"github.com/FairForge/vaultaire/internal/recovery/client"
	"github.com/FairForge/vaultaire/internal/recovery/realm"
	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

var tenantKey = bytes.Repeat([]byte{0x5a}, 32)

type testRealm struct {
	srv     *server
	http    *httptest.Server
	store   *realm.BoltStore
	reg     *prometheus.Registry
	cfg     client.Realm
	idHex   string
	destroy func() float64
}

func startRealm(t *testing.T, idByte byte) *testRealm {
	t.Helper()
	var secretKey [32]byte
	_, err := rand.Read(secretKey[:])
	require.NoError(t, err)
	c, err := loadConfig(func(k string) string {
		return map[string]string{
			"RECOVERY_REALM_ID":      hex.EncodeToString(bytes.Repeat([]byte{idByte}, 16)),
			"RECOVERY_REALM_DB":      filepath.Join(t.TempDir(), "realm.db"),
			"RECOVERY_REALM_KEY":     hex.EncodeToString(secretKey[:]),
			"RECOVERY_REALM_TENANTS": fmt.Sprintf(`{"stored": {"1": %q}}`, hex.EncodeToString(tenantKey)),
		}[k]
	}, func(string) ([]byte, error) { return nil, fmt.Errorf("no files") })
	require.NoError(t, err)
	store, err := realm.OpenBoltStore(c.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	reg := prometheus.NewRegistry()
	srv, err := newServer(c, store, reg, zap.NewNop())
	require.NoError(t, err)
	hs := httptest.NewServer(srv.routes())
	t.Cleanup(hs.Close)
	return &testRealm{srv: srv, http: hs, store: store, reg: reg, idHex: c.id.String(),
		cfg:     client.Realm{ID: c.id, Address: hs.URL + "/", PublicKey: append([]byte(nil), srv.staticPublic[:]...)},
		destroy: func() float64 { return testutil.ToFloat64(srv.metrics.destroyed) }}
}

type claimsOpt func(jwt.MapClaims, map[string]any)

func token(t *testing.T, aud string, opts ...claimsOpt) string {
	t.Helper()
	now := time.Now()
	c := jwt.MapClaims{"iss": "stored", "sub": "opaque-sub", "aud": aud, "scope": "user",
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(10 * time.Minute).Unix()}
	h := map[string]any{"kid": "stored:1"}
	for _, o := range opts {
		o(c, h)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	for k, v := range h {
		tok.Header[k] = v
	}
	s, err := tok.SignedString(tenantKey)
	require.NoError(t, err)
	return s
}

// handshakeOnly posts a valid HandshakeOnly envelope carrying tok.
func handshakeOnly(t *testing.T, r *testRealm, tok string) (int, recovery.ClientResponse) {
	t.Helper()
	_, msg, err := recovery.NoiseClientStart(rand.Reader, r.srv.staticPublic, nil)
	require.NoError(t, err)
	body, err := recovery.ClientRequest{Realm: r.srv.id, AuthToken: tok, SessionID: 1, Kind: recovery.KindHandshakeOnly, Handshake: &msg}.MarshalCBOR()
	require.NoError(t, err)
	resp, err := http.Post(r.http.URL+"/req", "application/octet-stream", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	cr, err := recovery.UnmarshalClientResponse(raw)
	require.NoError(t, err)
	return resp.StatusCode, cr
}

func assertRefused(t *testing.T, r *testRealm, tok string) {
	t.Helper()
	before := testutil.ToFloat64(r.srv.metrics.requests.WithLabelValues("unknown", "invalid_auth"))
	code, resp := handshakeOnly(t, r, tok)
	assert.Equal(t, http.StatusUnauthorized, code)
	assert.Equal(t, recovery.ClientInvalidAuth, resp.Status, "InvalidAuthentication")
	assert.Equal(t, before+1, testutil.ToFloat64(r.srv.metrics.requests.WithLabelValues("unknown", "invalid_auth")))
}

func TestRecoveryRealm_AcceptsAValidToken(t *testing.T) {
	r := startRealm(t, 1)
	code, resp := handshakeOnly(t, r, token(t, r.idHex))
	assert.Equal(t, http.StatusOK, code)
	require.Equal(t, recovery.ClientOK, resp.Status)
	assert.NotNil(t, resp.Handshake)
	assert.Equal(t, sessionLifetime, resp.SessionLifetime)
}

func TestRecoveryRealm_AuthRejectsMissingToken(t *testing.T) {
	r := startRealm(t, 1)
	assertRefused(t, r, "")
}

func TestRecoveryRealm_AuthRejectsWrongAud(t *testing.T) {
	r := startRealm(t, 1)
	other := startRealm(t, 2)
	assertRefused(t, r, token(t, other.idHex)) // a token for another realm
	assertRefused(t, r, token(t, r.idHex, func(c jwt.MapClaims, _ map[string]any) { c["aud"] = []string{r.idHex, other.idHex} }))
}

func TestRecoveryRealm_AuthRejectsWideWindow(t *testing.T) {
	r := startRealm(t, 1)
	assertRefused(t, r, token(t, r.idHex, func(c jwt.MapClaims, _ map[string]any) {
		c["exp"] = time.Now().Add(86401 * time.Second).Unix() // exp − nbf = 86 401 s
	}))
	// 86 400 exactly is the limit, allowed.
	code, _ := handshakeOnly(t, r, token(t, r.idHex, func(c jwt.MapClaims, _ map[string]any) {
		c["exp"] = c["nbf"].(int64) + 86400
	}))
	assert.Equal(t, http.StatusOK, code)
	assertRefused(t, r, token(t, r.idHex, func(c jwt.MapClaims, _ map[string]any) { delete(c, "nbf") }))
}

func TestRecoveryRealm_AuthRejectsUnknownKid(t *testing.T) {
	r := startRealm(t, 1)
	assertRefused(t, r, token(t, r.idHex, func(_ jwt.MapClaims, h map[string]any) { h["kid"] = "stored:2" }))
	assertRefused(t, r, token(t, r.idHex, func(c jwt.MapClaims, h map[string]any) { h["kid"] = "acme:1"; c["iss"] = "acme" }))
	assertRefused(t, r, token(t, r.idHex, func(_ jwt.MapClaims, h map[string]any) { h["kid"] = "stored" }))
}

func TestRecoveryRealm_AuthRejectsTheRest(t *testing.T) {
	r := startRealm(t, 1)
	for name, opt := range map[string]claimsOpt{
		"expired": func(c jwt.MapClaims, _ map[string]any) {
			c["nbf"] = time.Now().Add(-2 * time.Hour).Unix()
			c["exp"] = time.Now().Add(-time.Hour).Unix()
		},
		"not yet valid":      func(c jwt.MapClaims, _ map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() },
		"no exp":             func(c jwt.MapClaims, _ map[string]any) { delete(c, "exp") },
		"iss is not the kid": func(c jwt.MapClaims, _ map[string]any) { c["iss"] = "acme" },
		"no sub":             func(c jwt.MapClaims, _ map[string]any) { delete(c, "sub") },
		"no scope":           func(c jwt.MapClaims, _ map[string]any) { delete(c, "scope") },
		"audit scope":        func(c jwt.MapClaims, _ map[string]any) { c["scope"] = "audit" },
	} {
		t.Run(name, func(t *testing.T) { assertRefused(t, r, token(t, r.idHex, opt)) })
	}
	t.Run("wrong key", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "stored", "sub": "x", "aud": r.idHex, "scope": "user",
			"nbf": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
		tok.Header["kid"] = "stored:1"
		s, err := tok.SignedString(bytes.Repeat([]byte{1}, 32))
		require.NoError(t, err)
		assertRefused(t, r, s)
	})
	t.Run("alg none", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"iss": "stored", "sub": "x", "aud": r.idHex, "scope": "user",
			"nbf": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
		tok.Header["kid"] = "stored:1"
		s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
		require.NoError(t, err)
		assertRefused(t, r, s)
	})
}

func networkConfig(rs ...*testRealm) client.Config {
	realms := make([]client.Realm, len(rs))
	byID := map[recovery.RealmID]string{}
	for i, r := range rs {
		realms[i] = r.cfg
		byID[r.cfg.ID] = r.idHex
	}
	tokens := func(_ context.Context, r client.Realm) (string, error) {
		now := time.Now()
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "stored", "sub": "opaque-sub", "aud": byID[r.ID],
			"scope": "user", "nbf": now.Unix(), "exp": now.Add(10 * time.Minute).Unix()})
		tok.Header["kid"] = "stored:1"
		return tok.SignedString(tenantKey)
	}
	return client.Config{Realms: realms, RegisterThreshold: len(rs), RecoverThreshold: len(rs)/2 + 1,
		PINHashing: recovery.FastInsecure, Transport: &client.HTTPTransport{Token: tokens}}
}

func TestRecoveryRealm_EndToEnd_OverNoise(t *testing.T) {
	ctx := context.Background()
	r1, r2 := startRealm(t, 1), startRealm(t, 2)
	cfg := networkConfig(r1, r2)
	pin, secretBytes, info := []byte("2580"), []byte("opaque recovery key"), []byte("opaque-sub")

	require.NoError(t, client.Register(ctx, cfg, pin, secretBytes, info, 2))
	_, err := client.Recover(ctx, cfg, []byte("0000"), info)
	var bad client.ErrInvalidPin
	require.ErrorAs(t, err, &bad)
	assert.Equal(t, uint16(1), bad.GuessesRemaining)
	got, err := client.Recover(ctx, cfg, pin, info)
	require.NoError(t, err)
	assert.Equal(t, secretBytes, got)

	// Burn both guesses, then the right PIN: destroyed, on both realms.
	for i := 0; i < 2; i++ {
		_, err = client.Recover(ctx, cfg, []byte("0000"), info)
		require.ErrorAs(t, err, &bad)
	}
	_, err = client.Recover(ctx, cfg, pin, info)
	assert.ErrorIs(t, err, client.ErrNoGuesses)
	for _, r := range []*testRealm{r1, r2} {
		rec, _, err := r.store.Load(ctx, "stored", "opaque-sub")
		require.NoError(t, err)
		assert.Equal(t, realm.NoGuesses, rec.State)
		assert.False(t, rec.HoldsSecretMaterial())
		assert.Equal(t, float64(1), r.destroy())
		assert.Equal(t, float64(5), testutil.ToFloat64(r.srv.metrics.requests.WithLabelValues("recover1", "ok"))+
			testutil.ToFloat64(r.srv.metrics.requests.WithLabelValues("recover1", "no_guesses")))
	}
	require.NoError(t, client.Delete(ctx, cfg))
	_, err = client.Recover(ctx, cfg, pin, info)
	assert.ErrorIs(t, err, client.ErrNotRegistered)
}

func TestRecoveryRealm_NeverAcceptsAGuessUnderTheStaticKeyAlone(t *testing.T) {
	r := startRealm(t, 1)
	_, blinded, err := recovery.OPRFBlind(rand.Reader, []byte("guess"))
	require.NoError(t, err)
	payload, err := recovery.SecretsRequest{Op: recovery.OpRecover2, Recover2: &recovery.Recover2Request{BlindedInput: blinded}}.MarshalCBOR()
	require.NoError(t, err)
	_, msg, err := recovery.NoiseClientStart(rand.Reader, r.srv.staticPublic, payload)
	require.NoError(t, err)
	body, err := recovery.ClientRequest{Realm: r.srv.id, AuthToken: token(t, r.idHex), SessionID: 9, Kind: recovery.KindSecretsRequest, Handshake: &msg}.MarshalCBOR()
	require.NoError(t, err)
	resp, err := http.Post(r.http.URL+"/req", "application/octet-stream", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	cr, err := recovery.UnmarshalClientResponse(raw)
	require.NoError(t, err)
	assert.Equal(t, recovery.ClientDecodingError, cr.Status, "a Recover2 needs forward secrecy (requests.rs:147)")
}

func TestRecoveryRealm_MetricsPrecreatedAtZero(t *testing.T) {
	r := startRealm(t, 1)
	mfs, err := r.reg.Gather()
	require.NoError(t, err)
	series := 0
	for _, mf := range mfs {
		if mf.GetName() == "recovery_realm_requests_total" {
			series = len(mf.GetMetric())
		}
	}
	assert.Equal(t, len(ops)*len(outcomes), series, "every op × outcome exported before any request")
	assert.Equal(t, float64(0), r.destroy())
}

func TestRecoveryRealm_Config(t *testing.T) {
	base := map[string]string{
		"RECOVERY_REALM_ID":      strings.Repeat("ab", 16),
		"RECOVERY_REALM_DB":      "/tmp/x.db",
		"RECOVERY_REALM_KEY":     strings.Repeat("11", 32),
		"RECOVERY_REALM_TENANTS": `{"stored": {"1": "` + strings.Repeat("22", 32) + `"}}`,
	}
	load := func(over map[string]string) error {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		_, err := loadConfig(func(k string) string { return m[k] }, func(string) ([]byte, error) { return nil, fmt.Errorf("no") })
		return err
	}
	require.NoError(t, load(nil))
	for name, over := range map[string]map[string]string{
		"no realm id":         {"RECOVERY_REALM_ID": ""},
		"no db":               {"RECOVERY_REALM_DB": ""},
		"no key":              {"RECOVERY_REALM_KEY": ""},
		"key not hex":         {"RECOVERY_REALM_KEY": "zz-not-a-key-zz"},
		"key too short":       {"RECOVERY_REALM_KEY": "1111"},
		"key file unreadable": {"RECOVERY_REALM_KEY": "", "RECOVERY_REALM_KEY_FILE": "/run/missing"},
		"tenants not json":    {"RECOVERY_REALM_TENANTS": "stored=abc"},
		"tenant key short":    {"RECOVERY_REALM_TENANTS": `{"stored": {"1": "abcd"}}`},
		"tenant name":         {"RECOVERY_REALM_TENANTS": `{"st ored": {"1": "` + strings.Repeat("22", 32) + `"}}`},
		"public without tls":  {"RECOVERY_REALM_LISTEN": "0.0.0.0:8780"},
		"tls half":            {"RECOVERY_REALM_TLS_CERT": "/c.pem"},
	} {
		t.Run(name, func(t *testing.T) {
			err := load(over)
			require.Error(t, err)
			for _, secretValue := range []string{strings.Repeat("11", 32), strings.Repeat("22", 32), "zz-not-a-key-zz"} {
				assert.NotContains(t, err.Error(), secretValue, "an error never quotes a key")
			}
		})
	}
	require.NoError(t, load(map[string]string{"RECOVERY_REALM_LISTEN": "0.0.0.0:8780", "RECOVERY_REALM_TLS_CERT": "/c", "RECOVERY_REALM_TLS_KEY": "/k"}))
}

// A realm belongs on another box: nothing of it may be linked into the
// product binary (cmd/tools/README.md's rule, the go list check).
func TestRecoveryRealm_NotLinkedIntoVaultaire(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "../vaultaire").Output()
	require.NoError(t, err)
	deps := string(out)
	require.Contains(t, deps, "github.com/FairForge/vaultaire/internal/api", "the listing is real")
	for _, banned := range []string{
		"github.com/FairForge/vaultaire/internal/recovery/realm",
		"github.com/FairForge/vaultaire/cmd/recovery-realm",
		"go.etcd.io/bbolt",
	} {
		for _, line := range strings.Split(deps, "\n") {
			assert.NotEqual(t, banned, strings.TrimSpace(line), "cmd/vaultaire links %s", banned)
		}
	}
}

// A realm that restarted (or expired the session) answers MissingSession;
// the client opens a new session and the request goes through.
func TestRecoveryRealm_LostSessionsAreReopened(t *testing.T) {
	ctx := context.Background()
	r1, r2 := startRealm(t, 1), startRealm(t, 2)
	cfg := networkConfig(r1, r2)
	require.NoError(t, client.Register(ctx, cfg, []byte("2580"), []byte("s"), []byte("u"), 5))
	for _, r := range []*testRealm{r1, r2} {
		r.srv.mu.Lock()
		clear(r.srv.sessions)
		r.srv.mu.Unlock()
	}
	got, err := client.Recover(ctx, cfg, []byte("2580"), []byte("u"))
	require.NoError(t, err)
	assert.Equal(t, []byte("s"), got)
	assert.Positive(t, testutil.ToFloat64(r1.srv.metrics.requests.WithLabelValues("unknown", "missing_session")))
}

// The enrolment body is a key_ceremony the transparency log accepts as is:
// decoded exactly as POST /api/v1/admin/translog/ceremony decodes it (an
// unknown field — the removed free-text statement — refused), then the
// log's own validation (RP37-10: no free text in a ceremony).
func TestRecoveryRealm_EnrolmentIsAValidCeremonyEntry(t *testing.T) {
	r := startRealm(t, 0x3c)
	c := config{id: r.srv.id, staticSecret: r.srv.staticSecret}
	body, err := enrolment(c)
	require.NoError(t, err)
	raw, err := json.Marshal(body)
	require.NoError(t, err)

	var entry translog.KeyCeremony
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&entry), "the ceremony route's decoder: %s", raw)
	_, err = translog.Encode(entry, time.Now())
	require.NoError(t, err, "the log's own validation")

	fp := sha256.Sum256(r.srv.staticPublic[:])
	assert.Equal(t, "realm_enrolled", entry.Action)
	assert.Equal(t, "recovery_realm_key", entry.KeyRole)
	assert.Equal(t, "sha256:"+hex.EncodeToString(fp[:]), entry.KeyFingerprint, "a reader hashes the pinned Noise key")
}
