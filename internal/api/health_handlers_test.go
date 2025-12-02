// internal/api/health_handlers_test.go
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthHandler_AllHealthy(t *testing.T) {
	s := newTestServerWithHealthChecker(t)
	s.healthChecker.RegisterBackend("lyve")
	s.healthChecker.RegisterBackend("quotaless")
	s.healthChecker.UpdateHealth("lyve", true, 10*time.Millisecond, nil)
	s.healthChecker.UpdateHealth("quotaless", true, 15*time.Millisecond, nil)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	s.handleHealthEnhanced(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)

	assert.Equal(t, "healthy", resp["status"])
	assert.NotEmpty(t, resp["version"])
	assert.Greater(t, resp["uptime"].(float64), 0.0)
}

func TestHealthHandler_DegradedBackend(t *testing.T) {
	s := newTestServerWithHealthChecker(t)
	s.healthChecker.RegisterBackend("lyve")
	s.healthChecker.RegisterBackend("quotaless")
	s.healthChecker.UpdateHealth("lyve", true, 10*time.Millisecond, nil)
	s.healthChecker.UpdateHealth("quotaless", false, 0, assert.AnError)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	s.handleHealthEnhanced(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)

	assert.Equal(t, "degraded", resp["status"])
}

func TestHealthHandler_AllUnhealthy(t *testing.T) {
	s := newTestServerWithHealthChecker(t)
	s.healthChecker.RegisterBackend("lyve")
	s.healthChecker.RegisterBackend("quotaless")
	s.healthChecker.UpdateHealth("lyve", false, 0, assert.AnError)
	s.healthChecker.UpdateHealth("quotaless", false, 0, assert.AnError)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	s.handleHealthEnhanced(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)

	assert.Equal(t, "unhealthy", resp["status"])
}

func TestLivenessHandler(t *testing.T) {
	s := newTestServerWithHealthChecker(t)

	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	w := httptest.NewRecorder()

	s.handleLiveness(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)

	assert.Equal(t, "alive", resp["status"])
}

func TestReadinessHandler_Ready(t *testing.T) {
	s := newTestServerWithHealthChecker(t)
	s.healthChecker.RegisterBackend("lyve")
	s.healthChecker.UpdateHealth("lyve", true, 10*time.Millisecond, nil)

	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	w := httptest.NewRecorder()

	s.handleReadiness(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)

	assert.Equal(t, true, resp["ready"])
}

func TestReadinessHandler_NotReady(t *testing.T) {
	s := newTestServerWithHealthChecker(t)
	s.healthChecker.RegisterBackend("lyve")
	s.healthChecker.UpdateHealth("lyve", false, 0, assert.AnError)

	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	w := httptest.NewRecorder()

	s.handleReadiness(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestBackendsHealthHandler(t *testing.T) {
	s := newTestServerWithHealthChecker(t)
	s.healthChecker.RegisterBackend("lyve")
	s.healthChecker.RegisterBackend("quotaless")
	s.healthChecker.RegisterBackend("onedrive")
	s.healthChecker.UpdateHealth("lyve", true, 10*time.Millisecond, nil)
	s.healthChecker.UpdateHealth("quotaless", true, 15*time.Millisecond, nil)
	s.healthChecker.UpdateHealth("onedrive", false, 0, assert.AnError)

	req := httptest.NewRequest(http.MethodGet, "/health/backends", nil)
	w := httptest.NewRecorder()

	s.handleBackendsHealth(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)

	assert.Len(t, resp, 3)
	assert.Equal(t, "healthy", resp["lyve"]["status"])
	assert.Equal(t, "healthy", resp["quotaless"]["status"])
	assert.Equal(t, "unhealthy", resp["onedrive"]["status"])
}

func TestHealthHandler_WithDetails(t *testing.T) {
	s := newTestServerWithHealthChecker(t)
	s.healthChecker.RegisterBackend("lyve")
	s.healthChecker.UpdateHealth("lyve", true, 10*time.Millisecond, nil)

	req := httptest.NewRequest(http.MethodGet, "/health?details=true", nil)
	w := httptest.NewRecorder()

	s.handleHealthEnhanced(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)

	// Should include backends when details=true
	backends, ok := resp["backends"].(map[string]interface{})
	assert.True(t, ok, "backends should be included with details=true")
	assert.Contains(t, backends, "lyve")
}

// Helper to create test server with health checker
func newTestServerWithHealthChecker(t *testing.T) *Server {
	t.Helper()
	return &Server{
		startTime:     time.Now(),
		healthChecker: NewBackendHealthChecker(),
	}
}
