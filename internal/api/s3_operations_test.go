package api

import (
	"bytes"
	"io"
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

func TestS3_PutAndGet_WithTenant(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	eng := engine.NewEngine(logger)

	// Create temp dir for storage
	tempDir, err := os.MkdirTemp("", "vaultaire-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Create and register a local driver
	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local") // Assuming this method exists, or check the engine API

	// Create the namespaced bucket directory
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
		APIKey:    "test-key",
	}

	// Test PUT
	testContent := "test content for step 56"
	putReq := httptest.NewRequest("PUT", "/test-bucket/test-key.txt",
		bytes.NewReader([]byte(testContent)))
	ctx := tenant.WithTenant(putReq.Context(), testTenant)
	putReq = putReq.WithContext(ctx)

	putW := httptest.NewRecorder()
	server.handleS3Request(putW, putReq)
	assert.Equal(t, 200, putW.Code, "PUT should succeed")

	// Test GET
	getReq := httptest.NewRequest("GET", "/test-bucket/test-key.txt", nil)
	ctx = tenant.WithTenant(getReq.Context(), testTenant)
	getReq = getReq.WithContext(ctx)

	getW := httptest.NewRecorder()
	server.handleS3Request(getW, getReq)
	assert.Equal(t, 200, getW.Code, "GET should succeed")

	// Verify content
	body, err := io.ReadAll(getW.Body)
	require.NoError(t, err)
	assert.Equal(t, testContent, string(body))
}

func TestS3_RequiresTenant(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	eng := engine.NewEngine(logger)

	server := &Server{
		logger:   logger,
		router:   mux.NewRouter(),
		engine:   eng,
		testMode: true,
	}

	req := httptest.NewRequest("PUT", "/test-bucket/test.txt", nil)
	w := httptest.NewRecorder()

	server.handleS3Request(w, req)
	assert.NotEqual(t, 403, w.Code, "Should not fail with tenant error")
}
