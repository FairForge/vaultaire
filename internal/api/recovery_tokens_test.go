package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/config"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	dashmw "github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/flags"
)

var (
	recoverySubjectKeyHex = strings.Repeat("a1", 32)
	recoveryRealm1        = strings.Repeat("0a", 16)
	recoveryRealm2        = strings.Repeat("0b", 16)
	recoveryTokenKey1     = strings.Repeat("c1", 32)
	recoveryTokenKey2     = strings.Repeat("c2", 32)
)

// recoveryEnv is a complete configuration: two realms in a file, their
// token keys and the subject key in variables.
func recoveryEnv(t *testing.T) (func(string) string, func(string) ([]byte, error)) {
	t.Helper()
	dir := t.TempDir()
	realms := fmt.Sprintf(`[{"id":%q,"address":"https://realm-a.example","public_key":%q,"kid":"stored:1"},
		{"id":%q,"address":"https://realm-b.example/","public_key":%q,"kid":"storedcloud:7"}]`,
		recoveryRealm1, strings.Repeat("e1", 32), recoveryRealm2, strings.Repeat("e2", 32))
	path := filepath.Join(dir, "realms.json")
	require.NoError(t, os.WriteFile(path, []byte(realms), 0o600))
	env := map[string]string{
		envRecoverySubjectKey: recoverySubjectKeyHex,
		envRecoveryTokenKeys:  fmt.Sprintf(`{%q: %q, %q: %q}`, recoveryRealm1, recoveryTokenKey1, recoveryRealm2, recoveryTokenKey2),
		envRecoveryRealmsFile: path,
	}
	return func(k string) string { return env[k] }, os.ReadFile
}

type recoveryFixture struct {
	s     *Server
	db    *jobsFixture
	fl    *flags.Service
	user  *auth.User
	jwt   string
	other *auth.User
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	t.Setenv("JWT_SECRET", "recovery-test-secret")
	jf := setupJobsFixture(t)
	fl := flags.New(jf.db, zap.NewNop())
	registerFlags(fl)
	authSvc := auth.NewAuthService(nil, jf.db)
	f := &recoveryFixture{db: jf, fl: fl}
	mkUser := func() *auth.User {
		u := &auth.User{ID: uuid.New().String(), Email: fmt.Sprintf("rv-%d@test.local", time.Now().UnixNano()), TenantID: "tenant-" + uuid.New().String()}
		_, err := jf.db.Exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, u.ID, u.Email)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = jf.db.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, u.ID)
			_, _ = jf.db.Exec(`DELETE FROM users WHERE id = $1`, u.ID)
		})
		return u
	}
	f.user, f.other = mkUser(), mkUser()
	var err error
	f.jwt, err = authSvc.GenerateJWT(f.user)
	require.NoError(t, err)

	getenv, readFile := recoveryEnv(t)
	v, err := loadRecoveryVault(getenv, readFile)
	require.NoError(t, err)
	require.NotNil(t, v)
	f.s = &Server{logger: zap.NewNop(), router: chi.NewRouter(), db: jf.db, flags: fl, auth: authSvc,
		config: &config.Config{}, sessionStore: dashauth.NewMemoryStore(), recovery: v}
	f.s.registerRecoveryRoutes()
	return f
}

func (f *recoveryFixture) open(t *testing.T) {
	t.Helper()
	// Global ON modelled by this fixture's own default: never a '*' row of a
	// real flag on the shared test database (internal/flags/CLAUDE.md).
	f.fl.Register(flagRecoveryVault, true)
}

func (f *recoveryFixture) post(bearer string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recovery/tokens", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for _, m := range mod {
		m(req)
	}
	w := httptest.NewRecorder()
	f.s.router.ServeHTTP(w, req)
	return w
}

type mintedRealm struct {
	ID, Address, PublicKey, Token string
}

func decodeMinted(t *testing.T, w *httptest.ResponseRecorder) []mintedRealm {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		Realms []struct {
			ID        string `json:"id"`
			Address   string `json:"address"`
			PublicKey string `json:"public_key"`
			Token     string `json:"token"`
		} `json:"realms"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	out := make([]mintedRealm, len(body.Realms))
	for i, r := range body.Realms {
		out[i] = mintedRealm{r.ID, r.Address, r.PublicKey, r.Token}
	}
	return out
}

func TestRecoveryTokens_DarkWhenFlagOff_404(t *testing.T) {
	f := newRecoveryFixture(t)
	session, err := f.s.sessionStore.Create(context.Background(), dashauth.SessionData{UserID: f.user.ID, TenantID: f.user.TenantID}, time.Hour)
	require.NoError(t, err)

	// Off: a 404 to everyone — no auth, a valid token, a session.
	assert.Equal(t, http.StatusNotFound, f.post("").Code)
	assert.Equal(t, http.StatusNotFound, f.post(f.jwt).Code)
	assert.Equal(t, http.StatusNotFound, f.post("", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: session})
	}).Code)

	// A per-tenant row cannot be written (the gate is read before
	// authentication, so only the global row means anything — RP44-2's
	// rule for translog_public); the route stays dark.
	require.ErrorIs(t, f.fl.Set(context.Background(), flagRecoveryVault, f.user.TenantID, true, "test"), flags.ErrGlobalOnly)
	t.Cleanup(func() { _ = f.fl.Unset(context.Background(), flagRecoveryVault, f.user.TenantID) })
	assert.Equal(t, http.StatusNotFound, f.post(f.jwt).Code)

	// Global ON: a user JWT gets its tokens; no credential is 401.
	f.open(t)
	assert.Len(t, decodeMinted(t, f.post(f.jwt)), 2)
	assert.Equal(t, http.StatusUnauthorized, f.post("").Code)
	assert.Equal(t, http.StatusUnauthorized, f.post("not-a-jwt").Code)

	// Not configured while on: 503, not a crash.
	f.s.recovery = nil
	assert.Equal(t, http.StatusServiceUnavailable, f.post(f.jwt).Code)
}

func TestRecoveryTokens_SubjectIsOpaque(t *testing.T) {
	f := newRecoveryFixture(t)
	f.open(t)
	minted := decodeMinted(t, f.post(f.jwt))
	require.Len(t, minted, 2)

	mac := hmac.New(sha256.New, mustHex(t, recoverySubjectKeyHex))
	mac.Write([]byte("stored/recovery/v1" + f.user.ID))
	wantSub := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	keys := map[string]string{recoveryRealm1: recoveryTokenKey1, recoveryRealm2: recoveryTokenKey2}
	kids := map[string]string{recoveryRealm1: "stored:1", recoveryRealm2: "storedcloud:7"}
	for _, m := range minted {
		// Every header and claim, as text: no user id, e-mail or tenant id.
		parts := strings.Split(m.Token, ".")
		require.Len(t, parts, 3)
		for _, p := range parts[:2] {
			raw, err := base64.RawURLEncoding.DecodeString(p)
			require.NoError(t, err)
			for _, leak := range []string{f.user.ID, f.user.Email, f.user.TenantID, strings.ReplaceAll(f.user.ID, "-", "")} {
				assert.NotContains(t, string(raw), leak, "realm %s", m.ID)
			}
		}
		// The token is what a realm accepts (cmd/recovery-realm/auth.go).
		var c jwt.MapClaims
		tok, err := jwt.NewParser(jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(m.ID), jwt.WithExpirationRequired()).
			ParseWithClaims(m.Token, &c, func(*jwt.Token) (any, error) { return mustHex(t, keys[m.ID]), nil })
		require.NoError(t, err)
		assert.Equal(t, kids[m.ID], tok.Header["kid"])
		assert.Equal(t, strings.Split(kids[m.ID], ":")[0], c["iss"], "iss is the kid's tenant")
		assert.Equal(t, wantSub, c["sub"])
		assert.Equal(t, "user", c["scope"])
		nbf, exp := int64(c["nbf"].(float64)), int64(c["exp"].(float64))
		assert.LessOrEqual(t, exp-nbf, int64(86400))
		assert.True(t, strings.HasSuffix(m.Address, "/"))
	}

	// Stable for the user, different for another.
	again := decodeMinted(t, f.post(f.jwt))
	assert.Equal(t, wantSub, claim(t, again[0].Token, "sub"))
	otherJWT, err := f.s.auth.GenerateJWT(f.other)
	require.NoError(t, err)
	assert.NotEqual(t, wantSub, claim(t, decodeMinted(t, f.post(otherJWT))[0].Token, "sub"))

	// Audited, with no subject in the trail.
	var meta string
	require.NoError(t, f.db.db.QueryRow(`SELECT metadata::text FROM audit_logs WHERE user_id = $1 AND action = 'recovery.tokens_issued' ORDER BY timestamp DESC LIMIT 1`, f.user.ID).Scan(&meta))
	assert.NotContains(t, meta, wantSub)
	assert.Contains(t, meta, `"realms": 2`)
}

func claim(t *testing.T, token, name string) any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	require.NoError(t, err)
	var c map[string]any
	require.NoError(t, json.Unmarshal(raw, &c))
	return c[name]
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

func TestRecoveryTokens_RateLimited(t *testing.T) {
	f := newRecoveryFixture(t)
	f.open(t)
	for i := 0; i < recoveryTokensPerHour; i++ {
		require.Equal(t, http.StatusOK, f.post(f.jwt).Code, "request %d", i+1)
	}
	w := f.post(f.jwt)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	otherJWT, err := f.s.auth.GenerateJWT(f.other)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, f.post(otherJWT).Code, "per user")

	// An hour later the user may ask again.
	f.s.recovery.now = func() time.Time { return time.Now().Add(time.Hour + time.Second) }
	assert.Equal(t, http.StatusOK, f.post(f.jwt).Code)
}

func TestRecoveryTokens_SessionNeedsCSRF(t *testing.T) {
	f := newRecoveryFixture(t)
	f.open(t)
	session, err := f.s.sessionStore.Create(context.Background(), dashauth.SessionData{UserID: f.user.ID, TenantID: f.user.TenantID, Email: f.user.Email}, time.Hour)
	require.NoError(t, err)
	cookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: session}) }

	assert.Equal(t, http.StatusForbidden, f.post("", cookie).Code, "a session without the page's CSRF token")
	mac := hmac.New(sha256.New, dashmw.DeriveCSRFKey("recovery-test-secret"))
	mac.Write([]byte(session))
	token := hex.EncodeToString(mac.Sum(nil))
	assert.Equal(t, http.StatusForbidden, f.post("", cookie, func(r *http.Request) {
		r.Header.Set("X-CSRF-Token", token)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	}).Code, "another site")
	minted := decodeMinted(t, f.post("", cookie, func(r *http.Request) { r.Header.Set("X-CSRF-Token", token) }))
	assert.Len(t, minted, 2)
	assert.Equal(t, http.StatusUnauthorized, f.post("", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: dashauth.SessionCookieName, Value: "ended"})
	}).Code)
}

func TestRecoveryTokens_Config(t *testing.T) {
	getenv, readFile := recoveryEnv(t)
	v, err := loadRecoveryVault(getenv, readFile)
	require.NoError(t, err)
	require.Len(t, v.realms, 2)
	assert.Equal(t, "https://realm-a.example/", v.realms[0].Address, "a trailing slash for the SDK's join")

	none, err := loadRecoveryVault(func(string) string { return "" }, readFile)
	require.NoError(t, err)
	assert.Nil(t, none, "nothing set: not configured")

	for name, over := range map[string]map[string]string{
		"subject key alone":   {envRecoveryTokenKeys: "", envRecoveryRealmsFile: ""},
		"subject key not hex": {envRecoverySubjectKey: "zz" + strings.Repeat("a1", 31)},
		"subject key short":   {envRecoverySubjectKey: "a1a1"},
		"token keys not json": {envRecoveryTokenKeys: "c1c1"},
		"a realm has no key":  {envRecoveryTokenKeys: fmt.Sprintf(`{%q: %q}`, recoveryRealm1, recoveryTokenKey1)},
		"realms file missing": {envRecoveryRealmsFile: "/nonexistent/realms.json"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadRecoveryVault(func(k string) string {
				if v, ok := over[k]; ok {
					return v
				}
				return getenv(k)
			}, readFile)
			require.Error(t, err)
			for _, key := range []string{recoverySubjectKeyHex, recoveryTokenKey1, recoveryTokenKey2} {
				assert.NotContains(t, err.Error(), key, "an error never quotes a key")
			}
		})
	}
	for name, realms := range map[string]string{
		"bad id":       `[{"id":"xyz","address":"https://a.example","public_key":"` + strings.Repeat("e1", 32) + `","kid":"stored:1"}]`,
		"bad address":  `[{"id":"` + recoveryRealm1 + `","address":"ftp://a","public_key":"` + strings.Repeat("e1", 32) + `","kid":"stored:1"}]`,
		"bad key":      `[{"id":"` + recoveryRealm1 + `","address":"https://a.example","public_key":"e1","kid":"stored:1"}]`,
		"bad kid":      `[{"id":"` + recoveryRealm1 + `","address":"https://a.example","public_key":"` + strings.Repeat("e1", 32) + `","kid":"stored"}]`,
		"empty":        `[]`,
		"listed twice": `[{"id":"` + recoveryRealm1 + `","address":"https://a.example","public_key":"` + strings.Repeat("e1", 32) + `","kid":"stored:1"},{"id":"` + recoveryRealm1 + `","address":"https://a.example","public_key":"` + strings.Repeat("e1", 32) + `","kid":"stored:1"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "r.json")
			require.NoError(t, os.WriteFile(p, []byte(realms), 0o600))
			_, err := loadRecoveryVault(func(k string) string {
				if k == envRecoveryRealmsFile {
					return p
				}
				return getenv(k)
			}, readFile)
			assert.Error(t, err)
		})
	}
}
