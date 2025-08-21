package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetrics_Initialization(t *testing.T) {
	// Reset for testing
	ResetMetricsForTesting()
	
	// Test that metrics are properly initialized
	metrics := NewMetrics()
	
	if metrics == nil {
		t.Fatal("Metrics should not be nil")
	}
	
	// Should have request counter
	if metrics.RequestCounter == nil {
		t.Error("RequestCounter should be initialized")
	}
	
	// Should have latency histogram
	if metrics.LatencyHistogram == nil {
		t.Error("LatencyHistogram should be initialized")
	}
	
	// Should have rate limit counter
	if metrics.RateLimitHits == nil {
		t.Error("RateLimitHits should be initialized")
	}
}

func TestMetrics_IncrementRequestCounter(t *testing.T) {
	ResetMetricsForTesting()
	metrics := NewMetrics()
	
	// Increment for tenant1
	metrics.IncrementRequest("tenant1", "GET", "/test", 200)
	
	// Verify it was incremented (this will need Prometheus testutil)
	// For now, just ensure no panic
}

func TestMetrics_RecordLatency(t *testing.T) {
	ResetMetricsForTesting()
	metrics := NewMetrics()
	
	// Record latency for a request
	metrics.RecordLatency("tenant1", "GET", "/test", 0.123)
	
	// Verify it was recorded (this will need Prometheus testutil)
	// For now, just ensure no panic
}

func TestMetrics_IncrementRateLimitHits(t *testing.T) {
	ResetMetricsForTesting()
	metrics := NewMetrics()
	
	// Increment rate limit hits
	metrics.IncrementRateLimitHit("tenant1")
	
	// Verify it was incremented
	// For now, just ensure no panic
}

func TestMetrics_Handler(t *testing.T) {
	ResetMetricsForTesting()
	metrics := NewMetrics()
	
	// Increment some metrics first so they appear in output
	metrics.IncrementRequest("test-tenant", "GET", "/test", 200)
	metrics.RecordLatency("test-tenant", "GET", "/test", 0.5)
	
	handler := metrics.Handler()
	
	// Make request to metrics endpoint
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	
	handler.ServeHTTP(rec, req)
	
	// Should return 200
	if rec.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rec.Code)
	}
	
	// Should contain our custom metrics
	body := rec.Body.String()
	if !strings.Contains(body, "vaultaire_requests_total") {
		t.Error("Response should contain vaultaire_requests_total metric")
	}
	if !strings.Contains(body, "vaultaire_request_duration_seconds") {
		t.Error("Response should contain vaultaire_request_duration_seconds metric")
	}
}

func TestMetrics_Singleton(t *testing.T) {
	ResetMetricsForTesting()
	
	// Create two instances
	m1 := NewMetrics()
	m2 := NewMetrics()
	
	// Should be the same instance
	if m1 != m2 {
		t.Error("NewMetrics should return the same instance (singleton)")
	}
}
