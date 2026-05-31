package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestPutGet_ContentDisposition verifies a Content-Disposition set on PUT is
// stored and returned verbatim on GET.
func TestPutGet_ContentDisposition(t *testing.T) {
	f := setupAdapterFixture(t)
	content := []byte("downloadable report content")
	disposition := `attachment; filename="report.pdf"`

	putReq := httptest.NewRequest("PUT", "/test-bucket/report.bin", bytes.NewReader(content))
	putReq.ContentLength = int64(len(content))
	putReq.Header.Set("Content-Type", "application/octet-stream")
	putReq.Header.Set("Content-Disposition", disposition)
	putReq = putReq.WithContext(tenant.WithTenant(putReq.Context(), f.tenant))
	pw := httptest.NewRecorder()
	f.adapter.HandlePut(pw, putReq, "test-bucket", "report.bin")
	require.Equal(t, http.StatusOK, pw.Code)

	getReq := httptest.NewRequest("GET", "/test-bucket/report.bin", nil)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "report.bin")

	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, disposition, gw.Header().Get("Content-Disposition"))
}

// TestHeadObject_ContentDisposition verifies HEAD returns the stored value.
func TestHeadObject_ContentDisposition(t *testing.T) {
	f := setupAdapterFixture(t)
	disposition := `attachment; filename="head.txt"`

	_, err := f.db.Exec(`
		INSERT INTO object_head_cache
			(tenant_id, bucket, object_key, size_bytes, etag, content_type, content_disposition)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET content_disposition = $7
	`, f.tenantID, "test-bucket", "head.txt", 10, "abc123", "text/plain", disposition)
	require.NoError(t, err)

	srv := &Server{db: f.db, logger: zap.NewNop()}
	req := httptest.NewRequest("HEAD", "/test-bucket/head.txt", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	srv.handleHeadObject(w, req, &S3Request{Bucket: "test-bucket", Object: "head.txt"})

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, disposition, w.Header().Get("Content-Disposition"))
}

// TestGet_ResponseContentDispositionOverride verifies the query-string override
// wins over the stored value.
func TestGet_ResponseContentDispositionOverride(t *testing.T) {
	f := setupAdapterFixture(t)
	content := []byte("data")
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(context.Background(), container, "ovr.bin", bytes.NewReader(content))
	require.NoError(t, err)

	_, err = f.db.Exec(`
		INSERT INTO object_head_cache
			(tenant_id, bucket, object_key, size_bytes, etag, content_type, content_disposition)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, content_disposition = $7
	`, f.tenantID, "test-bucket", "ovr.bin", len(content), "abc123", "application/octet-stream", `attachment; filename="stored.bin"`)
	require.NoError(t, err)

	override := `attachment; filename="override.bin"`
	getReq := httptest.NewRequest("GET", "/test-bucket/ovr.bin", nil)
	getReq.URL.RawQuery = "response-content-disposition=" + url.QueryEscape(override)
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "ovr.bin")

	require.Equal(t, http.StatusOK, gw.Code)
	assert.Equal(t, override, gw.Header().Get("Content-Disposition"))
}

// TestGet_ContentDispositionHeaderInjectionStripped verifies a CRLF-laden
// override value is dropped rather than emitted (header-injection guard).
func TestGet_ContentDispositionHeaderInjectionStripped(t *testing.T) {
	f := setupAdapterFixture(t)
	content := []byte("data")
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(context.Background(), container, "inj.bin", bytes.NewReader(content))
	require.NoError(t, err)

	_, err = f.db.Exec(`
		INSERT INTO object_head_cache
			(tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET size_bytes = $4
	`, f.tenantID, "test-bucket", "inj.bin", len(content), "abc123", "application/octet-stream")
	require.NoError(t, err)

	// Build the request manually so the raw CRLF survives into the query value.
	getReq := httptest.NewRequest("GET", "/test-bucket/inj.bin", nil)
	getReq.URL.RawQuery = "response-content-disposition=attachment\r\nX-Injected: evil"
	getReq = getReq.WithContext(tenant.WithTenant(getReq.Context(), f.tenant))
	gw := httptest.NewRecorder()
	f.adapter.HandleGet(gw, getReq, "test-bucket", "inj.bin")

	require.Equal(t, http.StatusOK, gw.Code)
	assert.Empty(t, gw.Header().Get("Content-Disposition"))
	assert.Empty(t, gw.Header().Get("X-Injected"))
}

// TestCDN_InlineForRenderable verifies a renderable content type gets inline.
func TestCDN_InlineForRenderable(t *testing.T) {
	f := setupCDNFixture(t)
	seedCDNObject(t, f, "pic.png", "image/png", []byte("\x89PNG fake image data"))

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/pic.png", nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "inline", w.Header().Get("Content-Disposition"))
}

// TestCDN_AttachmentForOther verifies a non-renderable content type gets
// attachment with a filename.
func TestCDN_AttachmentForOther(t *testing.T) {
	f := setupCDNFixture(t)
	seedCDNObject(t, f, "archive.zip", "application/zip", []byte("PK fake zip data"))

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/archive.zip", nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, `attachment; filename="archive.zip"`, w.Header().Get("Content-Disposition"))
}

// TestCDN_ForceDownload verifies cdn_force_download=TRUE forces attachment even
// for an otherwise-renderable image.
func TestCDN_ForceDownload(t *testing.T) {
	f := setupCDNFixture(t)
	seedCDNObject(t, f, "forced.png", "image/png", []byte("\x89PNG fake image data"))

	_, err := f.db.Exec(`UPDATE buckets SET cdn_force_download = TRUE WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, f.bucket)
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/forced.png", nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, `attachment; filename="forced.png"`, w.Header().Get("Content-Disposition"))
}

// TestCDN_ActiveContentNotInline verifies scriptable types (HTML, SVG) are never
// served inline on the shared-origin public CDN — inline rendering would execute
// JavaScript in the cdn.stored.ge origin (stored XSS / hosted phishing). They
// must be forced to attachment even though text/* and image/* are otherwise
// renderable.
func TestCDN_ActiveContentNotInline(t *testing.T) {
	f := setupCDNFixture(t)
	seedCDNObject(t, f, "evil.html", "text/html", []byte("<script>alert(document.domain)</script>"))
	seedCDNObject(t, f, "evil.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))

	for _, tc := range []struct{ key, want string }{
		{"evil.html", `attachment; filename="evil.html"`},
		{"evil.svg", `attachment; filename="evil.svg"`},
	} {
		req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+tc.key, nil)
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, "key=%s", tc.key)
		assert.Equal(t, tc.want, w.Header().Get("Content-Disposition"), "key=%s", tc.key)
	}
}

// seedCDNObject inserts an object into the head cache and writes it to the
// fixture's local storage backend.
func seedCDNObject(t *testing.T, f *cdnTestFixture, key, contentType string, content []byte) {
	t.Helper()
	_, err := f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, etag = $5, content_type = $6
	`, f.tenantID, f.bucket, key, len(content), "etag-"+key, contentType)
	require.NoError(t, err)

	container := f.tenantID + "_" + f.bucket
	_, err = f.server.engine.Put(context.Background(), container, key, bytes.NewReader(content))
	require.NoError(t, err)
}
