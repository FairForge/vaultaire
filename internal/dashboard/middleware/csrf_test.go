package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R12-5 (Review R12-19): the CSRF token is an HMAC over the session id
// with a server key. It used to be a bare random cookie compared with the
// submitted value — unsigned and not bound to the session, so any page that
// could set a cookie for the registrable domain (a `*.stored.ge` host)
// forged a valid pair.

const (
	sessionA = "session-token-of-alice-0123456789abcdef"
	sessionB = "session-token-of-bob-0123456789abcdef00"
)

var testCSRFKey = DeriveCSRFKey("a-jwt-secret-for-tests")

func okHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := Token(r.Context())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(tok))
	}
}

func csrfUnderTest() http.Handler {
	return NewCSRF(testCSRFKey, "https://stored.ge")(okHandler())
}

// tokenFor asks the middleware itself: a GET with the session cookie returns
// the token the page for that session would carry.
func tokenFor(t *testing.T, session string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "https://stored.ge/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: "vaultaire_session", Value: session})
	w := httptest.NewRecorder()
	csrfUnderTest().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, w.Body.String(), 64, "32 bytes of HMAC-SHA256, hex")
	return w.Body.String()
}

type post struct {
	session string
	header  string // X-CSRF-Token
	field   string // csrf_token form field
	cookies map[string]string
	headers map[string]string
}

func send(p post) *httptest.ResponseRecorder {
	var body *strings.Reader
	if p.field != "" {
		body = strings.NewReader("csrf_token=" + p.field + "&name=test")
	} else {
		body = strings.NewReader("name=test")
	}
	req := httptest.NewRequest("POST", "https://stored.ge/dashboard/settings/profile", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if p.session != "" {
		req.AddCookie(&http.Cookie{Name: "vaultaire_session", Value: p.session})
	}
	if p.header != "" {
		req.Header.Set("X-CSRF-Token", p.header)
	}
	for k, v := range p.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	csrfUnderTest().ServeHTTP(w, req)
	return w
}

func TestCSRF_TokenIsBoundToTheSession(t *testing.T) {
	// Arrange
	a, b := tokenFor(t, sessionA), tokenFor(t, sessionB)

	// Assert: deterministic per session, different across sessions.
	assert.Equal(t, a, tokenFor(t, sessionA), "the same session gets the same token on every page")
	assert.NotEqual(t, a, b, "a new session (login, 2FA step-up, logout + login) gets a new token")

	// Act + Assert: A's token works for A, in the form field and in the header.
	assert.Equal(t, http.StatusOK, send(post{session: sessionA, field: a}).Code)
	assert.Equal(t, http.StatusOK, send(post{session: sessionA, header: a}).Code)

	// A token minted for one session is refused for another.
	w := send(post{session: sessionB, header: a})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "CSRF token invalid")
}

func TestCSRF_APlantedCookieValidatesNothing(t *testing.T) {
	// The old double-submit: cookie == submitted value. An attacker who can
	// set a cookie for the registrable domain plants one and submits the
	// same value.
	planted := strings.Repeat("ab", 32)

	w := send(post{session: sessionA, header: planted, cookies: map[string]string{"csrf_token": planted}})

	assert.Equal(t, http.StatusForbidden, w.Code, "a cookie the attacker chose must not make their value valid")
}

func TestCSRF_NoCookieIsIssuedAndAnOldOneIsCleared(t *testing.T) {
	// Arrange + Act: a first visit.
	req := httptest.NewRequest("GET", "https://stored.ge/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: "vaultaire_session", Value: sessionA})
	w := httptest.NewRecorder()
	csrfUnderTest().ServeHTTP(w, req)

	// Assert: nothing to plant against — the token lives in the page only.
	for _, c := range w.Result().Cookies() {
		assert.NotEqual(t, "csrf_token", c.Name, "no CSRF cookie is set any more")
	}

	// A browser that still holds the old cookie is told to drop it.
	req = httptest.NewRequest("GET", "https://stored.ge/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: "vaultaire_session", Value: sessionA})
	req.AddCookie(&http.Cookie{Name: "csrf_token", Value: "left-over"})
	w = httptest.NewRecorder()
	csrfUnderTest().ServeHTTP(w, req)
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == "csrf_token" && c.MaxAge < 0 {
			cleared = true
		}
	}
	assert.True(t, cleared)
}

func TestCSRF_MissingOrWrongTokenIsRefused(t *testing.T) {
	good := tokenFor(t, sessionA)
	for name, p := range map[string]post{
		"no token":                 {session: sessionA},
		"wrong token":              {session: sessionA, header: strings.Repeat("0", 64)},
		"truncated token":          {session: sessionA, header: good[:63]},
		"token with a suffix":      {session: sessionA, header: good + "0"},
		"good token, no session":   {header: good},
		"empty token, no session":  {},
		"upper-cased token":        {session: sessionA, header: strings.ToUpper(good)},
		"token of the empty input": {session: sessionA, header: tokenForNoSession(t)},
	} {
		assert.Equal(t, http.StatusForbidden, send(p).Code, name)
	}
}

// tokenForNoSession: what a GET without a session cookie carries (nothing).
func tokenForNoSession(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest("GET", "https://stored.ge/dashboard", nil)
	w := httptest.NewRecorder()
	csrfUnderTest().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, w.Body.String(), "no session, no token — never the HMAC of an empty string")
	return "x"
}

func TestCSRF_TheKeyDecidesTheToken(t *testing.T) {
	// Same secret → same key → the token an open form carries is still valid
	// after a restart. Another secret → another token.
	assert.Equal(t, DeriveCSRFKey("a-jwt-secret-for-tests"), testCSRFKey)
	assert.NotEqual(t, DeriveCSRFKey("another-secret"), testCSRFKey)
	assert.NotEqual(t, []byte("a-jwt-secret-for-tests"), testCSRFKey, "the key is derived, never the secret itself")
	assert.Len(t, testCSRFKey, 32)

	other := NewCSRF(DeriveCSRFKey("another-secret"), "https://stored.ge")(okHandler())
	req := httptest.NewRequest("POST", "https://stored.ge/dashboard/x", nil)
	req.AddCookie(&http.Cookie{Name: "vaultaire_session", Value: sessionA})
	req.Header.Set("X-CSRF-Token", tokenFor(t, sessionA))
	w := httptest.NewRecorder()
	other.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)

	// No secret configured: a random key per process, never an empty one.
	k1, k2 := DeriveCSRFKey(""), DeriveCSRFKey("")
	assert.Len(t, k1, 32)
	assert.NotEqual(t, k1, k2)
}

func TestCSRF_FetchMetadataAndOrigin(t *testing.T) {
	good := tokenFor(t, sessionA)
	with := func(h map[string]string) int { return send(post{session: sessionA, header: good, headers: h}).Code }

	// Refused even with a valid token.
	for name, h := range map[string]map[string]string{
		"cross-site":                      {"Sec-Fetch-Site": "cross-site"},
		"same-site (a sibling subdomain)": {"Sec-Fetch-Site": "same-site"},
		"an unknown Sec-Fetch-Site value": {"Sec-Fetch-Site": "whatever"},
		"Origin of a sibling subdomain":   {"Origin": "https://cdn.stored.ge"},
		"Origin of another site":          {"Origin": "https://evil.example"},
		"Origin null (sandboxed frame)":   {"Origin": "null"},
		"http Origin of the same host":    {"Origin": "http://stored.ge"},
		"same-origin fetch, foreign Origin": {
			"Sec-Fetch-Site": "same-origin", "Origin": "https://evil.example"},
		"Origin with our host as a prefix": {"Origin": "https://stored.ge.evil.example"},
	} {
		assert.Equal(t, http.StatusForbidden, with(h), name)
	}

	// Accepted.
	for name, h := range map[string]map[string]string{
		"neither header (curl, old browsers): the token alone": {},
		"same-origin":                    {"Sec-Fetch-Site": "same-origin"},
		"user-initiated (none)":          {"Sec-Fetch-Site": "none"},
		"Origin = the configured origin": {"Origin": "https://stored.ge"},
		"same-origin with its Origin":    {"Sec-Fetch-Site": "same-origin", "Origin": "https://stored.ge"},
	} {
		assert.Equal(t, http.StatusOK, with(h), name)
	}

	// The fetch-metadata checks never replace the token.
	assert.Equal(t, http.StatusForbidden,
		send(post{session: sessionA, headers: map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://stored.ge"}}).Code)
}

func TestCSRF_OriginOfTheRequestHostIsAccepted(t *testing.T) {
	// The dashboard is served on more than one name (stored.ge, stored.cloud):
	// the origin of the host the request was sent to is the page's own.
	h := NewCSRF(testCSRFKey, "https://stored.ge")(okHandler())
	post := func(host, origin string) int {
		req := httptest.NewRequest("POST", "https://"+host+"/dashboard/x", nil)
		req.Host = host
		req.AddCookie(&http.Cookie{Name: "vaultaire_session", Value: sessionA})
		req.Header.Set("X-CSRF-Token", tokenFor(t, sessionA))
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	assert.Equal(t, http.StatusOK, post("stored.cloud", "https://stored.cloud"))
	assert.Equal(t, http.StatusForbidden, post("stored.cloud", "https://cdn.stored.cloud"))
	// Plain http is the page's own origin only on a loopback host (local dev).
	assert.Equal(t, http.StatusOK, post("localhost:8000", "http://localhost:8000"))
	assert.Equal(t, http.StatusOK, post("127.0.0.1:8099", "http://127.0.0.1:8099"))
	assert.Equal(t, http.StatusForbidden, post("stored.cloud", "http://stored.cloud"))
}

func TestCSRF_SafeMethodsPassWithoutAToken(t *testing.T) {
	for _, m := range []string{"GET", "HEAD", "OPTIONS"} {
		req := httptest.NewRequest(m, "https://stored.ge/dashboard", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site") // a link from another site is a navigation, not a mutation
		w := httptest.NewRecorder()
		csrfUnderTest().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, m)
	}
}
