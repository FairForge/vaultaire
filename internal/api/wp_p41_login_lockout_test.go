package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/dashboard"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
)

// Stage 1a Prompt 41 Part 0 items 1 and 2 (RP57-2, upgraded to P2 by the
// WP-P40 review's second pass; RP57-3; RP58-8 a/b): the JSON sign-in
// POST /auth/login had the per-IP limiter (5/min) but no per-ACCOUNT
// lockout — the dashboard locks an account for 15 min after 10 failed
// sign-ins in 15 min, password OR second factor — and no metric: a
// second-factor guess from ~100 IPs at the per-IP limit reached ~50 % TOTP
// success in ~8 h against a leaked password, invisible to the auth rules.
// Now both surfaces share ONE lockout (a dashboard lock refuses the JSON
// sign-in and the reverse), every failure of either surface is a
// vaultaire_dashboard_login_failures_total{surface,reason} increment, a
// store failure is ours (500, its own reason) and never a caller's 401, and
// the audit row of a factor that passed is written even when the token
// after it could not be minted.

// loginFailures reads the counter for one surface/reason pair.
func loginFailures(surface, reason string) float64 {
	return promtest.ToFloat64(dashboard.LoginFailures.WithLabelValues(surface, reason))
}

func (f *loginFixture) badPassword() *httptest.ResponseRecorder {
	return f.post("/auth/login", map[string]string{"email": f.email, "password": "not-the-password"})
}

// dashboardLogin posts the dashboard's sign-in FORM (the other surface) from
// its own client address.
func (f *loginFixture) dashboardLogin(password string) *httptest.ResponseRecorder {
	f.t.Helper()
	form := url.Values{"email": {f.email}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Host = "stored.ge"
	f.n++
	req.RemoteAddr = fmt.Sprintf("198.51.100.%d:4000", f.n)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	return w
}

func TestAuthLogin_TenFailuresLockTheAccountWhateverTheIP(t *testing.T) {
	f := newLoginFixture(t)
	bad0, locked0 := loginFailures("api", "bad_password"), loginFailures("api", "locked")
	lockouts0 := promtest.ToFloat64(dashboard.LoginLockouts)

	// Eleven wrong passwords from eleven addresses: the per-IP limiter sees
	// one attempt each.
	for i := 1; i <= 11; i++ {
		w := f.badPassword()
		assert.Equal(t, http.StatusUnauthorized, w.Code, "attempt %d", i)
		assert.Equal(t, "Invalid credentials\n", w.Body.String(), "attempt %d: a locked account answers exactly as a wrong password", i)
	}
	// The right password is refused while the lock holds.
	w := f.login("")
	assert.Equal(t, http.StatusUnauthorized, w.Code, "locked: the right password is refused too")
	assert.Equal(t, "Invalid credentials\n", w.Body.String())
	assert.Empty(t, loginToken(t, w))

	want := []string{"auth.login_failed api:password locked", "auth.login_failed api:password locked", "auth.login_locked api:password"}
	for i := 0; i < 10; i++ {
		want = append(want, "auth.login_failed api:password bad_password")
	}
	assert.ElementsMatch(t, want, f.events(), "10 evaluated, the lock at the 10th, the 11th and the right password refused unevaluated")

	assert.Equal(t, 10.0, loginFailures("api", "bad_password")-bad0, "every password failure counted on the api surface")
	assert.Equal(t, 2.0, loginFailures("api", "locked")-locked0, "the refusals of a locked account counted")
	assert.Equal(t, 1.0, promtest.ToFloat64(dashboard.LoginLockouts)-lockouts0, "one lockout")
}

func TestAuthLogin_ARightPasswordAndTenBadCodesLockTheAccount(t *testing.T) {
	f := newLoginFixture(t)
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Stored", AccountName: f.email})
	require.NoError(t, err)
	require.NoError(t, f.srv.auth.EnableMFA(context.Background(), f.userID, key.Secret(), nil))
	badCode0 := loginFailures("api", "bad_code")

	for i := 1; i <= 10; i++ {
		w := f.login(fmt.Sprintf("%06d", i)) // never the real code: six digits, so the row names totp
		assert.Equal(t, http.StatusUnauthorized, w.Code, "attempt %d", i)
	}
	code, err := totp.GenerateCode(key.Secret(), time.Now())
	require.NoError(t, err)
	w := f.login(code)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "the right code is refused while the lock holds")
	assert.Equal(t, "Invalid credentials\n", w.Body.String(), "nothing says 'locked'")
	assert.Empty(t, loginToken(t, w))

	want := []string{"auth.login_locked api:totp", "auth.login_failed api:password locked"}
	for i := 0; i < 10; i++ {
		want = append(want, "auth.mfa_failed api:totp bad_code")
	}
	assert.ElementsMatch(t, want, f.events(), "second-factor guesses count like first-factor guesses (the dashboard's rule)")
	assert.Equal(t, 10.0, loginFailures("api", "bad_code")-badCode0)
}

func TestAuthLogin_TheLockoutIsTheDashboards(t *testing.T) {
	t.Run("a dashboard lockout refuses the JSON sign-in", func(t *testing.T) {
		f := newLoginFixture(t)
		dashBad0, apiBad0 := loginFailures("dashboard", "bad_password"), loginFailures("api", "bad_password")
		for i := 1; i <= 10; i++ {
			w := f.dashboardLogin("wrong")
			require.Equal(t, http.StatusUnauthorized, w.Code, "dashboard attempt %d", i)
		}
		assert.Equal(t, 10.0, loginFailures("dashboard", "bad_password")-dashBad0, "the form's failures count on the dashboard surface")
		assert.Equal(t, 0.0, loginFailures("api", "bad_password")-apiBad0, "and never on the api surface")
		w := f.login("")
		assert.Equal(t, http.StatusUnauthorized, w.Code, "the JSON sign-in with the right password: %s", w.Body.String())
		assert.Empty(t, loginToken(t, w))
		assert.Contains(t, f.events(), "auth.login_failed api:password locked")
		assert.Contains(t, f.events(), "auth.login_locked password")
	})
	t.Run("a JSON sign-in lockout refuses the dashboard", func(t *testing.T) {
		f := newLoginFixture(t)
		for i := 1; i <= 10; i++ {
			require.Equal(t, http.StatusUnauthorized, f.badPassword().Code, "api attempt %d", i)
		}
		w := f.dashboardLogin(f.password)
		assert.Equal(t, http.StatusUnauthorized, w.Code, "the dashboard form with the right password")
		assert.Contains(t, w.Body.String(), "Invalid email or password")
		for _, c := range w.Result().Cookies() {
			assert.Empty(t, c.Value, "no session cookie %s with a value", c.Name)
		}
		assert.Contains(t, f.events(), "auth.login_failed password locked")
		assert.Contains(t, f.events(), "auth.login_locked api:password")
	})
}

// A store failure (the users read erroring) is OUR failure: 500 with the
// generic text, one Error log, an audit row with its own reason — never the
// caller's 401 "Invalid credentials" audited as bad_password / unknown_user
// (RP58-8 b), and never a failed attempt against the account.
func TestAuthLogin_AStoreFailureIsOursNotTheCallers(t *testing.T) {
	f := newLoginFixture(t)
	store0 := loginFailures("api", "store_error")
	orig := f.srv.auth
	t.Cleanup(func() { f.srv.SetAuthService(orig) })

	dead, err := sql.Open("postgres", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	require.NoError(t, dead.Close()) // every query: sql: database is closed
	f.srv.SetAuthService(auth.NewAuthService(nil, dead))

	for i := 0; i < 11; i++ {
		w := f.login("")
		assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		assert.Equal(t, http.StatusText(http.StatusInternalServerError)+"\n", w.Body.String(), "the generic text")
	}
	assert.Equal(t, 11.0, loginFailures("api", "store_error")-store0)

	// The rows name the store, never the caller — and never the user id,
	// which the store could not give.
	rows, err := f.db.Query(`SELECT action, metadata->>'reason', metadata->>'via' FROM audit_logs
		WHERE event_type = 'auth' AND metadata->>'email' = $1 ORDER BY timestamp`, f.email)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var action, reason, via string
		require.NoError(t, rows.Scan(&action, &reason, &via))
		assert.Equal(t, "auth.login_failed", action)
		assert.Equal(t, "store_error", reason)
		assert.Equal(t, "api:password", via)
		n++
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, 11, n)

	// Eleven store failures locked nothing: the store back, the password works.
	f.srv.SetAuthService(orig)
	w := f.login("")
	assert.Equal(t, http.StatusOK, w.Code, "a store failure is not a failed attempt: %s", w.Body.String())
	assert.NotEmpty(t, loginToken(t, w))
}

// RP57-3: the password validated but the user row could not be loaded. It
// answered 404 "User not found" and wrote nothing. The seam fails the load
// only (the password check before it reads the same store and passed).
func TestAuthLogin_AUserRowThatCannotBeLoadedIs500AndAudited(t *testing.T) {
	f := newLoginFixture(t)
	load0 := loginFailures("api", "user_load_failed")
	f.srv.loginUserLoader = func(context.Context, string) (*auth.User, error) {
		return nil, errors.New("read users: connection reset")
	}
	t.Cleanup(func() { f.srv.loginUserLoader = nil })

	w := f.login("")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, http.StatusText(http.StatusInternalServerError)+"\n", w.Body.String())
	assert.Empty(t, loginToken(t, w))
	assert.Equal(t, 1.0, loginFailures("api", "user_load_failed")-load0)

	var reason string
	require.NoError(t, f.db.QueryRow(`SELECT metadata->>'reason' FROM audit_logs
		WHERE event_type = 'auth' AND action = 'auth.login_failed' AND metadata->>'email' = $1`, f.email).Scan(&reason))
	assert.Equal(t, "user_load_failed", reason)

	// Ours, not a failed attempt: the loader back, the same password signs in.
	f.srv.loginUserLoader = nil
	assert.Equal(t, http.StatusOK, f.login("").Code)
}

// RP58-8 a: on a second factor that PASSED and a token mint that failed, the
// TOTP code is consumed and auth.mfa_succeeded was never written. The row
// records what happened (the factor passed); the Error log the mint.
func TestAuthLogin_ATokenMintFailureStillRecordsTheFactorThatPassed(t *testing.T) {
	f := newLoginFixture(t)
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Stored", AccountName: f.email})
	require.NoError(t, err)
	require.NoError(t, f.srv.auth.EnableMFA(context.Background(), f.userID, key.Secret(), nil))
	f.srv.loginTokenMinter = func(*auth.User) (string, error) { return "", errors.New("hsm away") }
	t.Cleanup(func() { f.srv.loginTokenMinter = nil })

	code, err := totp.GenerateCode(key.Secret(), time.Now())
	require.NoError(t, err)
	w := f.login(code)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Empty(t, loginToken(t, w))
	assert.Equal(t, []string{"auth.mfa_succeeded api:totp"}, f.events(), "the code was accepted (and burnt): the row says so")

	// The same code again is a replay, as it should be — the burnt code is
	// acceptable, the missing row was not.
	w = f.login(code)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, f.events(), "auth.mfa_failed api:totp replayed_code")
}

// Every surface × reason pair of the sign-in counter exists at 0 from boot
// (a rule on a series that does not exist yet reads nothing), and the
// JSON sign-in's failures land on the api surface by reason.
func TestLoginFailures_EverySeriesExistsFromBootAndTheAPICountsByReason(t *testing.T) {
	ch := make(chan prometheus.Metric, 64)
	go func() { dashboard.LoginFailures.Collect(ch); close(ch) }()
	have := map[string]bool{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		var surface, reason string
		for _, l := range pb.GetLabel() {
			switch l.GetName() {
			case "surface":
				surface = l.GetValue()
			case "reason":
				reason = l.GetValue()
			}
		}
		have[surface+"/"+reason] = true
	}
	for _, surface := range []string{"dashboard", "api"} {
		for _, reason := range dashboard.LoginFailureReasons {
			assert.True(t, have[surface+"/"+reason], "%s/%s pre-created", surface, reason)
		}
	}

	f := newLoginFixture(t)
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Stored", AccountName: f.email})
	require.NoError(t, err)
	require.NoError(t, f.srv.auth.EnableMFA(context.Background(), f.userID, key.Secret(), nil))
	before := map[string]float64{}
	for _, reason := range dashboard.LoginFailureReasons {
		before[reason] = loginFailures("api", reason)
	}
	f.login("")                                                                             // code_required
	f.login("000000")                                                                       // bad_code
	f.badPassword()                                                                         // bad_password
	f.post("/auth/login", map[string]string{"email": "nobody-" + f.email, "password": "x"}) // unknown_user
	for reason, want := range map[string]float64{"code_required": 1, "bad_code": 1, "bad_password": 1, "unknown_user": 1,
		"locked": 0, "replayed_code": 0, "store_error": 0, "user_load_failed": 0} {
		assert.Equal(t, want, loginFailures("api", reason)-before[reason], reason)
	}
	// The dashboard surface counted none of them.
	assert.Equal(t, 0.0, loginFailures("dashboard", "bad_password")-promtest.ToFloat64(dashboard.LoginFailures.WithLabelValues("dashboard", "bad_password")))
}

// dashboardVerify2FA posts the dashboard's second-factor form with the
// pending-challenge cookie a password login set, from its own address.
func (f *loginFixture) dashboardVerify2FA(pending *http.Cookie, code string) *httptest.ResponseRecorder {
	f.t.Helper()
	form := url.Values{"totp_code": {code}}
	req := httptest.NewRequest(http.MethodPost, "/login/verify-2fa", strings.NewReader(form.Encode()))
	req.Host = "stored.ge"
	f.n++
	req.RemoteAddr = fmt.Sprintf("198.51.100.%d:4000", f.n)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(pending)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	return w
}

// The dashboard's rule, held on the dashboard too: the password step used to
// RESET the account's failure count before the second factor, so a known
// password plus a fresh /login before every code guess never locked — the
// same hole item 1 closes on /auth/login. A sign-in resets the count only
// when it completes.
func TestDashboardLogin_AFreshPasswordStepBeforeEveryBadCodeStillLocks(t *testing.T) {
	f := newLoginFixture(t)
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Stored", AccountName: f.email})
	require.NoError(t, err)
	require.NoError(t, f.srv.auth.EnableMFA(context.Background(), f.userID, key.Secret(), nil))
	lockouts0 := promtest.ToFloat64(dashboard.LoginLockouts)

	pendingCookie := func(w *httptest.ResponseRecorder) *http.Cookie {
		for _, c := range w.Result().Cookies() {
			if c.Name == handlers.MFAPendingCookie && c.Value != "" {
				return c
			}
		}
		return nil
	}
	for i := 1; i <= 10; i++ {
		w := f.dashboardLogin(f.password)
		require.Equal(t, http.StatusSeeOther, w.Code, "login %d: the password step: %s", i, w.Body.String())
		c := pendingCookie(w)
		require.NotNil(t, c, "login %d: a pending challenge", i)
		w = f.dashboardVerify2FA(c, fmt.Sprintf("%06d", i))
		if i < 10 {
			assert.Equal(t, http.StatusUnauthorized, w.Code, "code guess %d", i)
		} else {
			assert.Equal(t, http.StatusSeeOther, w.Code, "the 10th guess locks: the challenge is consumed and the browser sent back to /login")
		}
	}
	assert.Equal(t, 1.0, promtest.ToFloat64(dashboard.LoginLockouts)-lockouts0, "one lockout")
	assert.Contains(t, f.events(), "auth.login_locked")

	w := f.dashboardLogin(f.password)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "locked: the right password is refused on the dashboard")
	assert.Nil(t, pendingCookie(w), "no challenge for a locked account")
	code, err := totp.GenerateCode(key.Secret(), time.Now())
	require.NoError(t, err)
	w = f.login(code)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "locked: the right password and code are refused on /auth/login too")
	assert.Contains(t, f.events(), "auth.login_failed api:password locked")
}
