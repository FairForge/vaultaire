package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/lib/pq"
)

// --- Unit tests for conditional helpers ---

func TestNormalizeETag(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`"abc123"`, "abc123"},
		{`abc123`, "abc123"},
		{`W/"abc123"`, "abc123"},
		{`W/"abc"`, "abc"},
		{`""`, ""},
		{``, ""},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.expected, normalizeETag(tt.input), "normalizeETag(%q)", tt.input)
	}
}

func TestEtagsMatch(t *testing.T) {
	tests := []struct {
		a, b  string
		match bool
	}{
		{`"abc"`, `"abc"`, true},
		{`"abc"`, `abc`, true},
		{`W/"abc"`, `"abc"`, true},
		{`W/"abc"`, `abc`, true},
		{`"abc"`, `"def"`, false},
		{`""`, `""`, false},
		{``, ``, false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.match, etagsMatch(tt.a, tt.b), "etagsMatch(%q, %q)", tt.a, tt.b)
	}
}

func TestCheckIfNoneMatch(t *testing.T) {
	t.Run("matching etag returns true", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-None-Match", `"abc123"`)
		assert.True(t, checkIfNoneMatch(req, "abc123"))
	})

	t.Run("non-matching etag returns false", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-None-Match", `"abc123"`)
		assert.False(t, checkIfNoneMatch(req, "def456"))
	})

	t.Run("wildcard returns true", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-None-Match", "*")
		assert.True(t, checkIfNoneMatch(req, "anything"))
	})

	t.Run("multiple etags with match", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-None-Match", `"aaa", "bbb", "ccc"`)
		assert.True(t, checkIfNoneMatch(req, "bbb"))
	})

	t.Run("multiple etags without match", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-None-Match", `"aaa", "bbb"`)
		assert.False(t, checkIfNoneMatch(req, "ccc"))
	})

	t.Run("weak etag matches", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-None-Match", `W/"abc123"`)
		assert.True(t, checkIfNoneMatch(req, "abc123"))
	})

	t.Run("no header returns false", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		assert.False(t, checkIfNoneMatch(req, "abc123"))
	})
}

func TestCheckIfModifiedSince(t *testing.T) {
	ref := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)

	t.Run("not modified when same time", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-Modified-Since", ref.Format(http.TimeFormat))
		assert.True(t, checkIfModifiedSince(req, ref))
	})

	t.Run("not modified when older", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-Modified-Since", ref.Format(http.TimeFormat))
		assert.True(t, checkIfModifiedSince(req, ref.Add(-time.Hour)))
	})

	t.Run("modified when newer", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-Modified-Since", ref.Format(http.TimeFormat))
		assert.False(t, checkIfModifiedSince(req, ref.Add(time.Hour)))
	})

	t.Run("invalid date returns false", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-Modified-Since", "garbage")
		assert.False(t, checkIfModifiedSince(req, ref))
	})

	t.Run("no header returns false", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		assert.False(t, checkIfModifiedSince(req, ref))
	})
}

func TestCheckIfUnmodifiedSince(t *testing.T) {
	ref := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)

	t.Run("unmodified when same time", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-Unmodified-Since", ref.Format(http.TimeFormat))
		assert.False(t, checkIfUnmodifiedSince(req, ref))
	})

	t.Run("unmodified when older", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-Unmodified-Since", ref.Format(http.TimeFormat))
		assert.False(t, checkIfUnmodifiedSince(req, ref.Add(-time.Hour)))
	})

	t.Run("modified returns true (412)", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-Unmodified-Since", ref.Format(http.TimeFormat))
		assert.True(t, checkIfUnmodifiedSince(req, ref.Add(time.Hour)))
	})
}

func TestCheckIfMatch(t *testing.T) {
	t.Run("matching etag — precondition passes", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "/", nil)
		req.Header.Set("If-Match", `"abc123"`)
		assert.False(t, checkIfMatch(req, "abc123"))
	})

	t.Run("non-matching etag — precondition fails", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "/", nil)
		req.Header.Set("If-Match", `"abc123"`)
		assert.True(t, checkIfMatch(req, "def456"))
	})

	t.Run("wildcard — precondition passes", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "/", nil)
		req.Header.Set("If-Match", "*")
		assert.False(t, checkIfMatch(req, "anything"))
	})

	t.Run("no header — precondition passes", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "/", nil)
		assert.False(t, checkIfMatch(req, "abc"))
	})

	t.Run("multiple etags with match", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "/", nil)
		req.Header.Set("If-Match", `"aaa", "bbb"`)
		assert.False(t, checkIfMatch(req, "bbb"))
	})

	t.Run("multiple etags without match", func(t *testing.T) {
		req := httptest.NewRequest("PUT", "/", nil)
		req.Header.Set("If-Match", `"aaa", "bbb"`)
		assert.True(t, checkIfMatch(req, "ccc"))
	})
}

func TestEvaluateConditionalGET(t *testing.T) {
	ref := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)

	t.Run("304 on If-None-Match match", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-None-Match", `"abc123"`)
		assert.Equal(t, http.StatusNotModified, evaluateConditionalGET(req, "abc123", ref))
	})

	t.Run("304 on If-Modified-Since not modified", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-Modified-Since", ref.Format(http.TimeFormat))
		assert.Equal(t, http.StatusNotModified, evaluateConditionalGET(req, "abc", ref.Add(-time.Hour)))
	})

	t.Run("If-Modified-Since ignored when If-None-Match present", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-None-Match", `"no-match"`)
		req.Header.Set("If-Modified-Since", ref.Format(http.TimeFormat))
		assert.Equal(t, 0, evaluateConditionalGET(req, "abc", ref.Add(-time.Hour)))
	})

	t.Run("412 on If-Unmodified-Since when modified", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-Unmodified-Since", ref.Format(http.TimeFormat))
		assert.Equal(t, http.StatusPreconditionFailed, evaluateConditionalGET(req, "abc", ref.Add(time.Hour)))
	})

	t.Run("412 takes priority over 304", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-Unmodified-Since", ref.Format(http.TimeFormat))
		req.Header.Set("If-None-Match", `"abc"`)
		assert.Equal(t, http.StatusPreconditionFailed, evaluateConditionalGET(req, "abc", ref.Add(time.Hour)))
	})

	t.Run("0 when no conditional headers", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		assert.Equal(t, 0, evaluateConditionalGET(req, "abc", ref))
	})

	t.Run("0 when conditions pass", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("If-None-Match", `"old-etag"`)
		assert.Equal(t, 0, evaluateConditionalGET(req, "new-etag", ref))
	})
}

func TestWriteNotModified(t *testing.T) {
	ref := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	w := httptest.NewRecorder()

	writeNotModified(w, "abc123", ref, "public, max-age=3600")

	assert.Equal(t, http.StatusNotModified, w.Code)
	assert.Equal(t, `"abc123"`, w.Header().Get("ETag"))
	assert.Equal(t, ref.UTC().Format(http.TimeFormat), w.Header().Get("Last-Modified"))
	assert.Equal(t, "public, max-age=3600", w.Header().Get("Cache-Control"))

	body, _ := io.ReadAll(w.Body)
	assert.Empty(t, body, "304 must not include a body")
}

// --- Integration tests for S3 GET conditional requests ---

func TestHandleGet_IfNoneMatch_304(t *testing.T) {
	f := setupAdapterFixture(t)

	content := []byte("conditional content")
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(context.Background(), container, "cond.txt", bytes.NewReader(content))
	require.NoError(t, err)

	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, etag = $5, content_type = $6
	`, f.tenantID, "test-bucket", "cond.txt", len(content), "cond999", "text/plain")
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/test-bucket/cond.txt", nil)
	req.Header.Set("If-None-Match", `"cond999"`)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", "cond.txt")

	assert.Equal(t, http.StatusNotModified, w.Code)
	assert.Equal(t, `"cond999"`, w.Header().Get("ETag"))
	assert.NotEmpty(t, w.Header().Get("Last-Modified"))
	assert.Equal(t, "private, no-cache", w.Header().Get("Cache-Control"))
	body, _ := io.ReadAll(w.Body)
	assert.Empty(t, body)
}

func TestHandleGet_IfNoneMatch_NoMatch_Returns200(t *testing.T) {
	f := setupAdapterFixture(t)

	content := []byte("fresh content")
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(context.Background(), container, "fresh.txt", bytes.NewReader(content))
	require.NoError(t, err)

	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, etag = $5, content_type = $6
	`, f.tenantID, "test-bucket", "fresh.txt", len(content), "fresh999", "text/plain")
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/test-bucket/fresh.txt", nil)
	req.Header.Set("If-None-Match", `"old-etag"`)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", "fresh.txt")

	assert.Equal(t, http.StatusOK, w.Code)
	body, _ := io.ReadAll(w.Body)
	assert.Equal(t, content, body)
}

func TestHandleGet_ETagAndLastModifiedOnResponse(t *testing.T) {
	f := setupAdapterFixture(t)

	content := []byte("header test content")
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(context.Background(), container, "headers.txt", bytes.NewReader(content))
	require.NoError(t, err)

	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, etag = $5, content_type = $6
	`, f.tenantID, "test-bucket", "headers.txt", len(content), "hdr123", "text/plain")
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/test-bucket/headers.txt", nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", "headers.txt")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, `"hdr123"`, w.Header().Get("ETag"))
	assert.NotEmpty(t, w.Header().Get("Last-Modified"))
	assert.Equal(t, "private, no-cache", w.Header().Get("Cache-Control"))
}

func TestHandlePut_IfMatch_412(t *testing.T) {
	f := setupAdapterFixture(t)

	content := []byte("original content")
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(context.Background(), container, "locked.txt", bytes.NewReader(content))
	require.NoError(t, err)

	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, etag = $5, content_type = $6
	`, f.tenantID, "test-bucket", "locked.txt", len(content), "orig111", "text/plain")
	require.NoError(t, err)

	newContent := []byte("updated content")
	req := httptest.NewRequest("PUT", "/test-bucket/locked.txt", bytes.NewReader(newContent))
	req.Header.Set("If-Match", `"wrong-etag"`)
	req.Header.Set("Content-Type", "text/plain")
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "locked.txt")

	assert.Equal(t, http.StatusPreconditionFailed, w.Code)
}

func TestHandlePut_IfMatch_MatchingETag_Succeeds(t *testing.T) {
	f := setupAdapterFixture(t)

	content := []byte("original content")
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(context.Background(), container, "update.txt", bytes.NewReader(content))
	require.NoError(t, err)

	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, etag = $5, content_type = $6
	`, f.tenantID, "test-bucket", "update.txt", len(content), "match111", "text/plain")
	require.NoError(t, err)

	newContent := []byte("updated content")
	req := httptest.NewRequest("PUT", "/test-bucket/update.txt", bytes.NewReader(newContent))
	req.Header.Set("If-Match", `"match111"`)
	req.Header.Set("Content-Type", "text/plain")
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "update.txt")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotEmpty(t, w.Header().Get("ETag"))
}

// --- Integration tests for CDN conditional requests ---

func TestCDN_IfNoneMatch_304(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	req.Header.Set("If-None-Match", `"abc123"`)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotModified, w.Code)
	assert.Equal(t, `"abc123"`, w.Header().Get("ETag"))
	assert.NotEmpty(t, w.Header().Get("Last-Modified"))
	assert.Contains(t, w.Header().Get("Cache-Control"), "public")

	body, _ := io.ReadAll(w.Body)
	assert.Empty(t, body, "304 must not include a body")
}

func TestCDN_IfNoneMatch_NoMatch_Returns200(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	req.Header.Set("If-None-Match", `"stale-etag"`)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body, _ := io.ReadAll(w.Body)
	assert.Equal(t, f.content, body)
}

func TestCDN_IfModifiedSince_304(t *testing.T) {
	f := setupCDNFixture(t)

	var updatedAt time.Time
	err := f.db.QueryRow(`
		SELECT updated_at FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
	`, f.tenantID, f.bucket, f.key).Scan(&updatedAt)
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	req.Header.Set("If-Modified-Since", updatedAt.Add(time.Hour).UTC().Format(http.TimeFormat))
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotModified, w.Code)
	body, _ := io.ReadAll(w.Body)
	assert.Empty(t, body)
}

func TestCDN_IfModifiedSince_Modified_Returns200(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	req.Header.Set("If-Modified-Since", "Mon, 01 Jan 2020 00:00:00 GMT")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body, _ := io.ReadAll(w.Body)
	assert.Equal(t, f.content, body)
}

func TestCDN_LastModifiedHeader(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	lm := w.Header().Get("Last-Modified")
	assert.NotEmpty(t, lm, "CDN response should include Last-Modified")

	parsed, err := http.ParseTime(lm)
	require.NoError(t, err)
	assert.False(t, parsed.IsZero())
}

// --- HeadObject tests ---

func TestHeadObject_LastModifiedFromCache(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	f := setupAdapterFixture(t)

	_, err := f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, etag = $5, content_type = $6, updated_at = $7
	`, f.tenantID, "test-bucket", "dated.txt", 100, "date111", "text/plain",
		time.Date(2026, 4, 15, 10, 30, 0, 0, time.UTC))
	require.NoError(t, err)

	req := httptest.NewRequest("HEAD", "/test-bucket/dated.txt", nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	s := &Server{
		db:     f.db,
		logger: f.adapter.logger,
	}

	w := httptest.NewRecorder()
	s3req := &S3Request{Bucket: "test-bucket", Object: "dated.txt"}
	s.handleHeadObject(w, req, s3req)

	assert.Equal(t, http.StatusOK, w.Code)

	lm := w.Header().Get("Last-Modified")
	require.NotEmpty(t, lm)
	parsed, err := http.ParseTime(lm)
	require.NoError(t, err)
	assert.Equal(t, 2026, parsed.Year())
	assert.Equal(t, time.April, parsed.Month())
	assert.Equal(t, 15, parsed.Day())
}

func TestHandleGet_S3CacheControlHeader(t *testing.T) {
	f := setupAdapterFixture(t)

	content := []byte("cache control test")
	container := f.tenant.NamespaceContainer("test-bucket")
	_, err := f.eng.Put(context.Background(), container, "cc.txt", bytes.NewReader(content))
	require.NoError(t, err)

	_, err = f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes = $4, etag = $5, content_type = $6
	`, f.tenantID, "test-bucket", "cc.txt", len(content), "cc123", "text/plain")
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/test-bucket/cc.txt", nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", "cc.txt")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "private, no-cache", w.Header().Get("Cache-Control"))
}

func TestCDN_HeadObject_ConditionalRequest(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("HEAD", "/cdn/"+f.slug+"/"+f.bucket+"/"+f.key, nil)
	req.Header.Set("If-None-Match", `"abc123"`)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotModified, w.Code)
	body, _ := io.ReadAll(w.Body)
	assert.Empty(t, body)
}

func TestCDN_IfUnmodifiedSince_412(t *testing.T) {
	f := setupCDNFixture(t)

	req := httptest.NewRequest("GET", fmt.Sprintf("/cdn/%s/%s/%s", f.slug, f.bucket, f.key), nil)
	req.Header.Set("If-Unmodified-Since", "Mon, 01 Jan 2020 00:00:00 GMT")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusPreconditionFailed, w.Code)
}
