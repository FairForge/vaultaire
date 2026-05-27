package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

type inventoryFixture struct {
	server    *Server
	db        *sql.DB
	eng       *engine.CoreEngine
	tenantID  string
	tenant    *tenant.Tenant
	tempDir   string
	bucket    string
	invBucket string
}

func setupInventoryFixture(t *testing.T) *inventoryFixture {
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

	tempDir, err := os.MkdirTemp("", "vaultaire-inv-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	eng := engine.NewEngine(nil, logger, nil)
	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	tenantID := fmt.Sprintf("inv-%d", os.Getpid())
	bucket := "data-bucket"
	invBucket := "inventory-bucket"
	email := fmt.Sprintf("inv-%d@test.local", os.Getpid())

	_, err = db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, tenantID, "Inventory Test", email, "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)

	for _, b := range []string{bucket, invBucket} {
		_, err = db.Exec(`
			INSERT INTO buckets (tenant_id, name, visibility)
			VALUES ($1, $2, 'private')
			ON CONFLICT (tenant_id, name) DO NOTHING
		`, tenantID, b)
		require.NoError(t, err)
	}

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM buckets WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})

	tn := &tenant.Tenant{
		ID:        tenantID,
		Namespace: "tenant/" + tenantID + "/",
	}

	for _, b := range []string{bucket, invBucket} {
		container := tn.NamespaceContainer(b)
		require.NoError(t, os.MkdirAll(filepath.Join(tempDir, container), 0755))
	}

	srv := &Server{
		logger:   logger,
		router:   chi.NewRouter(),
		engine:   eng,
		db:       db,
		testMode: true,
	}

	return &inventoryFixture{
		server:    srv,
		db:        db,
		eng:       eng,
		tenantID:  tenantID,
		tenant:    tn,
		tempDir:   tempDir,
		bucket:    bucket,
		invBucket: invBucket,
	}
}

func TestPutBucketInventory_Enable(t *testing.T) {
	f := setupInventoryFixture(t)

	configXML := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<InventoryConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <IsEnabled>true</IsEnabled>
  <Schedule><Frequency>Daily</Frequency></Schedule>
  <Destination>
    <S3BucketDestination>
      <Bucket>%s</Bucket>
      <Prefix>reports/</Prefix>
      <Format>CSV</Format>
    </S3BucketDestination>
  </Destination>
</InventoryConfiguration>`, f.invBucket)

	s3Req := &S3Request{Bucket: f.bucket, TenantID: f.tenantID}
	ctx := tenant.WithTenant(context.Background(), f.tenant)
	r := httptest.NewRequest("PUT", "/"+f.bucket+"?inventory", bytes.NewReader([]byte(configXML))).WithContext(ctx)
	w := httptest.NewRecorder()

	f.server.handlePutBucketInventory(w, r, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	// GET should return the config
	r2 := httptest.NewRequest("GET", "/"+f.bucket+"?inventory", nil).WithContext(ctx)
	w2 := httptest.NewRecorder()
	f.server.handleGetBucketInventory(w2, r2, s3Req)

	assert.Equal(t, http.StatusOK, w2.Code)
	var resp InventoryConfiguration
	err := xml.Unmarshal(w2.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.True(t, resp.IsEnabled)
	require.NotNil(t, resp.Destination)
	require.NotNil(t, resp.Destination.S3BucketDestination)
	assert.Equal(t, f.invBucket, resp.Destination.S3BucketDestination.Bucket)
	assert.Equal(t, "reports/", resp.Destination.S3BucketDestination.Prefix)
}

func TestDeleteBucketInventory(t *testing.T) {
	f := setupInventoryFixture(t)

	// First enable inventory
	configXML := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<InventoryConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <IsEnabled>true</IsEnabled>
  <Schedule><Frequency>Daily</Frequency></Schedule>
  <Destination>
    <S3BucketDestination>
      <Bucket>%s</Bucket>
      <Format>CSV</Format>
    </S3BucketDestination>
  </Destination>
</InventoryConfiguration>`, f.invBucket)

	s3Req := &S3Request{Bucket: f.bucket, TenantID: f.tenantID}
	ctx := tenant.WithTenant(context.Background(), f.tenant)
	r := httptest.NewRequest("PUT", "/"+f.bucket+"?inventory", bytes.NewReader([]byte(configXML))).WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handlePutBucketInventory(w, r, s3Req)
	require.Equal(t, http.StatusOK, w.Code)

	// DELETE inventory
	r2 := httptest.NewRequest("DELETE", "/"+f.bucket+"?inventory", nil).WithContext(ctx)
	w2 := httptest.NewRecorder()
	f.server.handleDeleteBucketInventory(w2, r2, s3Req)
	assert.Equal(t, http.StatusNoContent, w2.Code)

	// GET should return disabled
	r3 := httptest.NewRequest("GET", "/"+f.bucket+"?inventory", nil).WithContext(ctx)
	w3 := httptest.NewRecorder()
	f.server.handleGetBucketInventory(w3, r3, s3Req)

	var resp InventoryConfiguration
	err := xml.Unmarshal(w3.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.False(t, resp.IsEnabled)
}

func TestInventoryCSV_Format(t *testing.T) {
	f := setupInventoryFixture(t)

	// Insert some objects into object_head_cache
	for i, key := range []string{"file-a.txt", "file-b.jpg", "folder/file-c.pdf"} {
		_, err := f.db.Exec(`
			INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (tenant_id, bucket, object_key) DO NOTHING
		`, f.tenantID, f.bucket, key, (i+1)*1024, fmt.Sprintf("etag-%d", i), "application/octet-stream", "local")
		require.NoError(t, err)
	}

	// Run inventory report
	logger := zap.NewNop()
	runner := NewInventoryRunner(f.db, f.eng, logger)
	require.NotNil(t, runner)

	runner.GenerateReportNow(context.Background(), f.tenantID, f.bucket, f.invBucket, "inv/", "csv")

	// Read the generated inventory object from the local filesystem
	container := fmt.Sprintf("tenant/%s/%s", f.tenantID, f.invBucket)
	containerPath := filepath.Join(f.tempDir, container)

	// Find the CSV file under inv/
	var csvPath string
	err := filepath.Walk(containerPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.Contains(path, "manifest.csv") {
			csvPath = path
		}
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, csvPath, "inventory CSV file not found")

	data, err := os.ReadFile(csvPath)
	require.NoError(t, err)

	reader := csv.NewReader(bytes.NewReader(data))
	records, err := reader.ReadAll()
	require.NoError(t, err)

	// Header + 3 data rows
	require.Len(t, records, 4)

	// Verify header
	assert.Equal(t, []string{"Key", "SizeBytes", "ETag", "ContentType", "LastModified", "EncryptionAlgorithm", "BackendName"}, records[0])

	// Verify data rows are sorted by key
	assert.Equal(t, "file-a.txt", records[1][0])
	assert.Equal(t, "1024", records[1][1])
	assert.Equal(t, "file-b.jpg", records[2][0])
	assert.Equal(t, "2048", records[2][1])
	assert.Equal(t, "folder/file-c.pdf", records[3][0])
	assert.Equal(t, "3072", records[3][1])
}

func TestCountingResponseWriter_CapturesStatusCode(t *testing.T) {
	w := httptest.NewRecorder()
	cw := &countingResponseWriter{ResponseWriter: w}

	// Act — write header
	cw.WriteHeader(http.StatusNotFound)

	// Assert
	assert.Equal(t, http.StatusNotFound, cw.statusCode)
	assert.True(t, cw.wroteHeader)

	// Second WriteHeader should not change status
	cw.WriteHeader(http.StatusOK)
	assert.Equal(t, http.StatusNotFound, cw.statusCode)
}

func TestCountingResponseWriter_ImplicitOK(t *testing.T) {
	w := httptest.NewRecorder()
	cw := &countingResponseWriter{ResponseWriter: w}

	// Act — Write without explicit WriteHeader
	_, err := cw.Write([]byte("hello"))
	require.NoError(t, err)

	// Assert — implicit 200
	assert.Equal(t, http.StatusOK, cw.statusCode)
	assert.Equal(t, int64(5), cw.bytesWritten)
}

func TestDetermineOperation_LoggingAndInventory(t *testing.T) {
	logger := zap.NewNop()
	parser := NewS3Parser(logger)

	tests := []struct {
		method string
		query  map[string]string
		wantOp string
	}{
		{"GET", map[string]string{"logging": ""}, "GetBucketLogging"},
		{"PUT", map[string]string{"logging": ""}, "PutBucketLogging"},
		{"GET", map[string]string{"inventory": ""}, "GetBucketInventory"},
		{"PUT", map[string]string{"inventory": ""}, "PutBucketInventory"},
		{"DELETE", map[string]string{"inventory": ""}, "DeleteBucketInventory"},
	}

	for _, tt := range tests {
		t.Run(tt.wantOp, func(t *testing.T) {
			req := &S3Request{
				Bucket: "test-bucket",
				Query:  tt.query,
			}
			parser.determineOperation(req, tt.method)
			assert.Equal(t, tt.wantOp, req.Operation)
		})
	}
}

func TestGetBucketInventory_Disabled(t *testing.T) {
	f := setupInventoryFixture(t)

	s3Req := &S3Request{Bucket: f.bucket, TenantID: f.tenantID}
	ctx := tenant.WithTenant(context.Background(), f.tenant)
	r := httptest.NewRequest("GET", "/"+f.bucket+"?inventory", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	f.server.handleGetBucketInventory(w, r, s3Req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp InventoryConfiguration
	err := xml.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.False(t, resp.IsEnabled)
}
