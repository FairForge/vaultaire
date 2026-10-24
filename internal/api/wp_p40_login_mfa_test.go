package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/testutil"
)

// Found while doing Prompt 40 Part 0 item 5 (RP56-5, option b: "a real admin
// JWT, obtained by signing in with MFA"): the JSON sign-in POST /auth/login
// minted a 24 h JWT on the password ALONE for an account with MFA on — the
// prod admin included — and the admin API (requireJWT + users.role) took
// it; the dashboard's sign-in for the same account asks for the code. It
// wrote no audit row either. Now an MFA account must send "code" (a TOTP
// code, accepted once, or a backup code, single use) and every outcome is
// an auth.* row with via "api:…".

type loginFixture struct {
	t        *testing.T
	h        http.Handler
	srv      *Server
	db       *sql.DB
	email    string
	password string
	userID   string
	n        int
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("DB-backed")
	}
	srv, _ := gateServer(t, prodTodayHosts)
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	f := &loginFixture{t: t, h: srv.httpServer.Handler, srv: srv, db: db,
		email: "login-mfa-" + uuid.New().String()[:8] + "@test.local", password: "correct-horse-battery-9"}
	w := f.post("/auth/register", map[string]string{"email": f.email, "password": f.password})
	require.Less(t, w.Code, 300, "register: %s", w.Body.String())
	require.NoError(t, db.QueryRow(`SELECT id FROM users WHERE email = $1`, f.email).Scan(&f.userID))
	t.Cleanup(func() {
		var tenant string
		_ = db.QueryRow(`SELECT tenant_id FROM api_keys WHERE user_id = $1 LIMIT 1`, f.userID).Scan(&tenant)
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE user_id = $1 OR performed_by = $1`, f.userID)
		_, _ = db.Exec(`DELETE FROM user_mfa WHERE user_id = $1`, f.userID)
		_, _ = db.Exec(`DELETE FROM api_keys WHERE user_id = $1`, f.userID)
		if tenant != "" {
			_, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, tenant)
			_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenant)
			_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenant)
		}
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, f.userID)
	})
	return f
}

func (f *loginFixture) post(path string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	b, err := json.Marshal(body)
	require.NoError(f.t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Host = "stored.ge"
	f.n++ // its own client address: the sign-in limiter allows 5/min per IP
	req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", f.n)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	return w
}

func (f *loginFixture) login(code string) *httptest.ResponseRecorder {
	body := map[string]string{"email": f.email, "password": f.password}
	if code != "" {
		body["code"] = code
	}
	return f.post("/auth/login", body)
}

// loginToken returns the JWT of a 200 login, "" otherwise.
func loginToken(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != http.StatusOK {
		return ""
	}
	var out struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
	return out.Token
}

// events lists the user's auth.* audit rows as "action via reason".
func (f *loginFixture) events() []string {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT action, COALESCE(metadata->>'via',''), COALESCE(metadata->>'reason','')
		FROM audit_logs WHERE user_id = $1 AND event_type = 'auth' ORDER BY timestamp, id`, f.userID)
	require.NoError(f.t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var a, via, reason string
		require.NoError(f.t, rows.Scan(&a, &via, &reason))
		out = append(out, strings.TrimSpace(a+" "+via+" "+reason))
	}
	require.NoError(f.t, rows.Err())
	sort.Strings(out)
	return out
}

func TestAuthLogin_AnMFAAccountNeedsItsCode(t *testing.T) {
	f := newLoginFixture(t)
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Stored", AccountName: f.email})
	require.NoError(t, err)
	require.NoError(t, f.srv.auth.EnableMFA(context.Background(), f.userID, key.Secret(), []string{"backup-code-p40a"}))

	w := f.login("")
	assert.Equal(t, http.StatusUnauthorized, w.Code, "the password alone: %s", w.Body.String())
	assert.Empty(t, loginToken(t, w), "no JWT without the second factor")
	assert.Contains(t, w.Body.String(), "code required")

	w = f.login("000000")
	assert.Equal(t, http.StatusUnauthorized, w.Code, "a wrong code")

	code, err := totp.GenerateCode(key.Secret(), time.Now())
	require.NoError(t, err)
	w = f.login(code)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	tok := loginToken(t, w)
	_, err = f.srv.auth.ValidateJWT(tok)
	require.NoError(t, err)

	w = f.login(code)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "a TOTP code is accepted once (RFC 6238 § 5.2)")

	w = f.login("backup-code-p40a")
	assert.Equal(t, http.StatusOK, w.Code, "a backup code: %s", w.Body.String())
	w = f.login("backup-code-p40a")
	assert.Equal(t, http.StatusUnauthorized, w.Code, "a backup code is single use")

	wrongPw := f.post("/auth/login", map[string]string{"email": f.email, "password": "not-the-password", "code": code})
	assert.Equal(t, http.StatusUnauthorized, wrongPw.Code)

	assert.Equal(t, []string{
		"auth.login_failed api:password bad_password",
		"auth.mfa_failed api:backup_code bad_code",
		"auth.mfa_failed api:totp bad_code",
		"auth.mfa_failed api:totp code_required",
		"auth.mfa_failed api:totp replayed_code",
		"auth.mfa_succeeded api:backup_code",
		"auth.mfa_succeeded api:totp",
	}, f.events(), "every outcome of the JSON sign-in is in the audit trail")
}

func TestAuthLogin_WithoutMFAThePasswordIsEnoughAndAudited(t *testing.T) {
	f := newLoginFixture(t)
	w := f.login("")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotEmpty(t, loginToken(t, w))
	assert.Equal(t, []string{"auth.login_succeeded api:password"}, f.events())
}
