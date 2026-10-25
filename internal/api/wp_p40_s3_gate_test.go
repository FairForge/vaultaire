package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/testutil"
)

// Stage 1a Prompt 40 Part 0 item 1 (RP56-1, the WP-P39 review, proven on
// prod 2026-10-24): the web routes — the dashboard's public pages, /docs,
// /status, /changelog, /abuse, /llms.txt, /legal/*, /health, /version,
// /metrics — are chi routes ahead of the S3 catch-all on EVERY host, and
// none of them looked at credentials. A customer's SIGNED GetObject of key
// "privacy" in bucket "legal" got 200 with the privacy page as the object's
// bytes; ListObjects of bucket "docs" a page; every key of bucket
// "dashboard" a 303. Now one router-level gate sends a request carrying
// AWS-scheme credentials to the S3 API before any route matches.

// gateServer is NewServer on the test database with s3Fallback recording
// every request the S3 API was handed.
func gateServer(t *testing.T, env map[string]string) (*Server, *[]string) {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — make test-db", err)
	}
	prev := hosts.Get()
	t.Cleanup(func() { hosts.Set(prev) })
	for _, k := range []string{"VAULTAIRE_PUBLIC_HOST", "VAULTAIRE_ENDPOINT", "VAULTAIRE_BASE_URL", "CDN_PUBLIC_HOST", "TRUST_PUBLIC_HOST", "VAULTAIRE_ALIAS_HOSTS"} {
		t.Setenv(k, env[k])
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"}), nil, db)
	var hits []string
	srv.s3Fallback = func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.Host+r.URL.Path)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusTeapot) // the S3 API's, whatever it would answer
	}
	return srv, &hits
}

// routePaths is every route registered on the main router before the S3
// catch-all, from chi.Walk — never a typed list — as a concrete path: a
// {param} becomes "x", a trailing * becomes "k".
func routePaths(t *testing.T, srv *Server) [][2]string {
	t.Helper()
	param := regexp.MustCompile(`\{[^}]*\}`)
	var out [][2]string
	require.NoError(t, chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route == "/*" || method == http.MethodOptions {
			return nil // the catch-all itself; the CORS preflight is answered before any auth
		}
		p := param.ReplaceAllString(route, "x")
		if strings.HasSuffix(p, "/*") {
			p = strings.TrimSuffix(p, "*") + "k"
		}
		out = append(out, [2]string{method, p})
		return nil
	}))
	require.Greater(t, len(out), 100, "the walk found the router's routes")
	return out
}

// the routes outside the S3 API that verify an AWS-scheme signature
// themselves; the gate leaves them to their route.
func ownsSigV4(method, path string) bool {
	return method == http.MethodGet && path == whoamiPath
}

func sigV4Signed(t *testing.T, method, host, path string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Host = host
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	require.NoError(t, signer.SignHTTP(context.Background(), aws.Credentials{AccessKeyID: "AKIDNOPE", SecretAccessKey: "nope"},
		req, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Now().UTC()))
	return req
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestS3Gate_ASignedRequestToEveryRouteIsTheS3APIs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		hosts []string
	}{
		{"prod today (web host = S3 host)", prodTodayHosts, []string{"stored.ge"}},
		{"prod target", prodTargetHosts, []string{"s3.stored.cloud", "stored.ge", "stored.cloud"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := gateServer(t, tc.env)
			h := srv.httpServer.Handler
			for _, host := range tc.hosts {
				for _, rp := range routePaths(t, srv) {
					method, path := rp[0], rp[1]
					reqs := map[string]*http.Request{"sigv4 header": sigV4Signed(t, method, host, path)}
					v2 := httptest.NewRequest(method, path, nil)
					v2.Host, v2.Header = host, http.Header{"Authorization": {"AWS AKIDNOPE:c2ln"}}
					reqs["sigv2 header"] = v2
					v4a := httptest.NewRequest(method, path, nil)
					v4a.Host, v4a.Header = host, http.Header{"Authorization": {sigV4AHeader}}
					reqs["sigv4a header"] = v4a
					if method == http.MethodGet || method == http.MethodHead {
						q := signPresignedURL(method, path, host, "AKIDNOPE", "nope", 300, time.Now().UTC())
						pre := httptest.NewRequest(method, path+"?"+q.Encode(), nil)
						pre.Host = host
						reqs["presigned"] = pre
						v2q := httptest.NewRequest(method, path+"?AWSAccessKeyId=AKIDNOPE&Signature=c2ln&Expires=1900000000", nil)
						v2q.Host = host
						reqs["sigv2 presigned"] = v2q
					}
					for kind, req := range reqs {
						*hits = nil
						w := serve(h, req)
						if ownsSigV4(method, path) {
							assert.Empty(t, *hits, "%s %s%s (%s): whoami verifies its own signature", method, host, path, kind)
							continue
						}
						assert.Equal(t, []string{method + " " + host + path}, *hits,
							"%s %s%s (%s): a request carrying S3 credentials is the S3 API's (answered %d %s)",
							method, host, path, kind, w.Code, w.Header().Get("Content-Type"))
					}
				}
			}
		})
	}
}

// The review's own rows, named: what a customer's signed request got on
// prod on 2026-10-24 (200 text/html, or a 303), and now the S3 API.
func TestS3Gate_TheReviewsRowsAreTheS3APIs(t *testing.T) {
	srv, hits := gateServer(t, prodTodayHosts)
	h := srv.httpServer.Handler
	for _, p := range []string{"/docs", "/docs?list-type=2", "/status", "/login", "/changelog", "/abuse",
		"/legal/privacy", "/dashboard", "/dashboard/x", "/register", "/llms.txt", "/health", "/version", "/metrics", "/"} {
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			*hits = nil
			req := sigV4Signed(t, m, "stored.ge", p)
			serve(h, req)
			assert.Len(t, *hits, 1, "%s %s: the S3 API's", m, p)
		}
	}
}

// An unsigned browser page view of the same paths still gets the page, and
// nothing of the web side moves: the gate looks only at AWS-scheme
// credentials.
func TestS3Gate_AnUnsignedBrowserStillGetsThePage(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		host string
	}{
		{"prod today", prodTodayHosts, "stored.ge"},
		{"prod target", prodTargetHosts, "stored.cloud"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := gateServer(t, tc.env)
			h := srv.httpServer.Handler
			for _, rp := range routePaths(t, srv) {
				method, path := rp[0], rp[1]
				if method != http.MethodGet && method != http.MethodHead {
					continue
				}
				if srv.hostSet().TrustPublic() && strings.HasPrefix(path, "/translog/") {
					continue // on a public origin the log's paths are the S3 API's off the trust host (K4b), signed or not
				}
				*hits = nil
				req := httptest.NewRequest(method, path, nil)
				req.Host = tc.host
				req.Header.Set("Accept", browserAccept)
				serve(h, req)
				assert.Empty(t, *hits, "%s %s%s: an unsigned browser request is not sent to the S3 API by the gate", method, tc.host, path)
			}
			for path, want := range map[string]int{
				"/docs": http.StatusOK, "/status": http.StatusOK, "/changelog": http.StatusOK, "/legal/privacy": http.StatusOK,
				"/llms.txt": http.StatusOK, "/health": http.StatusOK, "/version": http.StatusOK, "/login": http.StatusOK,
			} {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Host = tc.host
				req.Header.Set("Accept", browserAccept)
				w := serve(h, req)
				assert.Equal(t, want, w.Code, path)
				assert.NotContains(t, w.Body.String(), "<Error>", path)
			}
		})
	}
}

// A Bearer JWT, a Basic header, an API-key header or a session cookie is
// never S3: /api/v1/…, /auth/…, /webhook/… keep their own auth. (The gate
// on ANY Authorization — signedS3Request before RP56-1 — sends these to S3.)
func TestS3Gate_ABearerRequestKeepsItsRoute(t *testing.T) {
	srv, hits := gateServer(t, prodTodayHosts)
	h := srv.httpServer.Handler
	for _, auth := range []string{"Bearer not-a-jwt", "Basic dTpw"} {
		*hits = nil
		req := httptest.NewRequest(http.MethodGet, "/api/v1/manage/buckets", nil)
		req.Host = "stored.ge"
		req.Header.Set("Authorization", auth)
		w := serve(h, req)
		assert.Empty(t, *hits, "%s: the management API's", auth)
		assert.Equal(t, http.StatusUnauthorized, w.Code, "%s: the management API refuses the token itself: %s", auth, w.Body.String())
		assert.NotContains(t, w.Body.String(), "<Error>", auth)

		*hits = nil
		req = httptest.NewRequest(http.MethodGet, "/docs", nil)
		req.Host = "stored.ge"
		req.Header.Set("Authorization", auth)
		w = serve(h, req)
		assert.Empty(t, *hits, "%s /docs: not S3", auth)
		assert.Equal(t, http.StatusOK, w.Code, "%s /docs", auth)
	}
}

// The trust host is never the S3 API (contract rule 11): a signed request
// there is still its 404, on a public origin.
func TestS3Gate_TheTrustHostIsNeverTheS3API(t *testing.T) {
	srv, hits := gateServer(t, prodTargetHosts)
	h := srv.httpServer.Handler
	for _, p := range []string{"/translog/checkpoint", "/docs", "/legal/privacy", "/x"} {
		*hits = nil
		w := serve(h, sigV4Signed(t, http.MethodGet, "trust.stored.cloud", p))
		assert.Empty(t, *hits, p)
		assert.Equal(t, http.StatusNotFound, w.Code, p)
	}
	// Behind TrustHostRouter, the main router's translog branch still 404s
	// a signed request on the trust host (the gate leaves it there).
	*hits = nil
	w := serve(srv.router, sigV4Signed(t, http.MethodGet, "trust.stored.cloud", "/translog/checkpoint"))
	assert.Empty(t, *hits)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// The CORS preflight is answered before any authentication, whatever the
// request carries.
func TestS3Gate_OptionsIsThePreflights(t *testing.T) {
	srv, hits := gateServer(t, prodTodayHosts)
	req := httptest.NewRequest(http.MethodOptions, "/docs/key", nil)
	req.Host = "stored.ge"
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDNOPE/20261024/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00")
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	serve(srv.httpServer.Handler, req)
	assert.Empty(t, *hits, "OPTIONS is the preflight's")
}

// --- The real round trip: a test tenant, real SigV4 signatures, the whole
// router, the S3 handler on a local backend.

type gateTenant struct {
	t        *testing.T
	ts       *httptest.Server
	host     string
	ak, sk   string
	tenantID string
	db       *sql.DB
}

// ownBucket makes the tenant own a bucket of this name as if created before
// Prompt 41 item 5 reserved the web paths' names (an existing bucket keeps
// working; the fixture's cleanup deletes the tenant's rows).
func (g *gateTenant) ownBucket(name string) {
	g.t.Helper()
	_, err := g.db.Exec(`INSERT INTO buckets (tenant_id, name, region) VALUES ($1, $2, 'us-central-1')`, g.tenantID, name)
	require.NoError(g.t, err)
}

func newGateTenant(t *testing.T, env map[string]string, host string) *gateTenant {
	t.Helper()
	if testing.Short() {
		t.Skip("DB-backed")
	}
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — make test-db", err)
	}
	prev := hosts.Get()
	t.Cleanup(func() { hosts.Set(prev) })
	for _, k := range []string{"VAULTAIRE_PUBLIC_HOST", "VAULTAIRE_ENDPOINT", "VAULTAIRE_BASE_URL", "CDN_PUBLIC_HOST", "TRUST_PUBLIC_HOST", "VAULTAIRE_ALIAS_HOSTS"} {
		t.Setenv(k, env[k])
	}
	dir := t.TempDir()
	t.Setenv("DATA_PATH", dir)
	eng := engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"})
	eng.AddDriver("local", drivers.NewLocalDriver(dir, zap.NewNop()))
	eng.SetPrimary("local")
	srv := NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.NewNop(), eng, nil, db)

	id := uuid.New().String()
	g := &gateTenant{t: t, host: host, tenantID: "gate-" + id[:8], ak: "VKGATE" + strings.ToUpper(id[:8]), sk: "SK" + id, db: db}
	email := "gate-" + id[:8] + "@test.local"
	exec := func(q string, args ...any) {
		_, err := db.Exec(q, args...)
		require.NoError(t, err, q)
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, id, email)
	exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, 'Gate Test', $2, $3, $4)`, g.tenantID, email, g.ak, g.sk)
	exec(`INSERT INTO api_keys (id, user_id, tenant_id, is_primary, name, key_id, secret_hash, secret_key) VALUES ($1, $2, $3, TRUE, 'primary', $4, 'h', $5)`,
		uuid.New().String(), id, g.tenantID, g.ak, g.sk)
	g.ts = httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(func() {
		g.ts.Close()
		time.Sleep(50 * time.Millisecond) // emitEvent's webhook lookup runs detached
		for _, q := range []string{
			`DELETE FROM events WHERE tenant_id = $1`,
			`DELETE FROM retired_blobs WHERE tenant_id = $1`,
			`DELETE FROM object_versions WHERE tenant_id = $1`,
			`DELETE FROM object_head_cache WHERE tenant_id = $1`,
			`DELETE FROM buckets WHERE tenant_id = $1`,
			`DELETE FROM tenant_quotas WHERE tenant_id = $1`,
			`DELETE FROM audit_logs WHERE tenant_id = $1`,
			`DELETE FROM api_keys WHERE tenant_id = $1`,
			`DELETE FROM tenants WHERE id = $1`,
		} {
			_, _ = db.Exec(q, g.tenantID)
		}
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, id)
	})
	return g
}

func (g *gateTenant) do(method, path string, body []byte) (*http.Response, []byte) {
	g.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, g.ts.URL+path, bytes.NewReader(body))
	require.NoError(g.t, err)
	req.Host = g.host
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	require.NoError(g.t, signer.SignHTTP(context.Background(), aws.Credentials{AccessKeyID: g.ak, SecretAccessKey: g.sk},
		req, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Now().UTC()))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(g.t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(g.t, err)
	return resp, b
}

func TestS3Gate_ARealRoundTripOnWebPathBuckets(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		host string
	}{
		{"prod today: stored.ge", prodTodayHosts, "stored.ge"},
		{"prod target: s3.stored.cloud", prodTargetHosts, "s3.stored.cloud"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateTenant(t, tc.env, tc.host)
			// Since Prompt 41 item 5 a NEW bucket may not take a web path's
			// name (400 InvalidBucketName); the round trip below is on buckets
			// that EXIST under those names, which keep working.
			resp, b := g.do(http.MethodPut, "/legal", nil)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, "CreateBucket legal is reserved now: %s", b)
			assert.Contains(t, string(b), "<Code>InvalidBucketName</Code>")
			for _, name := range []string{"legal", "docs", "dashboard"} {
				g.ownBucket(name)
			}
			// GetObject of key "privacy" in bucket "legal" returns the object.
			want := []byte("the customer's own bytes, not the privacy page\n")
			resp, b = g.do(http.MethodPut, "/legal/privacy", want)
			require.Equal(t, http.StatusOK, resp.StatusCode, "PutObject legal/privacy: %s", b)
			resp, b = g.do(http.MethodGet, "/legal/privacy", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, want, b, "GetObject legal/privacy: the object's bytes")
			resp, _ = g.do(http.MethodHead, "/legal/privacy", nil)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "47", resp.Header.Get("Content-Length"), "HeadObject: the object's size")

			// ListObjectsV2 / HeadBucket of bucket "docs" are the S3 API's XML.
			resp, b = g.do(http.MethodGet, "/docs?list-type=2", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
			var list struct {
				XMLName xml.Name `xml:"ListBucketResult"`
				Name    string   `xml:"Name"`
			}
			require.NoError(t, xml.Unmarshal(b, &list), "ListObjectsV2 docs is XML: %s", b)
			assert.Equal(t, "docs", list.Name)
			resp, _ = g.do(http.MethodHead, "/docs", nil)
			assert.Equal(t, http.StatusOK, resp.StatusCode, "HeadBucket docs")

			// Every key of bucket "dashboard" (was a 303).
			resp, b = g.do(http.MethodPut, "/dashboard/x", []byte("x"))
			require.Equal(t, http.StatusOK, resp.StatusCode, "PutObject dashboard/x: %s", b)
			resp, b = g.do(http.MethodGet, "/dashboard/x", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "x", string(b))
			resp, b = g.do(http.MethodGet, "/dashboard?list-type=2", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Contains(t, string(b), "<Key>x</Key>")

			// A presigned GET of legal/privacy is the S3 API's too.
			u, err := url.Parse(g.ts.URL)
			require.NoError(t, err)
			q := signPresignedURL(http.MethodGet, "/legal/privacy", tc.host, g.ak, g.sk, 300, time.Now().UTC())
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+u.Host+"/legal/privacy?"+q.Encode(), nil)
			require.NoError(t, err)
			req.Host = tc.host
			presp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			pb, _ := io.ReadAll(presp.Body)
			_ = presp.Body.Close()
			assert.Equal(t, http.StatusOK, presp.StatusCode, string(pb))
			assert.Equal(t, want, pb, "presigned GetObject legal/privacy")

			// The operational paths are valid bucket names: a signed request
			// there is the S3 API's too, never handleS3Request's old empty
			// 200 for /health, /ready, /metrics, /version (lab finding A-1).
			for _, p := range []string{"/health", "/ready", "/metrics", "/version", "/llms.txt"} {
				resp, b = g.do(http.MethodGet, p+"?list-type=2", nil)
				assert.Equal(t, http.StatusNotFound, resp.StatusCode, "%s: %s", p, b)
				assert.Contains(t, string(b), "<Code>NoSuchBucket</Code>", p)
			}
			g.ownBucket("version")
			resp, b = g.do(http.MethodGet, "/version?list-type=2", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Contains(t, string(b), "<Name>version</Name>")
			resp, _ = g.do(http.MethodDelete, "/version", nil)
			assert.Equal(t, http.StatusNoContent, resp.StatusCode)

			for _, k := range []string{"/legal/privacy", "/dashboard/x"} {
				resp, _ = g.do(http.MethodDelete, k, nil)
				assert.Equal(t, http.StatusNoContent, resp.StatusCode, k)
			}
			for _, bk := range []string{"/legal", "/docs", "/dashboard"} {
				resp, b = g.do(http.MethodDelete, bk, nil)
				assert.Equal(t, http.StatusNoContent, resp.StatusCode, "DeleteBucket %s: %s", bk, b)
			}
		})
	}
}
