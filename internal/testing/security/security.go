// Package security provides utilities for security testing.
package security

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Scanner performs security scans on HTTP endpoints.
type Scanner struct {
	BaseURL    string
	HTTPClient *http.Client
	Headers    map[string]string
	Timeout    time.Duration
}

// NewScanner creates a security scanner.
func NewScanner(baseURL string) *Scanner {
	return &Scanner{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // Don't follow redirects
			},
		},
		Headers: make(map[string]string),
		Timeout: 30 * time.Second,
	}
}

// SetHeader sets a default header for all requests.
func (s *Scanner) SetHeader(key, value string) {
	s.Headers[key] = value
}

// Finding represents a security issue found during scanning.
type Finding struct {
	Severity    Severity
	Category    string
	Title       string
	Description string
	Endpoint    string
	Evidence    string
	Remediation string
}

// Severity indicates the severity of a finding.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityInfo     Severity = "info"
)

// ScanResult contains all findings from a scan.
type ScanResult struct {
	Target    string
	StartTime time.Time
	EndTime   time.Time
	Findings  []Finding
	Errors    []string
}

// HasCritical returns true if any critical findings exist.
func (r *ScanResult) HasCritical() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityCritical {
			return true
		}
	}
	return false
}

// HasHighOrAbove returns true if any high or critical findings exist.
func (r *ScanResult) HasHighOrAbove() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityCritical || f.Severity == SeverityHigh {
			return true
		}
	}
	return false
}

// CountBySeverity counts findings by severity.
func (r *ScanResult) CountBySeverity() map[Severity]int {
	counts := make(map[Severity]int)
	for _, f := range r.Findings {
		counts[f.Severity]++
	}
	return counts
}

// CheckSecurityHeaders scans for missing security headers.
func (s *Scanner) CheckSecurityHeaders(ctx context.Context, path string) []Finding {
	findings := make([]Finding, 0)

	resp, err := s.get(ctx, path)
	if err != nil {
		return findings
	}
	defer func() { _ = resp.Body.Close() }()

	requiredHeaders := map[string]struct {
		name        string
		description string
		remediation string
	}{
		"X-Content-Type-Options": {
			name:        "X-Content-Type-Options header missing",
			description: "The X-Content-Type-Options header prevents MIME type sniffing",
			remediation: "Add header: X-Content-Type-Options: nosniff",
		},
		"X-Frame-Options": {
			name:        "X-Frame-Options header missing",
			description: "The X-Frame-Options header prevents clickjacking attacks",
			remediation: "Add header: X-Frame-Options: DENY or SAMEORIGIN",
		},
		"X-XSS-Protection": {
			name:        "X-XSS-Protection header missing",
			description: "The X-XSS-Protection header enables browser XSS filtering",
			remediation: "Add header: X-XSS-Protection: 1; mode=block",
		},
		"Strict-Transport-Security": {
			name:        "HSTS header missing",
			description: "HTTP Strict Transport Security forces HTTPS connections",
			remediation: "Add header: Strict-Transport-Security: max-age=31536000; includeSubDomains",
		},
		"Content-Security-Policy": {
			name:        "Content-Security-Policy header missing",
			description: "CSP helps prevent XSS and data injection attacks",
			remediation: "Add appropriate Content-Security-Policy header",
		},
	}

	for header, info := range requiredHeaders {
		if resp.Header.Get(header) == "" {
			findings = append(findings, Finding{
				Severity:    SeverityMedium,
				Category:    "headers",
				Title:       info.name,
				Description: info.description,
				Endpoint:    path,
				Remediation: info.remediation,
			})
		}
	}

	// Check for information disclosure headers
	sensitiveHeaders := []string{"Server", "X-Powered-By", "X-AspNet-Version"}
	for _, header := range sensitiveHeaders {
		if value := resp.Header.Get(header); value != "" {
			findings = append(findings, Finding{
				Severity:    SeverityLow,
				Category:    "headers",
				Title:       fmt.Sprintf("Information disclosure: %s header present", header),
				Description: "Server information headers can help attackers identify vulnerabilities",
				Endpoint:    path,
				Evidence:    fmt.Sprintf("%s: %s", header, value),
				Remediation: fmt.Sprintf("Remove or obfuscate the %s header", header),
			})
		}
	}

	return findings
}

// CheckSQLInjection tests for basic SQL injection vulnerabilities.
func (s *Scanner) CheckSQLInjection(ctx context.Context, path string, params map[string]string) []Finding {
	findings := make([]Finding, 0)

	payloads := []string{
		"' OR '1'='1",
		"'; DROP TABLE users--",
		"1; SELECT * FROM users",
		"' UNION SELECT NULL--",
		"admin'--",
	}

	for param, originalValue := range params {
		for _, payload := range payloads {
			testParams := make(map[string]string)
			for k, v := range params {
				testParams[k] = v
			}
			testParams[param] = originalValue + payload

			resp, err := s.getWithParams(ctx, path, testParams)
			if err != nil {
				continue
			}

			// Check for SQL error messages in response
			body := s.readBody(resp)
			_ = resp.Body.Close()

			sqlErrors := []string{
				"SQL syntax",
				"mysql_fetch",
				"ORA-",
				"PostgreSQL",
				"SQLite",
				"ODBC",
				"syntax error",
			}

			for _, errStr := range sqlErrors {
				if strings.Contains(body, errStr) {
					findings = append(findings, Finding{
						Severity:    SeverityCritical,
						Category:    "injection",
						Title:       "Potential SQL Injection vulnerability",
						Description: "The application may be vulnerable to SQL injection attacks",
						Endpoint:    path,
						Evidence:    fmt.Sprintf("Parameter: %s, Payload: %s, Error: %s", param, payload, errStr),
						Remediation: "Use parameterized queries or prepared statements",
					})
					break
				}
			}
		}
	}

	return findings
}

// CheckXSS tests for basic XSS vulnerabilities.
func (s *Scanner) CheckXSS(ctx context.Context, path string, params map[string]string) []Finding {
	findings := make([]Finding, 0)

	payloads := []string{
		"<script>alert('XSS')</script>",
		"<img src=x onerror=alert('XSS')>",
		"javascript:alert('XSS')",
		"<svg onload=alert('XSS')>",
		"'\"><script>alert('XSS')</script>",
	}

	for param, originalValue := range params {
		for _, payload := range payloads {
			testParams := make(map[string]string)
			for k, v := range params {
				testParams[k] = v
			}
			testParams[param] = originalValue + payload

			resp, err := s.getWithParams(ctx, path, testParams)
			if err != nil {
				continue
			}

			body := s.readBody(resp)
			_ = resp.Body.Close()

			// Check if payload is reflected without encoding
			if strings.Contains(body, payload) {
				findings = append(findings, Finding{
					Severity:    SeverityHigh,
					Category:    "xss",
					Title:       "Potential Reflected XSS vulnerability",
					Description: "User input is reflected in the response without proper encoding",
					Endpoint:    path,
					Evidence:    fmt.Sprintf("Parameter: %s, Payload reflected: %s", param, payload),
					Remediation: "Encode all user input before reflecting in responses",
				})
				break
			}
		}
	}

	return findings
}

// CheckOpenRedirect tests for open redirect vulnerabilities.
func (s *Scanner) CheckOpenRedirect(ctx context.Context, path string, redirectParams []string) []Finding {
	findings := make([]Finding, 0)

	maliciousURLs := []string{
		"https://evil.com",
		"//evil.com",
		"/\\evil.com",
		"https:evil.com",
	}

	for _, param := range redirectParams {
		for _, malURL := range maliciousURLs {
			params := map[string]string{param: malURL}

			resp, err := s.getWithParams(ctx, path, params)
			if err != nil {
				continue
			}

			location := resp.Header.Get("Location")
			_ = resp.Body.Close()

			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				if strings.Contains(location, "evil.com") {
					findings = append(findings, Finding{
						Severity:    SeverityMedium,
						Category:    "redirect",
						Title:       "Open Redirect vulnerability",
						Description: "The application redirects to user-controlled URLs",
						Endpoint:    path,
						Evidence:    fmt.Sprintf("Parameter: %s, Redirects to: %s", param, location),
						Remediation: "Validate redirect URLs against a whitelist",
					})
					break
				}
			}
		}
	}

	return findings
}

// CheckTLSConfig analyzes TLS configuration.
func (s *Scanner) CheckTLSConfig(host string) []Finding {
	findings := make([]Finding, 0)

	// Test for weak TLS versions
	weakVersions := map[uint16]string{
		tls.VersionTLS10: "TLS 1.0",
		tls.VersionTLS11: "TLS 1.1",
	}

	for version, name := range weakVersions {
		conn, err := tls.Dial("tcp", host, &tls.Config{
			MinVersion:         version,
			MaxVersion:         version,
			InsecureSkipVerify: true,
		})
		if err == nil {
			_ = conn.Close()
			findings = append(findings, Finding{
				Severity:    SeverityMedium,
				Category:    "tls",
				Title:       fmt.Sprintf("Weak TLS version supported: %s", name),
				Description: fmt.Sprintf("The server supports %s which has known vulnerabilities", name),
				Endpoint:    host,
				Remediation: "Disable TLS versions below 1.2",
			})
		}
	}

	return findings
}

// CheckAuthBypass tests for authentication bypass.
func (s *Scanner) CheckAuthBypass(ctx context.Context, protectedPaths []string) []Finding {
	findings := make([]Finding, 0)

	for _, path := range protectedPaths {
		// Test without auth
		resp, err := s.get(ctx, path)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			findings = append(findings, Finding{
				Severity:    SeverityHigh,
				Category:    "auth",
				Title:       "Authentication bypass detected",
				Description: "Protected endpoint accessible without authentication",
				Endpoint:    path,
				Evidence:    fmt.Sprintf("Status code: %d", resp.StatusCode),
				Remediation: "Ensure authentication is required for protected endpoints",
			})
		}
	}

	return findings
}

// CheckRateLimiting tests if rate limiting is implemented.
func (s *Scanner) CheckRateLimiting(ctx context.Context, path string, requestCount int) []Finding {
	findings := make([]Finding, 0)
	rateLimited := false

	for i := 0; i < requestCount; i++ {
		resp, err := s.get(ctx, path)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			rateLimited = true
			break
		}
	}

	if !rateLimited {
		findings = append(findings, Finding{
			Severity:    SeverityMedium,
			Category:    "rate-limiting",
			Title:       "Rate limiting not detected",
			Description: fmt.Sprintf("Endpoint accepted %d requests without rate limiting", requestCount),
			Endpoint:    path,
			Remediation: "Implement rate limiting to prevent abuse",
		})
	}

	return findings
}

// get performs a GET request.
func (s *Scanner) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}

	for k, v := range s.Headers {
		req.Header.Set(k, v)
	}

	return s.HTTPClient.Do(req)
}

// getWithParams performs a GET request with query parameters.
func (s *Scanner) getWithParams(ctx context.Context, path string, params map[string]string) (*http.Response, error) {
	u, err := url.Parse(s.BaseURL + path)
	if err != nil {
		return nil, err
	}

	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	for k, v := range s.Headers {
		req.Header.Set(k, v)
	}

	return s.HTTPClient.Do(req)
}

// readBody reads response body as string.
func (s *Scanner) readBody(resp *http.Response) string {
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}

// GenerateReport creates a security scan report.
func (r *ScanResult) GenerateReport() string {
	report := "Security Scan Report\n"
	report += "====================\n\n"
	report += fmt.Sprintf("Target: %s\n", r.Target)
	report += fmt.Sprintf("Scan Time: %s - %s\n", r.StartTime.Format(time.RFC3339), r.EndTime.Format(time.RFC3339))
	report += fmt.Sprintf("Duration: %v\n\n", r.EndTime.Sub(r.StartTime))

	counts := r.CountBySeverity()
	report += "Summary:\n"
	report += fmt.Sprintf("  Critical: %d\n", counts[SeverityCritical])
	report += fmt.Sprintf("  High:     %d\n", counts[SeverityHigh])
	report += fmt.Sprintf("  Medium:   %d\n", counts[SeverityMedium])
	report += fmt.Sprintf("  Low:      %d\n", counts[SeverityLow])
	report += fmt.Sprintf("  Info:     %d\n\n", counts[SeverityInfo])

	if len(r.Findings) > 0 {
		report += "Findings:\n"
		report += "---------\n"
		for i, f := range r.Findings {
			report += fmt.Sprintf("\n%d. [%s] %s\n", i+1, f.Severity, f.Title)
			report += fmt.Sprintf("   Category: %s\n", f.Category)
			report += fmt.Sprintf("   Endpoint: %s\n", f.Endpoint)
			report += fmt.Sprintf("   Description: %s\n", f.Description)
			if f.Evidence != "" {
				report += fmt.Sprintf("   Evidence: %s\n", f.Evidence)
			}
			report += fmt.Sprintf("   Remediation: %s\n", f.Remediation)
		}
	} else {
		report += "No findings detected.\n"
	}

	if len(r.Errors) > 0 {
		report += "\nErrors:\n"
		for _, e := range r.Errors {
			report += fmt.Sprintf("  - %s\n", e)
		}
	}

	return report
}

// InputValidator validates user input for security issues.
type InputValidator struct {
	patterns map[string]*regexp.Regexp
}

// NewInputValidator creates an input validator.
func NewInputValidator() *InputValidator {
	v := &InputValidator{
		patterns: make(map[string]*regexp.Regexp),
	}

	// Compile common dangerous patterns
	v.patterns["sql_injection"] = regexp.MustCompile(`(?i)(union|select|insert|update|delete|drop|--|;|'|")`)
	v.patterns["xss"] = regexp.MustCompile(`(?i)(<script|javascript:|onerror|onload|onclick)`)
	v.patterns["path_traversal"] = regexp.MustCompile(`(\.\./|\.\.\\)`)
	v.patterns["command_injection"] = regexp.MustCompile(`[;&|$` + "`" + `]`)

	return v
}

// Validate checks input for security issues.
func (v *InputValidator) Validate(input string) []string {
	issues := make([]string, 0)

	for name, pattern := range v.patterns {
		if pattern.MatchString(input) {
			issues = append(issues, name)
		}
	}

	return issues
}

// IsSafe returns true if input has no detected security issues.
func (v *InputValidator) IsSafe(input string) bool {
	return len(v.Validate(input)) == 0
}

// AssertNoFindings fails the test if any findings exist.
func AssertNoFindings(t *testing.T, result *ScanResult) {
	t.Helper()
	if len(result.Findings) > 0 {
		t.Errorf("Security scan found %d issues", len(result.Findings))
		for _, f := range result.Findings {
			t.Logf("  [%s] %s: %s", f.Severity, f.Category, f.Title)
		}
	}
}

// AssertNoHighSeverity fails the test if high or critical findings exist.
func AssertNoHighSeverity(t *testing.T, result *ScanResult) {
	t.Helper()
	if result.HasHighOrAbove() {
		t.Error("Security scan found high or critical severity issues")
		for _, f := range result.Findings {
			if f.Severity == SeverityCritical || f.Severity == SeverityHigh {
				t.Logf("  [%s] %s: %s", f.Severity, f.Category, f.Title)
			}
		}
	}
}
