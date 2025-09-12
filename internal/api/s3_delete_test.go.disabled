package api

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestS3_DeleteObject(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	eng := engine.NewEngine(logger)

	// Setup storage
	tempDir, err := os.MkdirTemp("", "vaultaire-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	// Create the namespaced bucket directory that S3ToEngine expects
	namespacedBucket := "test-tenant_test-bucket"
	bucketPath := filepath.Join(tempDir, namespacedBucket)
	err = os.MkdirAll(bucketPath, 0755)
	require.NoError(t, err)
	server := &Server{
		logger:   logger,
		router:   mux.NewRouter(),
		engine:   eng,
		testMode: true,
	}
	server.router.PathPrefix("/").HandlerFunc(server.handleS3Request)

	testTenant := &tenant.Tenant{
		ID:        "test-tenant",
		Namespace: "tenant/test-tenant/",
	}

	// First PUT an object
	putReq := httptest.NewRequest("PUT", "/test-bucket/test-key.txt",
		bytes.NewReader([]byte("test content")))
	ctx := tenant.WithTenant(putReq.Context(), testTenant)
	putReq = putReq.WithContext(ctx)
	putW := httptest.NewRecorder()
	server.handleS3Request(putW, putReq)
	assert.Equal(t, 200, putW.Code, "PUT should succeed")

	// Then DELETE it
	deleteReq := httptest.NewRequest("DELETE", "/test-bucket/test-key.txt", nil)
	ctx = tenant.WithTenant(deleteReq.Context(), testTenant)
	deleteReq = deleteReq.WithContext(ctx)
	deleteW := httptest.NewRecorder()

	server.handleS3Request(deleteW, deleteReq)
	assert.Equal(t, 204, deleteW.Code, "DELETE should return 204 No Content")

	// Verify it's gone with GET
	getReq := httptest.NewRequest("GET", "/test-bucket/test-key.txt", nil)
	ctx = tenant.WithTenant(getReq.Context(), testTenant)
	getReq = getReq.WithContext(ctx)
	getW := httptest.NewRecorder()

	server.handleS3Request(getW, getReq)
	assert.Equal(t, 404, getW.Code, "GET after DELETE should return 404")
}
