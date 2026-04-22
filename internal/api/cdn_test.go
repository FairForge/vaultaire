package api

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

func cdnTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	err = db.Ping()
	require.NoError(t, err)
	return db
}

type cdnTestFixture struct {
	server   *Server
	router   chi.Router
	db       *sql.DB
	tenantID string
	slug     string
	bucket   string
	key      string
	content  []byte
}

func setupCDNFixture(t *testing.T) *cdnTestFixture {
	t.Helper()

	db := cdnTestDB(t)
	logger := zap.NewNop()

	// Create temp dir for local storage.
	tempDir, err := os.MkdirTemp("", "vaultaire-cdn-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	eng := engine.NewEngine(nil, logger, nil)
	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	tenantID := "cdn-test-tenant-" + t.Name()
	slug := "cdn-test-slug-" + t.Name()
	bucket := "public-bucket"
	key := "hello.txt"
	content := []byte("hello from CDN")

	// Seed tenant with slug.
	_, err = db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key, slug, slug_locked)
		VALUES ($1, $2, $3, $4, $5, $6, true)
		ON CONFLICT (id) DO UPDATE SET slug = $6
	`, tenantID, "CDN Test", "cdn@test.local", "AK-"+tenantID, "SK-"+tenantID, slug)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM buckets WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})

	// Seed public bucket.
	_, err = db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility, cors_origins, cache_max_age_secs)
		VALUES ($1, $2, 'public-read', 'https://example.com', 7200)
		ON CONFLICT (tenant_id, name) DO UPDATE SET visibility = 'public-read', cors_origins = 'https://example.com', cache_max_age_secs = 7200
	`, tenantID, bucket)
	require.NoError(t, err)

	// Seed private bucket.
	_, err = db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility)
		VALUES ($1, $2, 'private')
		ON CONFLICT (tenant_id, name) DO UPDATE SET visibility = 'private'
	`, tenantID, "private-bucket")
	require.NoError(t, err)

	// Seed object in head cache.
	_, err = db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, etag = $5, content_type = $6
	`, tenantID, bucket, key, len(content), "abc123", "text/plain")
	require.NoError(t, err)

	// Write object to local storage (namespaced: tenantID_bucket/key).
	container := tenantID + "_" + bucket
	containerDir := filepath.Join(tempDir, container)
	require.NoError(t, os.MkdirAll(containerDir, 0755))

	ctx := context.Background()
	_, err = eng.Put(ctx, container, key, bytes.NewReader(content))
	require.NoError(t, err)

	rl := NewRateLimiter()
	bt := NewBandwidthTracker(nil)
	bt.SetLogger(logger)

	s := &Server{
		logger:           logger,
		router:           chi.NewRouter(),
		db:               db,
		engine:           eng,
		bandwidthTracker: bt,
		cdnRateLimiter:   rl,
	}

	s.router.Get("/cdn/{slug}/{bucket}/*", s.handleCDNRequest)
	s.router.Head("/cdn/{slug}/{bucket}/*", s.handleCDNRequest)

	return &cdnTestFixture{
		server:   s,
		router:   s.router,
		db:       db,
		tenantID: tenantID,
		slug:     slug,
		bucket:   bucket,
		key:      key,
		content:  content,
	}
}

func TestCDN_GetPublicObject(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	body, err := io.ReadAll(w.Body)
	require.NoError(t, err)
	assert.Equal(t, f.content, body)

	assert.Equal(t, "text/plain", w.Header().Get("Content-Type"))
	assert.Equal(t, `"abc123"`, w.Header().Get("ETag"))
	assert.Contains(t, w.Header().Get("Cache-Control"), "public")
	assert.Contains(t, w.Header().Get("Cache-Control"), "max-age=7200")
	assert.Contains(t, w.Header().Get("Cache-Control"), "stale-while-revalidate=600")
	assert.Equal(t, "bytes", w.Header().Get("Accept-Ranges"))
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "noindex, nofollow", w.Header().Get("X-Robots-Tag"))
	assert.Equal(t, "https://example.com", w.Header().Get("Access-Control-Allow-Origin"))
}

func TestCDN_HeadPublicObject(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("HEAD", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	assert.Equal(t, "text/plain", w.Header().Get("Content-Type"))
	assert.Equal(t, `"abc123"`, w.Header().Get("ETag"))
	assert.NotEmpty(t, w.Header().Get("Content-Length"))
	assert.Contains(t, w.Header().Get("Cache-Control"), "public")

	body, _ := io.ReadAll(w.Body)
	assert.Empty(t, body, "HEAD must not return a body")
}

func TestCDN_PrivateBucket_Returns404(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/private-bucket/secret.txt", nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestCDN_UnknownSlug_Returns404(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", "/cdn/nonexistent-slug/"+f.bucket+"/"+f.key, nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestCDN_UnknownBucket_Returns404(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/no-such-bucket/"+f.key, nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestCDN_UnknownKey_Returns404(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/does-not-exist.bin", nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestCDN_404sAreIndistinguishable(t *testing.T) {
	f := setupCDNFixture(t)

	cases := []struct {
		name string
		path string
	}{
		{"unknown slug", "/cdn/no-slug/" + f.bucket + "/" + f.key},
		{"private bucket", "/cdn/" + f.slug + "/private-bucket/file.txt"},
		{"unknown bucket", "/cdn/" + f.slug + "/nope/file.txt"},
		{"unknown key", "/cdn/" + f.slug + "/" + f.bucket + "/missing.bin"},
	}

	var bodies []string
	for _, tc := range cases {
		req := httptest.NewRequest("GET", tc.path, nil)
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code, tc.name)
		b, _ := io.ReadAll(w.Body)
		bodies = append(bodies, string(b))
	}

	for i := 1; i < len(bodies); i++ {
		assert.Equal(t, bodies[0], bodies[i],
			"404 body for %q differs from %q", cases[i].name, cases[0].name)
	}
}

func TestCDN_PostReturns404(t *testing.T) {
	f := setupCDNFixture(t)

	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		req := httptest.NewRequest(method, "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code, "method %s should be 405", method)
	}
}

func TestCDN_BandwidthRecorded(t *testing.T) {
	f := setupCDNFixture(t)

	bt := NewBandwidthTracker(nil)
	bt.SetLogger(zap.NewNop())
	f.server.bandwidthTracker = bt

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	bt.mu.Lock()
	defer bt.mu.Unlock()
	require.NotEmpty(t, bt.buffer, "bandwidth event should be buffered")
	assert.Equal(t, f.tenantID, bt.buffer[0].tenantID)
	assert.Equal(t, int64(0), bt.buffer[0].ingress)
	assert.Equal(t, int64(len(f.content)), bt.buffer[0].egress)
}

func TestCDN_NestedKeyPath(t *testing.T) {
	f := setupCDNFixture(t)

	nestedKey := "images/photos/sunset.jpg"
	content := []byte("fake jpeg data")

	container := f.tenantID + "_" + f.bucket
	ctx := context.Background()
	_, err := f.server.engine.Put(ctx, container, nestedKey, bytes.NewReader(content))
	require.NoError(t, err)

	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, etag = $5, content_type = $6
	`, f.tenantID, f.bucket, nestedKey, len(content), "nested123", "image/jpeg")
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+nestedKey, nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body, _ := io.ReadAll(w.Body)
	assert.Equal(t, content, body)
	assert.Equal(t, "image/jpeg", w.Header().Get("Content-Type"))
}

// --- CDNHostRouter tests (no DB needed) ---

func TestCDNHostRouter_RoutesToCDN(t *testing.T) {
	cdnCalled := false
	cdnHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnCalled = true
		w.WriteHeader(http.StatusOK)
	})
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("fallback should not be called for CDN host")
	})

	handler := CDNHostRouter(cdnHandler, fallback)

	for _, host := range []string{"cdn.stored.ge", "cdn.stored.cloud"} {
		cdnCalled = false
		req := httptest.NewRequest("GET", "/test-slug/test-bucket/file.txt", nil)
		req.Host = host
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		assert.True(t, cdnCalled, "CDN handler should be called for host %s", host)
	}
}

func TestCDNHostRouter_FallsBackForOtherHosts(t *testing.T) {
	cdnHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("CDN handler should not be called for non-CDN host")
	})
	fallbackCalled := false
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled = true
		w.WriteHeader(http.StatusOK)
	})

	handler := CDNHostRouter(cdnHandler, fallback)

	for _, host := range []string{"stored.ge", "api.stored.ge", "localhost:8000", ""} {
		fallbackCalled = false
		req := httptest.NewRequest("GET", "/some/path", nil)
		req.Host = host
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		assert.True(t, fallbackCalled, "fallback should be called for host %q", host)
	}
}

func TestCDNHostRouter_StripsPort(t *testing.T) {
	cdnCalled := false
	cdnHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnCalled = true
	})
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("fallback should not be called")
	})

	handler := CDNHostRouter(cdnHandler, fallback)

	req := httptest.NewRequest("GET", "/slug/bucket/key", nil)
	req.Host = "cdn.stored.ge:443"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.True(t, cdnCalled)
}

func TestCDN_RangeRequest_PartialContent(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	req.Header.Set("Range", "bytes=0-4")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusPartialContent, w.Code)

	body, err := io.ReadAll(w.Body)
	require.NoError(t, err)
	assert.Equal(t, f.content[:5], body)

	assert.Equal(t, fmt.Sprintf("bytes 0-4/%d", len(f.content)), w.Header().Get("Content-Range"))
	assert.Equal(t, "5", w.Header().Get("Content-Length"))
	assert.Equal(t, "text/plain", w.Header().Get("Content-Type"))
}

func TestCDN_RangeRequest_MiddleRange(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	req.Header.Set("Range", "bytes=6-8")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusPartialContent, w.Code)

	body, err := io.ReadAll(w.Body)
	require.NoError(t, err)
	assert.Equal(t, f.content[6:9], body)
}

func TestCDN_RangeRequest_SuffixRange(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	req.Header.Set("Range", "bytes=-3")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusPartialContent, w.Code)

	body, err := io.ReadAll(w.Body)
	require.NoError(t, err)
	assert.Equal(t, f.content[len(f.content)-3:], body)
}

func TestCDN_RangeRequest_Unsatisfiable(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	req.Header.Set("Range", "bytes=9999-10000")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, w.Code)
	assert.Contains(t, w.Header().Get("Content-Range"), fmt.Sprintf("bytes */%d", len(f.content)))
}

func TestCDN_RangeRequest_NoRangeHeaderStillReturns200(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	body, err := io.ReadAll(w.Body)
	require.NoError(t, err)
	assert.Equal(t, f.content, body)
}

func TestCDN_MissingDBReturns404(t *testing.T) {
	logger := zap.NewNop()

	s := &Server{
		logger:         logger,
		router:         chi.NewRouter(),
		db:             nil,
		cdnRateLimiter: NewRateLimiter(),
	}
	s.router.Get("/cdn/{slug}/{bucket}/*", s.handleCDNRequest)

	req := httptest.NewRequest("GET", "/cdn/slug/bucket/key.txt", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}
