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
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

type loggingFixture struct {
	server    *Server
	db        *sql.DB
	eng       *engine.CoreEngine
	tenantID  string
	tenant    *tenant.Tenant
	tempDir   string
	bucket    string
	logBucket string
}

func setupLoggingFixture(t *testing.T) *loggingFixture {
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

	tempDir, err := os.MkdirTemp("", "vaultaire-logging-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	eng := engine.NewEngine(nil, logger, nil)
	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	tenantID := fmt.Sprintf("log-%d", os.Getpid())
	bucket := "source-bucket"
	logBucket := "log-bucket"
	email := fmt.Sprintf("log-%d@test.local", os.Getpid())

	_, err = db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, tenantID, "Logging Test", email, "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility)
		VALUES ($1, $2, 'private')
		ON CONFLICT (tenant_id, name) DO NOTHING
	`, tenantID, bucket)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility)
		VALUES ($1, $2, 'private')
		ON CONFLICT (tenant_id, name) DO NOTHING
	`, tenantID, logBucket)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM s3_access_log WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM buckets WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})

	tn := &tenant.Tenant{
		ID:        tenantID,
		Namespace: "tenant/" + tenantID + "/",
	}

	for _, b := range []string{bucket, logBucket} {
		container := tn.NamespaceContainer(b)
		require.NoError(t, os.MkdirAll(filepath.Join(tempDir, container), 0755))
	}

	srv := &Server{
		logger:           logger,
		router:           chi.NewRouter(),
		engine:           eng,
		db:               db,
		testMode:         true,
		accessLogTracker: NewS3AccessLogTracker(db),
	}

	return &loggingFixture{
		server:    srv,
		db:        db,
		eng:       eng,
		tenantID:  tenantID,
		tenant:    tn,
		tempDir:   tempDir,
		bucket:    bucket,
		logBucket: logBucket,
	}
}

func TestGetBucketLogging_Disabled(t *testing.T) {
	f := setupLoggingFixture(t)

	s3Req := &S3Request{Bucket: f.bucket, TenantID: f.tenantID}
	ctx := tenant.WithTenant(context.Background(), f.tenant)
	r := httptest.NewRequest("GET", "/"+f.bucket+"?logging", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	f.server.handleGetBucketLogging(w, r, s3Req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/xml")

	var resp BucketLoggingStatus
	err := xml.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Nil(t, resp.LoggingEnabled)
}

func TestPutBucketLogging_Enable(t *testing.T) {
	f := setupLoggingFixture(t)

	// PUT logging config
	configXML := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<BucketLoggingStatus xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <LoggingEnabled>
    <TargetBucket>%s</TargetBucket>
    <TargetPrefix>logs/</TargetPrefix>
  </LoggingEnabled>
</BucketLoggingStatus>`, f.logBucket)

	s3Req := &S3Request{Bucket: f.bucket, TenantID: f.tenantID}
	ctx := tenant.WithTenant(context.Background(), f.tenant)
	r := httptest.NewRequest("PUT", "/"+f.bucket+"?logging", bytes.NewReader([]byte(configXML))).WithContext(ctx)
	w := httptest.NewRecorder()

	f.server.handlePutBucketLogging(w, r, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	// GET should now return the config
	r2 := httptest.NewRequest("GET", "/"+f.bucket+"?logging", nil).WithContext(ctx)
	w2 := httptest.NewRecorder()
	f.server.handleGetBucketLogging(w2, r2, s3Req)

	assert.Equal(t, http.StatusOK, w2.Code)
	var resp BucketLoggingStatus
	err := xml.Unmarshal(w2.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.NotNil(t, resp.LoggingEnabled)
	assert.Equal(t, f.logBucket, resp.LoggingEnabled.TargetBucket)
	assert.Equal(t, "logs/", resp.LoggingEnabled.TargetPrefix)
}

func TestPutBucketLogging_SameBucket(t *testing.T) {
	f := setupLoggingFixture(t)

	configXML := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<BucketLoggingStatus xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <LoggingEnabled>
    <TargetBucket>%s</TargetBucket>
  </LoggingEnabled>
</BucketLoggingStatus>`, f.bucket) // self-referential

	s3Req := &S3Request{Bucket: f.bucket, TenantID: f.tenantID}
	ctx := tenant.WithTenant(context.Background(), f.tenant)
	r := httptest.NewRequest("PUT", "/"+f.bucket+"?logging", bytes.NewReader([]byte(configXML))).WithContext(ctx)
	w := httptest.NewRecorder()

	f.server.handlePutBucketLogging(w, r, s3Req)

	// Should reject self-referential logging
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
