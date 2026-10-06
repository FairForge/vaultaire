package api

// CORS on the S3 API (docs/STATUS.md queue item 2, 2026-10-06).
//
// Found by the 2026-10-06 plan review: `?cors` answered 501 and a browser
// preflight to the S3 endpoint got a 403 "No authorization header provided"
// with no CORS headers, so no web application (a customer's uploader, Ente
// web, SnapShelter) could use the S3 API from a browser. docs/API.md and
// llms.txt claimed `?cors` all along.
//
// The shape is AWS's: PutBucketCors / GetBucketCors / DeleteBucketCors on
// the bucket, default none; the OPTIONS preflight is answered BEFORE SigV4
// (a browser never signs it) for the bucket named in the path; the actual
// response carries the headers of the first matching rule on every status,
// the auth error included, so a browser can read a 403 instead of seeing a
// network error.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// --- parser -----------------------------------------------------------------

func TestDetermineOperation_CorsSubresource(t *testing.T) {
	p := NewS3Parser(zap.NewNop())
	cases := []struct{ method, want string }{
		{"GET", auth.OpGetBucketCors},
		{"PUT", auth.OpPutBucketCors},
		{"DELETE", auth.OpDeleteBucketCors},
	}
	for _, tc := range cases {
		req, err := p.ParseRequest(httptest.NewRequest(tc.method, "/bkt?cors", nil))
		require.NoError(t, err)
		assert.Equal(t, tc.want, req.Operation, "%s /bkt?cors", tc.method)
		assert.True(t, auth.ValidPermissions[tc.want], "%s is a grantable permission", tc.want)
	}
	// An object-level ?cors is not a thing: it stays the plain object op
	// gated by the object sub-resource list (unchanged).
	req, err := p.ParseRequest(httptest.NewRequest("GET", "/bkt/obj?cors", nil))
	require.NoError(t, err)
	assert.Equal(t, auth.OpGetObject, req.Operation)
}

// --- rule matching -----------------------------------------------------------

func TestCORSPatternMatch(t *testing.T) {
	cases := []struct {
		pattern, value string
		fold, want     bool
	}{
		{"*", "https://anything.example", false, true},
		{"https://app.example", "https://app.example", false, true},
		{"https://app.example", "https://APP.example", false, false}, // origins are case-sensitive
		{"https://app.example", "http://app.example", false, false},
		{"https://app.example", "https://app.example:8443", false, false},
		{"https://*.example.com", "https://a.example.com", false, true},
		{"https://*.example.com", "https://a.b.example.com", false, true},
		{"https://*.example.com", "https://example.com", false, false},
		{"https://*.example.com", "https://a.example.com.evil", false, false},
		{"*.example.com", "https://a.example.com", false, true},
		{"x-amz-*", "X-Amz-Meta-Foo", true, true}, // headers fold
		{"content-type", "Content-Type", true, true},
		{"content-type", "content-length", true, false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, corsPatternMatch(tc.pattern, tc.value, tc.fold), "%q vs %q", tc.pattern, tc.value)
	}
}

func TestCORSMatchRule(t *testing.T) {
	rules := []CORSRule{
		{AllowedOrigins: []string{"https://app.example"}, AllowedMethods: []string{"GET", "PUT"},
			AllowedHeaders: []string{"content-type", "x-amz-*"}, ExposeHeaders: []string{"ETag"}},
		{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}},
	}
	// the first rule whose origin AND method match wins
	r := corsMatchRule(rules, "https://app.example", "PUT", []string{"Content-Type", "x-amz-meta-a"}, true)
	require.NotNil(t, r)
	assert.Equal(t, "https://app.example", r.AllowedOrigins[0])
	// a requested header outside the rule's allow-list: no match (preflight)
	assert.Nil(t, corsMatchRule(rules, "https://app.example", "PUT", []string{"X-Custom"}, true))
	// … but an actual request does not check headers (AWS)
	assert.NotNil(t, corsMatchRule(rules, "https://app.example", "PUT", nil, false))
	// a method the first rule lacks falls to the wildcard rule
	r = corsMatchRule(rules, "https://other.example", "GET", nil, true)
	require.NotNil(t, r)
	assert.Equal(t, "*", r.AllowedOrigins[0])
	assert.Nil(t, corsMatchRule(rules, "https://other.example", "DELETE", nil, true))
	assert.Nil(t, corsMatchRule(nil, "https://app.example", "GET", nil, true))
}

// --- fixture -----------------------------------------------------------------

type corsFixture struct {
	server   *Server
	db       *sql.DB
	tenantID string
	tenant   *tenant.Tenant
	bucket   string
}

// setupCORSFixture: a tenant with one bucket, a server with the real S3
// catch-all + preflight route registration. testMode=false runs the real
// SigV4 gate (every unsigned request is 403) — what proves the preflight is
// answered before it and that the 403 carries the headers.
func setupCORSFixture(t *testing.T, testMode bool) *corsFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}

	tenantID := fmt.Sprintf("cors-%d-%s", os.Getpid(), strings.ToLower(t.Name()[len("Test"):min(len(t.Name()), 20)]))
	bucket := "cors-bucket"
	_, err = db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (id) DO NOTHING`,
		tenantID, "CORS test", tenantID+"@test.local", "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, $2, 'private')
		ON CONFLICT (tenant_id, name) DO NOTHING`, tenantID, bucket)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM buckets WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})

	srv := &Server{
		logger:   zap.NewNop(),
		router:   chi.NewRouter(),
		engine:   engine.NewEngine(nil, zap.NewNop(), nil),
		db:       db,
		testMode: testMode,
	}
	srv.registerS3CatchAll()

	return &corsFixture{server: srv, db: db, tenantID: tenantID,
		tenant: &tenant.Tenant{ID: tenantID, Namespace: "tenant/" + tenantID + "/"}, bucket: bucket}
}

func (f *corsFixture) ctx() context.Context { return s3Ctx(context.Background(), f.tenant) }

func (f *corsFixture) setRules(t *testing.T, rulesJSON string) {
	t.Helper()
	_, err := f.db.Exec(`UPDATE buckets SET cors_rules = $1::jsonb WHERE tenant_id = $2 AND name = $3`,
		rulesJSON, f.tenantID, f.bucket)
	require.NoError(t, err)
}

const corsXML = `<CORSConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <CORSRule>
    <ID>app</ID>
    <AllowedOrigin>https://app.example</AllowedOrigin>
    <AllowedMethod>GET</AllowedMethod>
    <AllowedMethod>PUT</AllowedMethod>
    <AllowedHeader>content-type</AllowedHeader>
    <AllowedHeader>x-amz-*</AllowedHeader>
    <ExposeHeader>ETag</ExposeHeader>
    <ExposeHeader>x-amz-version-id</ExposeHeader>
    <MaxAgeSeconds>3000</MaxAgeSeconds>
  </CORSRule>
  <CORSRule>
    <AllowedOrigin>*</AllowedOrigin>
    <AllowedMethod>GET</AllowedMethod>
  </CORSRule>
</CORSConfiguration>`

// --- Put / Get / Delete ------------------------------------------------------

func TestBucketCors_RoundTrip(t *testing.T) {
	f := setupCORSFixture(t, true)
	do := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/"+f.bucket+"?cors", strings.NewReader(body)).WithContext(f.ctx())
		w := httptest.NewRecorder()
		f.server.router.ServeHTTP(w, r)
		return w
	}

	// none yet
	w := do("GET", "")
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "<Code>NoSuchCORSConfiguration</Code>")

	w = do("PUT", corsXML)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = do("GET", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "application/xml", w.Header().Get("Content-Type"))
	body := w.Body.String()
	for _, want := range []string{"<CORSConfiguration", "<ID>app</ID>", "<AllowedOrigin>https://app.example</AllowedOrigin>",
		"<AllowedMethod>PUT</AllowedMethod>", "<AllowedHeader>x-amz-*</AllowedHeader>", "<ExposeHeader>ETag</ExposeHeader>",
		"<MaxAgeSeconds>3000</MaxAgeSeconds>", "<AllowedOrigin>*</AllowedOrigin>"} {
		assert.Contains(t, body, want)
	}
	// the second rule has no MaxAgeSeconds: absent, not 0
	assert.Equal(t, 1, strings.Count(body, "<MaxAgeSeconds>"))

	// stored where the preflight reads it
	var stored string
	require.NoError(t, f.db.QueryRow(`SELECT cors_rules::text FROM buckets WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, f.bucket).Scan(&stored))
	assert.Contains(t, stored, `"allowed_origins"`)

	w = do("DELETE", "")
	assert.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	w = do("GET", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
	// a second DELETE is idempotent
	assert.Equal(t, http.StatusNoContent, do("DELETE", "").Code)

	// a bucket that does not exist
	r := httptest.NewRequest("GET", "/no-such-bkt?cors", nil).WithContext(f.ctx())
	w = httptest.NewRecorder()
	f.server.router.ServeHTTP(w, r)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>NoSuchBucket</Code>")
	r = httptest.NewRequest("PUT", "/no-such-bkt?cors", strings.NewReader(corsXML)).WithContext(f.ctx())
	w = httptest.NewRecorder()
	f.server.router.ServeHTTP(w, r)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>NoSuchBucket</Code>")
}

func TestBucketCors_Validation(t *testing.T) {
	f := setupCORSFixture(t, true)
	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/"+f.bucket+"?cors", strings.NewReader(body)).WithContext(f.ctx())
		w := httptest.NewRecorder()
		f.server.router.ServeHTTP(w, r)
		return w
	}
	rule := func(inner string) string {
		return `<CORSConfiguration><CORSRule>` + inner + `</CORSRule></CORSConfiguration>`
	}
	cases := []struct {
		name, body, code string
	}{
		{"not xml", "not xml at all", ErrMalformedXML},
		{"no rule", `<CORSConfiguration></CORSConfiguration>`, ErrMalformedXML},
		{"no origin", rule(`<AllowedMethod>GET</AllowedMethod>`), ErrMalformedXML},
		{"no method", rule(`<AllowedOrigin>*</AllowedOrigin>`), ErrMalformedXML},
		{"unsupported method", rule(`<AllowedOrigin>*</AllowedOrigin><AllowedMethod>PATCH</AllowedMethod>`), ErrInvalidRequest},
		{"two wildcards in an origin", rule(`<AllowedOrigin>https://*.*.example</AllowedOrigin><AllowedMethod>GET</AllowedMethod>`), ErrInvalidRequest},
		{"two wildcards in a header", rule(`<AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod><AllowedHeader>x-*-*</AllowedHeader>`), ErrInvalidRequest},
		{"wildcard expose header", rule(`<AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod><ExposeHeader>*</ExposeHeader>`), ErrInvalidRequest},
		{"negative max age", rule(`<AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod><MaxAgeSeconds>-1</MaxAgeSeconds>`), ErrInvalidRequest},
	}
	for _, tc := range cases {
		w := put(tc.body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "%s: %s", tc.name, w.Body.String())
		assert.Contains(t, w.Body.String(), "<Code>"+tc.code+"</Code>", tc.name)
	}

	// 101 rules
	var sb strings.Builder
	sb.WriteString(`<CORSConfiguration>`)
	for i := 0; i <= maxCORSRules; i++ {
		sb.WriteString(`<CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule>`)
	}
	sb.WriteString(`</CORSConfiguration>`)
	w := put(sb.String())
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>"+ErrInvalidRequest+"</Code>")

	// nothing was stored by any of them
	var stored sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT cors_rules::text FROM buckets WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, f.bucket).Scan(&stored))
	assert.False(t, stored.Valid, "an invalid configuration must not replace the stored one")

	// methods are case-insensitive on input and stored upper-case, like AWS
	w = put(rule(`<AllowedOrigin>*</AllowedOrigin><AllowedMethod>get</AllowedMethod>`))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, f.db.QueryRow(`SELECT cors_rules::text FROM buckets WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, f.bucket).Scan(&stored))
	assert.Contains(t, stored.String, `"GET"`)
}

// A scoped key must be granted the new operation names like any other
// bucket configuration call (CheckPermission runs on the parser's name).
func TestBucketCors_PermissionNames(t *testing.T) {
	scope := &auth.KeyScope{Permissions: []string{auth.OpGetObject, auth.OpPutObject}}
	for _, op := range []string{auth.OpGetBucketCors, auth.OpPutBucketCors, auth.OpDeleteBucketCors} {
		assert.False(t, auth.CheckPermission(scope.Permissions, op), op)
		assert.True(t, auth.CheckPermission([]string{op}, op), op)
		assert.True(t, auth.CheckPermission([]string{"*"}, op), op)
	}
}

// --- preflight (before SigV4) --------------------------------------------------

const storedRules = `[{"id":"app","allowed_origins":["https://app.example"],"allowed_methods":["GET","PUT"],
 "allowed_headers":["content-type","x-amz-*"],"expose_headers":["ETag","x-amz-version-id"],"max_age_seconds":3000},
 {"allowed_origins":["*"],"allowed_methods":["GET"]}]`

func preflight(f *corsFixture, path, origin, method, headers string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("OPTIONS", path, nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if method != "" {
		r.Header.Set("Access-Control-Request-Method", method)
	}
	if headers != "" {
		r.Header.Set("Access-Control-Request-Headers", headers)
	}
	w := httptest.NewRecorder()
	f.server.router.ServeHTTP(w, r)
	return w
}

func TestCORSPreflight_AnsweredBeforeAuth(t *testing.T) {
	f := setupCORSFixture(t, false) // the real SigV4 gate is on
	f.setRules(t, storedRules)

	// an unsigned GET is refused by the gate …
	r := httptest.NewRequest("GET", "/"+f.bucket+"/photo.jpg", nil)
	w := httptest.NewRecorder()
	f.server.router.ServeHTTP(w, r)
	require.Equal(t, http.StatusForbidden, w.Code)

	// … the unsigned preflight is answered
	w = preflight(f, "/"+f.bucket+"/photo.jpg", "https://app.example", "PUT", "Content-Type, X-Amz-Meta-Album")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	h := w.Header()
	assert.Equal(t, "https://app.example", h.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "GET, PUT", h.Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "Content-Type, X-Amz-Meta-Album", h.Get("Access-Control-Allow-Headers"))
	assert.Equal(t, "ETag, x-amz-version-id", h.Get("Access-Control-Expose-Headers"))
	assert.Equal(t, "3000", h.Get("Access-Control-Max-Age"))
	assert.Equal(t, "true", h.Get("Access-Control-Allow-Credentials"))
	assert.Equal(t, "0", h.Get("Content-Length"))
	vary := strings.Join(h.Values("Vary"), ", ")
	for _, v := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
		assert.Contains(t, vary, v)
	}
	// nothing of the auth path ran: no auth-failure accounting, no body
	assert.Empty(t, w.Body.String())

	// a bucket-level preflight (a multipart create, a listing) too
	w = preflight(f, "/"+f.bucket, "https://app.example", "GET", "")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Headers"), "no requested headers → none echoed")
	assert.Equal(t, "3000", w.Header().Get("Access-Control-Max-Age"), "the first rule allows GET for this origin")

	// the wildcard rule: `*` is returned as is, no credentials
	w = preflight(f, "/"+f.bucket+"/photo.jpg", "https://viewer.example", "GET", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Credentials"))
	assert.Equal(t, "GET", w.Header().Get("Access-Control-Allow-Methods"))
}

func TestCORSPreflight_Refusals(t *testing.T) {
	f := setupCORSFixture(t, false)
	f.setRules(t, storedRules)

	refused := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		assert.Equal(t, http.StatusForbidden, w.Code, "%s: %s", name, w.Body.String())
		assert.Contains(t, w.Body.String(), "<Code>AccessForbidden</Code>", name)
		assert.Contains(t, w.Body.String(), "CORSResponse", name)
		assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"), name)
	}
	refused("origin not allowed", preflight(f, "/"+f.bucket+"/k", "https://evil.example", "PUT", ""))
	refused("method not allowed for that origin", preflight(f, "/"+f.bucket+"/k", "https://app.example", "DELETE", ""))
	refused("requested header not allowed", preflight(f, "/"+f.bucket+"/k", "https://app.example", "PUT", "X-Custom-Thing"))
	refused("bucket without a configuration", preflight(f, "/some-other-bucket/k", "https://app.example", "GET", ""))
	refused("no bucket in the path", preflight(f, "/", "https://app.example", "GET", ""))

	// no Origin / no requested method: not a preflight at all (AWS: 400)
	w := preflight(f, "/"+f.bucket+"/k", "", "PUT", "")
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	w = preflight(f, "/"+f.bucket+"/k", "https://app.example", "", "")
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// The /cdn path keeps its own CORS (cors_origins): the S3 preflight route
// must not capture it.
func TestCORSPreflight_DoesNotCaptureCDN(t *testing.T) {
	f := setupCORSFixture(t, false)
	called := false
	f.server.router.Options("/cdn/{slug}/{bucket}/*", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	w := preflight(f, "/cdn/slug/bkt/key", "https://x.example", "GET", "")
	assert.True(t, called)
	assert.Equal(t, http.StatusNoContent, w.Code)
}

// --- actual responses ---------------------------------------------------------

func TestCORSActualResponse_HeadersOnEveryStatus(t *testing.T) {
	f := setupCORSFixture(t, false)
	f.setRules(t, storedRules)

	// the 403 of the auth gate carries the headers (a browser can read it)
	r := httptest.NewRequest("PUT", "/"+f.bucket+"/photo.jpg", strings.NewReader("x"))
	r.Header.Set("Origin", "https://app.example")
	w := httptest.NewRecorder()
	f.server.router.ServeHTTP(w, r)
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "https://app.example", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "ETag, x-amz-version-id", w.Header().Get("Access-Control-Expose-Headers"))
	assert.Equal(t, "true", w.Header().Get("Access-Control-Allow-Credentials"))
	assert.Contains(t, strings.Join(w.Header().Values("Vary"), ","), "Origin")

	// an origin no rule allows: no CORS headers at all (the browser refuses)
	r = httptest.NewRequest("PUT", "/"+f.bucket+"/photo.jpg", strings.NewReader("x"))
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	f.server.router.ServeHTTP(w, r)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))

	// a method the origin's rule lacks: no headers either (DELETE is not in rule 1)
	r = httptest.NewRequest("DELETE", "/"+f.bucket+"/photo.jpg", nil)
	r.Header.Set("Origin", "https://app.example")
	w = httptest.NewRecorder()
	f.server.router.ServeHTTP(w, r)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))

	// no Origin header: a plain S3 client, nothing added
	r = httptest.NewRequest("GET", "/"+f.bucket+"/photo.jpg", nil)
	w = httptest.NewRecorder()
	f.server.router.ServeHTTP(w, r)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, w.Header().Get("Vary"))
}

func TestCORSActualResponse_OnASuccess(t *testing.T) {
	f := setupCORSFixture(t, true) // auth skipped: the listing succeeds
	f.setRules(t, storedRules)
	r := httptest.NewRequest("GET", "/"+f.bucket+"?list-type=2", nil).WithContext(f.ctx())
	r.Header.Set("Origin", "https://app.example")
	w := httptest.NewRecorder()
	f.server.router.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "https://app.example", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "ETag, x-amz-version-id", w.Header().Get("Access-Control-Expose-Headers"))
}

// Bucket names are per tenant (the PK is tenant_id + name); a preflight
// carries no credential, so it is answered for the bucket NAME: any tenant's
// configuration under that name that allows the origin and method lets the
// browser send the real, signed request, which is then authorised as usual.
func TestCORSPreflight_ByBucketNameAcrossTenants(t *testing.T) {
	f := setupCORSFixture(t, false)
	other := f.tenantID + "-b"
	_, err := f.db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING`, other, "CORS other", other+"@test.local", "AK-"+other, "SK-"+other)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, cors_rules) VALUES ($1, $2, 'private', $3::jsonb)
		ON CONFLICT (tenant_id, name) DO NOTHING`, other, f.bucket,
		`[{"allowed_origins":["https://second.example"],"allowed_methods":["DELETE"]}]`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec("DELETE FROM buckets WHERE tenant_id = $1", other)
		_, _ = f.db.Exec("DELETE FROM tenants WHERE id = $1", other)
	})
	// f's bucket has no configuration; the other tenant's same-named bucket has
	w := preflight(f, "/"+f.bucket+"/k", "https://second.example", "DELETE", "")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "https://second.example", w.Header().Get("Access-Control-Allow-Origin"))
	w = preflight(f, "/"+f.bucket+"/k", "https://second.example", "PUT", "")
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// Without a database (the degraded dev path) a preflight is refused and an
// actual response carries nothing — never a panic.
func TestCORS_NoDatabase(t *testing.T) {
	srv := &Server{logger: zap.NewNop(), router: chi.NewRouter(),
		engine: engine.NewEngine(nil, zap.NewNop(), nil), testMode: true}
	srv.registerS3CatchAll()
	f := &corsFixture{server: srv, bucket: "b"}
	w := preflight(f, "/b/k", "https://app.example", "GET", "")
	assert.Equal(t, http.StatusForbidden, w.Code)
	r := httptest.NewRequest("GET", "/b?cors", nil)
	r.Header.Set("Origin", "https://app.example")
	w = httptest.NewRecorder()
	srv.router.ServeHTTP(w, r)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}
