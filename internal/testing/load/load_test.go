package load

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewRunner(t *testing.T) {
	config := Config{
		URL:         "http://localhost:8080",
		Concurrency: 5,
	}

	runner := NewRunner(config)

	if runner.client == nil {
		t.Error("expected HTTP client")
	}
	if runner.config.Method != http.MethodGet {
		t.Error("expected default method GET")
	}
}

func TestNewRunner_Defaults(t *testing.T) {
	config := Config{URL: "http://test"}
	runner := NewRunner(config)

	if runner.config.Concurrency != 10 {
		t.Errorf("expected default concurrency 10, got %d", runner.config.Concurrency)
	}
	if runner.config.Timeout != 30*time.Second {
		t.Errorf("expected default timeout 30s, got %v", runner.config.Timeout)
	}
}

func TestRunner_Run_RequestCount(t *testing.T) {
	var requestCount int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	config := Config{
		URL:         server.URL,
		Requests:    100,
		Concurrency: 10,
	}

	runner := NewRunner(config)
	ctx := context.Background()

	result, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if result.TotalRequests != 100 {
		t.Errorf("expected 100 requests, got %d", result.TotalRequests)
	}
	if result.SuccessCount != 100 {
		t.Errorf("expected 100 successes, got %d", result.SuccessCount)
	}
	if result.FailureCount != 0 {
		t.Errorf("expected 0 failures, got %d", result.FailureCount)
	}

	t.Logf("Result: %d requests, %.2f rps, avg latency %v",
		result.TotalRequests, result.RequestsPerSec, result.AvgLatency)
}

func TestRunner_Run_Duration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	config := Config{
		URL:         server.URL,
		Duration:    500 * time.Millisecond,
		Concurrency: 5,
	}

	runner := NewRunner(config)
	ctx := context.Background()

	result, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if result.TotalRequests == 0 {
		t.Error("expected some requests")
	}
	if result.TotalDuration < 400*time.Millisecond {
		t.Errorf("expected duration >= 400ms, got %v", result.TotalDuration)
	}

	t.Logf("Duration test: %d requests in %v", result.TotalRequests, result.TotalDuration)
}

func TestRunner_Run_WithRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	config := Config{
		URL:         server.URL,
		Duration:    time.Second,
		RateLimit:   50, // 50 requests per second
		Concurrency: 5,
	}

	runner := NewRunner(config)
	ctx := context.Background()

	result, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Should be around 50 requests (±20% tolerance)
	if result.TotalRequests < 40 || result.TotalRequests > 60 {
		t.Errorf("expected ~50 requests with rate limit, got %d", result.TotalRequests)
	}

	t.Logf("Rate limited: %d requests (target: 50)", result.TotalRequests)
}

func TestRunner_Run_StatusCodes(t *testing.T) {
	var count int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt64(&count, 1)
		if c%3 == 0 {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	config := Config{
		URL:         server.URL,
		Requests:    30,
		Concurrency: 1, // Sequential for predictable results
	}

	runner := NewRunner(config)
	ctx := context.Background()

	result, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if result.StatusCodes[200] != 20 {
		t.Errorf("expected 20 status 200, got %d", result.StatusCodes[200])
	}
	if result.StatusCodes[500] != 10 {
		t.Errorf("expected 10 status 500, got %d", result.StatusCodes[500])
	}
}

func TestRunner_Run_ConnectionErrors(t *testing.T) {
	config := Config{
		URL:         "http://localhost:1", // Invalid port
		Requests:    5,
		Concurrency: 1,
		Timeout:     100 * time.Millisecond,
	}

	runner := NewRunner(config)
	ctx := context.Background()

	result, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if result.FailureCount == 0 {
		t.Error("expected failures for invalid URL")
	}
	if len(result.Errors) == 0 {
		t.Error("expected error counts")
	}

	t.Logf("Errors: %v", result.Errors)
}

func TestResult_GenerateReport(t *testing.T) {
	result := &Result{
		TotalRequests:  1000,
		SuccessCount:   980,
		FailureCount:   20,
		TotalDuration:  10 * time.Second,
		RequestsPerSec: 100,
		AvgLatency:     50 * time.Millisecond,
		MinLatency:     10 * time.Millisecond,
		MaxLatency:     200 * time.Millisecond,
		P50Latency:     45 * time.Millisecond,
		P90Latency:     80 * time.Millisecond,
		P95Latency:     100 * time.Millisecond,
		P99Latency:     150 * time.Millisecond,
		BytesSent:      50000,
		BytesReceived:  500000,
		StatusCodes:    map[int]int64{200: 980, 500: 20},
		Errors:         map[string]int64{"timeout": 5},
	}

	report := result.GenerateReport()

	if report == "" {
		t.Error("expected non-empty report")
	}
	if len(report) < 200 {
		t.Error("report seems too short")
	}

	t.Logf("Report:\n%s", report)
}

func TestPercentile(t *testing.T) {
	durations := make([]time.Duration, 100)
	for i := 0; i < 100; i++ {
		durations[i] = time.Duration(i+1) * time.Millisecond
	}

	// Index calculation: (n * len) / 100
	// p50: (50 * 100) / 100 = 50 -> durations[50] = 51ms
	p50 := percentile(durations, 50)
	if p50 != 51*time.Millisecond {
		t.Errorf("expected p50=51ms, got %v", p50)
	}

	// p99: (99 * 100) / 100 = 99 -> durations[99] = 100ms
	p99 := percentile(durations, 99)
	if p99 != 100*time.Millisecond {
		t.Errorf("expected p99=100ms, got %v", p99)
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		bytes    int64
		expected string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}

	for _, tt := range tests {
		result := formatBytes(tt.bytes)
		if result != tt.expected {
			t.Errorf("formatBytes(%d) = %s, expected %s", tt.bytes, result, tt.expected)
		}
	}
}

func TestScenarioRunner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sr := NewScenarioRunner()

	sr.AddScenario(Scenario{
		Name: "light-load",
		Config: Config{
			URL:         server.URL,
			Requests:    50,
			Concurrency: 5,
		},
	})

	sr.AddScenario(Scenario{
		Name: "medium-load",
		Config: Config{
			URL:         server.URL,
			Requests:    100,
			Concurrency: 10,
		},
	})

	ctx := context.Background()
	err := sr.Run(ctx)
	if err != nil {
		t.Fatalf("ScenarioRunner failed: %v", err)
	}

	results := sr.Results()
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}

	if results["light-load"].TotalRequests != 50 {
		t.Errorf("expected 50 requests for light-load")
	}
}

func TestRunStressTest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	st := StressTest{
		BaseConfig: Config{
			URL: server.URL,
		},
		StartConcurrency: 5,
		MaxConcurrency:   15,
		StepSize:         5,
		StepDuration:     200 * time.Millisecond,
		FailureThreshold: 50,
	}

	ctx := context.Background()
	results, err := RunStressTest(ctx, st)
	if err != nil {
		t.Fatalf("StressTest failed: %v", err)
	}

	if len(results) != 3 {
		t.Errorf("expected 3 stress results, got %d", len(results))
	}

	// Check concurrency levels
	expectedConcurrency := []int{5, 10, 15}
	for i, sr := range results {
		if sr.Concurrency != expectedConcurrency[i] {
			t.Errorf("step %d: expected concurrency %d, got %d",
				i, expectedConcurrency[i], sr.Concurrency)
		}
	}
}

func TestAssertPerformance(t *testing.T) {
	result := &Result{
		TotalRequests:  1000,
		SuccessCount:   990,
		FailureCount:   10,
		P99Latency:     50 * time.Millisecond,
		RequestsPerSec: 500,
	}

	// Should pass
	err := AssertPerformance(result, 100*time.Millisecond, 100, 5)
	if err != nil {
		t.Errorf("expected pass, got: %v", err)
	}

	// Should fail on P99
	err = AssertPerformance(result, 10*time.Millisecond, 100, 5)
	if err == nil {
		t.Error("expected P99 failure")
	}

	// Should fail on RPS
	err = AssertPerformance(result, 100*time.Millisecond, 1000, 5)
	if err == nil {
		t.Error("expected RPS failure")
	}

	// Should fail on failure rate
	err = AssertPerformance(result, 100*time.Millisecond, 100, 0.5)
	if err == nil {
		t.Error("expected failure rate failure")
	}
}

func TestConfig(t *testing.T) {
	config := Config{
		URL:         "http://test",
		Method:      http.MethodPost,
		Headers:     map[string]string{"X-Test": "value"},
		Body:        []byte("test body"),
		Concurrency: 20,
		Requests:    1000,
		Duration:    time.Minute,
		RateLimit:   100,
		Timeout:     10 * time.Second,
	}

	if config.Concurrency != 20 {
		t.Error("Concurrency not set")
	}
	if config.RateLimit != 100 {
		t.Error("RateLimit not set")
	}
}

func TestScenario(t *testing.T) {
	scenario := Scenario{
		Name:       "test-scenario",
		WarmupTime: 10 * time.Second,
		RampUpTime: 30 * time.Second,
	}

	if scenario.Name != "test-scenario" {
		t.Error("Name not set")
	}
	if scenario.WarmupTime != 10*time.Second {
		t.Error("WarmupTime not set")
	}
}

func TestStressTest(t *testing.T) {
	st := StressTest{
		StartConcurrency: 10,
		MaxConcurrency:   100,
		StepSize:         10,
		StepDuration:     time.Minute,
		FailureThreshold: 5.0,
	}

	if st.MaxConcurrency != 100 {
		t.Error("MaxConcurrency not set")
	}
	if st.FailureThreshold != 5.0 {
		t.Error("FailureThreshold not set")
	}
}

func TestStressResult(t *testing.T) {
	sr := StressResult{
		Concurrency:   50,
		BreakingPoint: true,
	}

	if sr.Concurrency != 50 {
		t.Error("Concurrency not set")
	}
	if !sr.BreakingPoint {
		t.Error("BreakingPoint not set")
	}
}

func TestSimplifyError(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"context deadline exceeded", "timeout"},
		{"dial tcp: connection refused", "connection refused"},
		{"no such host", "dns error"},
		{"short error", "short error"},
	}

	for _, tt := range tests {
		result := simplifyError(testError{tt.input})
		if result != tt.expected {
			t.Errorf("simplifyError(%q) = %q, expected %q", tt.input, result, tt.expected)
		}
	}
}

type testError struct {
	msg string
}

func (e testError) Error() string {
	return e.msg
}
