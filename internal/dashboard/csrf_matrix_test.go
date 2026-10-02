package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R12-5: R12's CSRF matrix, re-run over EVERY state-changing route of the
// two session chains as the router registers them (chi.Walk — a route added
// later is in the matrix without anyone remembering to add it). For each:
// no token, a wrong token, another session's valid token, a planted
// cookie + matching value (the old scheme's forgery), a good token sent
// cross-site, and the good token.

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

type csrfMatrixRow struct {
	method, route                                         string
	none, wrong, otherSession, planted, crossSite, goodOK string
}

func TestCSRFMatrix_EveryMutationOfBothChains(t *testing.T) {
	// Arrange: the production router, two customers and an admin signed in.
	r, authSvc, sessions := setupTestRouter(t)
	ctx := context.Background()
	signIn := func(email, role string) (cookie *http.Cookie) {
		u, err := authSvc.CreateUser(ctx, email, "securepass123")
		require.NoError(t, err)
		tok, err := sessions.Create(ctx, dashauth.SessionData{UserID: u.ID, TenantID: "t-" + role, Email: email, Role: role}, time.Hour)
		require.NoError(t, err)
		return &http.Cookie{Name: dashauth.SessionCookieName, Value: tok}
	}
	alice, bob, admin := signIn("alice@stored.ge", "user"), signIn("bob@stored.ge", "user"), signIn("root@stored.ge", "admin")

	// The token a session's pages carry: the layout's <meta name="csrf-token">
	// (what htmx sends as X-CSRF-Token) and the hidden field of the forms.
	meta := regexp.MustCompile(`<meta name="csrf-token" content="([0-9a-f]{64})">`)
	field := regexp.MustCompile(`name="csrf_token" value="([0-9a-f]{64})"`)
	pageToken := func(c *http.Cookie) string {
		req := httptest.NewRequest("GET", "/dashboard/apikeys", nil)
		req.AddCookie(c)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		m := meta.FindStringSubmatch(w.Body.String())
		require.NotNil(t, m, "the layout carries the token in its meta tag")
		f := field.FindStringSubmatch(w.Body.String())
		require.NotNil(t, f, "the page's form carries the token in its hidden field")
		require.Equal(t, m[1], f[1], "one token per session, wherever the page puts it")
		for _, sc := range w.Result().Cookies() {
			require.NotEqual(t, "csrf_token", sc.Name, "no CSRF cookie")
		}
		return m[1]
	}
	tokens := map[*http.Cookie]string{alice: pageToken(alice), bob: pageToken(bob), admin: pageToken(admin)}
	require.NotEqual(t, tokens[alice], tokens[bob])

	type mut struct{ method, route string }
	var muts []mut
	require.NoError(t, chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		unsafe := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
		if unsafe && (strings.HasPrefix(route, "/dashboard/") || strings.HasPrefix(route, "/admin/")) {
			muts = append(muts, mut{method, route})
		}
		return nil
	}))
	sort.Slice(muts, func(i, j int) bool { return muts[i].route < muts[j].route })

	do := func(m mut, session *http.Cookie, token string, extra func(*http.Request)) string {
		path := strings.TrimSuffix(pathParam.ReplaceAllString(m.route, "x"), "*")
		req := httptest.NewRequest(m.method, path, strings.NewReader("name=x"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(session)
		if token != "" {
			req.Header.Set("X-CSRF-Token", token)
		}
		if extra != nil {
			extra(req)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		body := w.Body.String()
		if w.Code == http.StatusForbidden && (strings.Contains(body, "CSRF token invalid") || strings.Contains(body, "Cross-site request refused")) {
			return "403"
		}
		return fmt.Sprintf("passes (%d)", w.Code)
	}

	// Act
	var rows []csrfMatrixRow
	var dash, adm int
	for _, m := range muts {
		me, other := alice, bob
		if strings.HasPrefix(m.route, "/admin/") {
			me, other = admin, alice
			adm++
		} else {
			dash++
		}
		planted := strings.Repeat("ab", 32)
		rows = append(rows, csrfMatrixRow{
			method: m.method, route: m.route,
			none:         do(m, me, "", nil),
			wrong:        do(m, me, strings.Repeat("0", 64), nil),
			otherSession: do(m, me, tokens[other], nil),
			planted: do(m, me, planted, func(q *http.Request) {
				q.AddCookie(&http.Cookie{Name: "csrf_token", Value: planted})
			}),
			crossSite: do(m, me, tokens[me], func(q *http.Request) {
				q.Header.Set("Sec-Fetch-Site", "cross-site")
				q.Header.Set("Origin", "https://evil.example")
			}),
			goodOK: do(m, me, tokens[me], func(q *http.Request) {
				q.Header.Set("Sec-Fetch-Site", "same-origin")
				q.Header.Set("Origin", "http://localhost:8000") // the fixture's BaseURL
			}),
		})
	}

	// Assert
	require.GreaterOrEqual(t, dash, 20, "R12 counted 20 /dashboard mutations")
	require.Greater(t, adm, 0)
	t.Logf("| route | no token | wrong token | another session's token | planted cookie + value | good token, cross-site | good token |")
	t.Logf("|---|---|---|---|---|---|---|")
	for _, row := range rows {
		t.Logf("| `%s %s` | %s | %s | %s | %s | %s | %s |", row.method, row.route, row.none, row.wrong, row.otherSession, row.planted, row.crossSite, row.goodOK)
		for name, got := range map[string]string{"no token": row.none, "wrong token": row.wrong,
			"another session's token": row.otherSession, "planted cookie": row.planted, "cross-site": row.crossSite} {
			assert.Equal(t, "403", got, "%s %s: %s", row.method, row.route, name)
		}
		assert.NotEqual(t, "403", row.goodOK, "%s %s: the good token is refused", row.method, row.route)
	}
	t.Logf("%d /dashboard mutations, %d /admin mutations", dash, adm)
}

// Both layouts hand the token to htmx, and every POST form of a session page
// carries the hidden field (the public forms have no session and no token).
func TestCSRF_EveryLayoutAndSessionFormCarriesTheToken(t *testing.T) {
	for _, layout := range []string{"templates/layouts/base.html", "templates/layouts/admin.html"} {
		b, err := Templates.ReadFile(layout)
		require.NoError(t, err)
		assert.Contains(t, string(b), `<meta name="csrf-token" content="{{.CSRFToken}}">`, layout)
		assert.Contains(t, string(b), `evt.detail.headers['X-CSRF-Token']`, layout)
	}
	form := regexp.MustCompile(`(?is)<form\b[^>]*>.*?</form>`)
	var checked int
	for _, dir := range []string{"templates/customer", "templates/admin"} {
		entries, err := Templates.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range entries {
			b, err := Templates.ReadFile(dir + "/" + e.Name())
			require.NoError(t, err)
			for _, f := range form.FindAllString(string(b), -1) {
				open := f[:strings.Index(f, ">")+1]
				lower := strings.ToLower(open)
				if !strings.Contains(lower, `method="post"`) && !strings.Contains(lower, "hx-post") {
					continue
				}
				if strings.Contains(lower, "hx-post") && !strings.Contains(lower, `method="post"`) {
					continue // htmx sends the meta tag's token as a header
				}
				checked++
				assert.True(t, strings.Contains(f, `name="csrf_token"`), "%s/%s: a POST form without the csrf_token field: %s", dir, e.Name(), open)
			}
		}
	}
	assert.Greater(t, checked, 10)
}

// The token changes when the session does: a sign-out and a new sign-in is a
// new session, and the page of the old one posts nothing into it.
func TestCSRF_TokenChangesWithTheSession(t *testing.T) {
	// Arrange
	r, authSvc, _ := setupTestRouter(t)
	_, err := authSvc.CreateUser(context.Background(), "carol@stored.ge", "securepass123")
	require.NoError(t, err)
	meta := regexp.MustCompile(`<meta name="csrf-token" content="([0-9a-f]{64})">`)
	signIn := func() (*http.Cookie, string) {
		req := httptest.NewRequest("POST", "/login", strings.NewReader("email=carol%40stored.ge&password=securepass123"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())
		var session *http.Cookie
		for _, c := range w.Result().Cookies() {
			if c.Name == dashauth.SessionCookieName {
				session = c
			}
		}
		require.NotNil(t, session)
		page := httptest.NewRequest("GET", "/dashboard/apikeys", nil)
		page.AddCookie(session)
		pw := httptest.NewRecorder()
		r.ServeHTTP(pw, page)
		m := meta.FindStringSubmatch(pw.Body.String())
		require.NotNil(t, m)
		return session, m[1]
	}
	post := func(session *http.Cookie, token string) int {
		req := httptest.NewRequest("POST", "/dashboard/settings/notifications", strings.NewReader("x=1"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", token)
		req.AddCookie(session)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	// Act: sign in, sign out, sign in again.
	s1, t1 := signIn()
	require.Equal(t, http.StatusOK, post(s1, t1))
	out := httptest.NewRequest("GET", "/logout", nil)
	out.AddCookie(s1)
	r.ServeHTTP(httptest.NewRecorder(), out)
	s2, t2 := signIn()

	// Assert
	assert.NotEqual(t, s1.Value, s2.Value)
	assert.NotEqual(t, t1, t2, "a new session has a new token")
	assert.Equal(t, http.StatusForbidden, post(s2, t1), "the page of the old session posts nothing into the new one")
	assert.Equal(t, http.StatusOK, post(s2, t2))
	assert.Equal(t, http.StatusSeeOther, post(s1, t1), "the signed-out session is gone: back to /login, token or not")
}

// The cookie policy lists the cookies the service sets. The CSRF cookie is
// gone; the page must not go on promising one.
func TestCookiePolicy_NoLongerListsACSRFCookie(t *testing.T) {
	b, err := Templates.ReadFile("templates/legal/cookies.html")
	require.NoError(t, err)
	page := string(b)
	table := page[strings.Index(page, "<tbody>"):strings.Index(page, "</tbody>")]
	assert.NotContains(t, table, "csrf_token")
	assert.Contains(t, table, dashauth.SessionCookieName)
	assert.Contains(t, page, "forgery protection uses no cookie")
}
