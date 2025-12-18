package security

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewScanner(t *testing.T) {
	scanner := NewScanner("http://localhost:8080/")

	if scanner.BaseURL != "http://localhost:8080" {
		t.Errorf("expected trailing slash removed, got %s", scanner.BaseURL)
	}
	if scanner.HTTPClient == nil {
		t.Error("expected HTTP client")
	}
}

func TestScanner_SetHeader(t *testing.T) {
	scanner := NewScanner("http://test")
	scanner.SetHeader("Authorization", "Bearer token")

	if scanner.Headers["Authorization"] != "Bearer token" {
		t.Error("header not set")
	}
}

func TestScanner_CheckSecurityHeaders(t *testing.T) {
	// Server without security headers
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "Apache/2.4.1")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	scanner := NewScanner(server.URL)
	ctx := context.Background()

	findings := scanner.CheckSecurityHeaders(ctx, "/")

	// Should find missing security headers
	if len(findings) == 0 {
		t.Error("expected findings for missing security headers")
	}

	// Check for specific headers
	foundCSP := false
	foundHSTS := false
	foundServer := false
	for _, f := range findings {
		if f.Title == "Content-Security-Policy header missing" {
			foundCSP = true
		}
		if f.Title == "HSTS header missing" {
			foundHSTS = true
		}
		if f.Title == "Information disclosure: Server header present" {
			foundServer = true
		}
	}

	if !foundCSP {
		t.Error("expected CSP missing finding")
	}
	if !foundHSTS {
		t.Error("expected HSTS missing finding")
	}
	if !foundServer {
		t.Error("expected Server header finding")
	}

	t.Logf("Found %d security header issues", len(findings))
}

func TestScanner_CheckSecurityHeaders_AllPresent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	scanner := NewScanner(server.URL)
	ctx := context.Background()

	findings := scanner.CheckSecurityHeaders(ctx, "/")

	if len(findings) != 0 {
		t.Errorf("expected no findings when all headers present, got %d", len(findings))
	}
}

func TestScanner_CheckAuthBypass(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin" {
			w.WriteHeader(http.StatusOK) // Should be 401
		} else {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	scanner := NewScanner(server.URL)
	ctx := context.Background()

	findings := scanner.CheckAuthBypass(ctx, []string{"/admin", "/api/users"})

	if len(findings) != 1 {
		t.Errorf("expected 1 finding, got %d", len(findings))
	}

	if len(findings) > 0 && findings[0].Endpoint != "/admin" {
		t.Errorf("expected /admin endpoint, got %s", findings[0].Endpoint)
	}
}

func TestScanner_CheckRateLimiting(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount > 5 {
			w.WriteHeader(http.StatusTooManyRequests)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	scanner := NewScanner(server.URL)
	ctx := context.Background()

	// Should detect rate limiting
	findings := scanner.CheckRateLimiting(ctx, "/", 10)
	if len(findings) != 0 {
		t.Errorf("expected no findings when rate limiting active, got %d", len(findings))
	}
}

func TestScanner_CheckRateLimiting_NotImplemented(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	scanner := NewScanner(server.URL)
	ctx := context.Background()

	findings := scanner.CheckRateLimiting(ctx, "/", 10)
	if len(findings) != 1 {
		t.Errorf("expected 1 finding for missing rate limiting, got %d", len(findings))
	}
}

func TestScanResult_HasCritical(t *testing.T) {
	result := &ScanResult{
		Findings: []Finding{
			{Severity: SeverityMedium},
			{Severity: SeverityLow},
		},
	}

	if result.HasCritical() {
		t.Error("expected no critical")
	}

	result.Findings = append(result.Findings, Finding{Severity: SeverityCritical})
	if !result.HasCritical() {
		t.Error("expected critical")
	}
}

func TestScanResult_HasHighOrAbove(t *testing.T) {
	result := &ScanResult{
		Findings: []Finding{
			{Severity: SeverityMedium},
		},
	}

	if result.HasHighOrAbove() {
		t.Error("expected no high or above")
	}

	result.Findings = append(result.Findings, Finding{Severity: SeverityHigh})
	if !result.HasHighOrAbove() {
		t.Error("expected high or above")
	}
}

func TestScanResult_CountBySeverity(t *testing.T) {
	result := &ScanResult{
		Findings: []Finding{
			{Severity: SeverityCritical},
			{Severity: SeverityHigh},
			{Severity: SeverityHigh},
			{Severity: SeverityMedium},
			{Severity: SeverityLow},
			{Severity: SeverityLow},
			{Severity: SeverityLow},
		},
	}

	counts := result.CountBySeverity()

	if counts[SeverityCritical] != 1 {
		t.Errorf("expected 1 critical, got %d", counts[SeverityCritical])
	}
	if counts[SeverityHigh] != 2 {
		t.Errorf("expected 2 high, got %d", counts[SeverityHigh])
	}
	if counts[SeverityLow] != 3 {
		t.Errorf("expected 3 low, got %d", counts[SeverityLow])
	}
}

func TestScanResult_GenerateReport(t *testing.T) {
	result := &ScanResult{
		Target:    "http://example.com",
		StartTime: time.Now().Add(-time.Minute),
		EndTime:   time.Now(),
		Findings: []Finding{
			{
				Severity:    SeverityHigh,
				Category:    "headers",
				Title:       "Missing CSP",
				Description: "Content Security Policy not set",
				Endpoint:    "/",
				Remediation: "Add CSP header",
			},
		},
	}

	report := result.GenerateReport()

	if report == "" {
		t.Error("expected non-empty report")
	}
	if len(report) < 100 {
		t.Error("report seems too short")
	}

	t.Logf("Report:\n%s", report)
}

func TestInputValidator(t *testing.T) {
	validator := NewInputValidator()

	tests := []struct {
		input    string
		expected []string
	}{
		{"normal input", []string{}},
		{"SELECT * FROM users", []string{"sql_injection"}},
		{"<script>alert('xss')</script>", []string{"xss"}},
		{"../../../etc/passwd", []string{"path_traversal"}},
		{"hello; rm -rf /", []string{"command_injection"}},
	}

	for _, tt := range tests {
		issues := validator.Validate(tt.input)

		if len(tt.expected) == 0 {
			if len(issues) != 0 {
				t.Errorf("input %q: expected no issues, got %v", tt.input, issues)
			}
		} else {
			found := false
			for _, expected := range tt.expected {
				for _, issue := range issues {
					if issue == expected {
						found = true
						break
					}
				}
			}
			if !found {
				t.Errorf("input %q: expected %v, got %v", tt.input, tt.expected, issues)
			}
		}
	}
}

func TestInputValidator_IsSafe(t *testing.T) {
	validator := NewInputValidator()

	if !validator.IsSafe("normal text") {
		t.Error("expected safe input")
	}
	if validator.IsSafe("' OR 1=1--") {
		t.Error("expected unsafe input")
	}
}

func TestFinding(t *testing.T) {
	finding := Finding{
		Severity:    SeverityHigh,
		Category:    "xss",
		Title:       "XSS Vulnerability",
		Description: "Cross-site scripting detected",
		Endpoint:    "/search",
		Evidence:    "Payload reflected",
		Remediation: "Encode output",
	}

	if finding.Severity != SeverityHigh {
		t.Error("Severity not set")
	}
	if finding.Category != "xss" {
		t.Error("Category not set")
	}
}

func TestSeverity(t *testing.T) {
	severities := []Severity{
		SeverityCritical,
		SeverityHigh,
		SeverityMedium,
		SeverityLow,
		SeverityInfo,
	}

	for _, s := range severities {
		if s == "" {
			t.Error("severity should not be empty")
		}
	}
}

func TestAssertNoFindings(t *testing.T) {
	mockT := &testing.T{}

	result := &ScanResult{Findings: []Finding{}}
	AssertNoFindings(mockT, result)

	// With findings would fail
}

func TestAssertNoHighSeverity(t *testing.T) {
	mockT := &testing.T{}

	result := &ScanResult{
		Findings: []Finding{
			{Severity: SeverityLow},
			{Severity: SeverityMedium},
		},
	}
	AssertNoHighSeverity(mockT, result)

	// With high severity would fail
}
