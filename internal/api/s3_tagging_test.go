package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

type taggingFixture struct {
	server   *Server
	db       *sql.DB
	tenantID string
	tenant   *tenant.Tenant
	bucket   string
	object   string
}

func setupTaggingFixture(t *testing.T) *taggingFixture {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	logger := zap.NewNop()
	eng := engine.NewEngine(nil, logger, nil)

	tenantID := fmt.Sprintf("tag-%d", os.Getpid())
	bucket := "tag-bucket"
	object := "doc.txt"
	email := fmt.Sprintf("tag-%d@test.local", os.Getpid())

	_, err = db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, tenantID, "Tagging Test", email, "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility)
		VALUES ($1, $2, 'private')
		ON CONFLICT (tenant_id, name) DO NOTHING
	`, tenantID, bucket)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, $3, 5, 'abc123', 'text/plain')
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET tags = '{}'
	`, tenantID, bucket, object)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM buckets WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})

	tn := &tenant.Tenant{
		ID:        tenantID,
		Namespace: "tenant/" + tenantID + "/",
	}

	srv := &Server{
		logger:   logger,
		router:   chi.NewRouter(),
		engine:   eng,
		db:       db,
		testMode: true,
	}

	return &taggingFixture{
		server:   srv,
		db:       db,
		tenantID: tenantID,
		tenant:   tn,
		bucket:   bucket,
		object:   object,
	}
}

func (f *taggingFixture) get(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	s3Req := &S3Request{Bucket: f.bucket, Object: f.object, TenantID: f.tenantID}
	ctx := tenant.WithTenant(context.Background(), f.tenant)
	r := httptest.NewRequest("GET", "/"+f.bucket+"/"+f.object+"?tagging", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handleGetObjectTagging(w, r, s3Req)
	return w
}

func (f *taggingFixture) put(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	s3Req := &S3Request{Bucket: f.bucket, Object: f.object, TenantID: f.tenantID}
	ctx := tenant.WithTenant(context.Background(), f.tenant)
	r := httptest.NewRequest("PUT", "/"+f.bucket+"/"+f.object+"?tagging", bytes.NewReader([]byte(body))).WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handlePutObjectTagging(w, r, s3Req)
	return w
}

func taggingXML(tags ...[2]string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><TagSet>`)
	for _, tag := range tags {
		fmt.Fprintf(&b, `<Tag><Key>%s</Key><Value>%s</Value></Tag>`, tag[0], tag[1])
	}
	b.WriteString(`</TagSet></Tagging>`)
	return b.String()
}

func TestGetObjectTagging_NoTags(t *testing.T) {
	f := setupTaggingFixture(t)

	w := f.get(t)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/xml")

	var resp Tagging
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.TagSet.Tags)
}

func TestPutObjectTagging_Valid(t *testing.T) {
	f := setupTaggingFixture(t)

	w := f.put(t, taggingXML([2]string{"env", "prod"}, [2]string{"team", "storage"}, [2]string{"cost", "infra"}))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "null", w.Header().Get("x-amz-version-id"))
}

func TestGetObjectTagging_AfterPut(t *testing.T) {
	f := setupTaggingFixture(t)

	wp := f.put(t, taggingXML([2]string{"env", "prod"}, [2]string{"team", "storage"}))
	require.Equal(t, http.StatusOK, wp.Code)

	w := f.get(t)
	require.Equal(t, http.StatusOK, w.Code)

	var resp Tagging
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.TagSet.Tags, 2)

	got := map[string]string{}
	for _, tag := range resp.TagSet.Tags {
		got[tag.Key] = tag.Value
	}
	assert.Equal(t, "prod", got["env"])
	assert.Equal(t, "storage", got["team"])
}

func TestPutObjectTagging_TooMany(t *testing.T) {
	f := setupTaggingFixture(t)

	tags := make([][2]string, 11)
	for i := range tags {
		tags[i] = [2]string{fmt.Sprintf("k%d", i), "v"}
	}
	w := f.put(t, taggingXML(tags...))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), ErrInvalidTag)
}

func TestPutObjectTagging_KeyTooLong(t *testing.T) {
	f := setupTaggingFixture(t)

	w := f.put(t, taggingXML([2]string{strings.Repeat("k", 129), "v"}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), ErrInvalidTag)
}

func TestPutObjectTagging_ValueTooLong(t *testing.T) {
	f := setupTaggingFixture(t)

	w := f.put(t, taggingXML([2]string{"k", strings.Repeat("v", 257)}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), ErrInvalidTag)
}

func TestDeleteObjectTagging(t *testing.T) {
	f := setupTaggingFixture(t)

	wp := f.put(t, taggingXML([2]string{"env", "prod"}))
	require.Equal(t, http.StatusOK, wp.Code)

	s3Req := &S3Request{Bucket: f.bucket, Object: f.object, TenantID: f.tenantID}
	ctx := tenant.WithTenant(context.Background(), f.tenant)
	r := httptest.NewRequest("DELETE", "/"+f.bucket+"/"+f.object+"?tagging", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handleDeleteObjectTagging(w, r, s3Req)
	assert.Equal(t, http.StatusNoContent, w.Code)

	wg := f.get(t)
	require.Equal(t, http.StatusOK, wg.Code)
	var resp Tagging
	require.NoError(t, xml.Unmarshal(wg.Body.Bytes(), &resp))
	assert.Empty(t, resp.TagSet.Tags)
}

func TestPutObjectTagging_ObjectNotFound(t *testing.T) {
	f := setupTaggingFixture(t)

	s3Req := &S3Request{Bucket: f.bucket, Object: "does-not-exist.txt", TenantID: f.tenantID}
	ctx := tenant.WithTenant(context.Background(), f.tenant)
	r := httptest.NewRequest("PUT", "/"+f.bucket+"/does-not-exist.txt?tagging",
		bytes.NewReader([]byte(taggingXML([2]string{"env", "prod"})))).WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handlePutObjectTagging(w, r, s3Req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), ErrNoSuchKey)
}
