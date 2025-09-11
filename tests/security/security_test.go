package security

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestPathTraversal attempts directory traversal attacks
func TestPathTraversal(t *testing.T) {
	attacks := []string{
		"/../../../etc/passwd",
		"/bucket/../../../root",
		"/bucket/..\\..\\..\\windows\\system32",
		"/bucket/%2e%2e%2f%2e%2e%2f",
	}

	for _, attack := range attacks {
		resp, _ := http.Get("http://localhost:8000" + attack)
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode == 200 {
				t.Errorf("Path traversal succeeded with: %s", attack)
			}
		}
	}
}

// TestSQLInjection tests for SQL injection vulnerabilities
func TestSQLInjection(t *testing.T) {
	payloads := []string{
		"'; DROP TABLE users; --",
		"1' OR '1'='1",
		"admin'--",
	}

	for _, payload := range payloads {
		req, _ := http.NewRequest("GET",
			fmt.Sprintf("http://localhost:8000/bucket/%s", payload), nil)
		req.Header.Set("X-Tenant-ID", payload)

		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
		}
	}
	// Check logs for SQL errors
	t.Log("SQL injection attempts completed - check logs for errors")
}

// TestRateLimiting verifies rate limits are enforced
func TestRateLimiting(t *testing.T) {
	// Send 100 rapid requests
	blocked := 0
	for i := 0; i < 100; i++ {
		resp, _ := http.Get("http://localhost:8000/bucket/test")
		if resp != nil {
			if resp.StatusCode == 429 {
				blocked++
			}
			_ = resp.Body.Close()
		}
	}

	if blocked == 0 {
		t.Error("No rate limiting detected after 100 rapid requests")
	}
}

// TestAuthBypass attempts to bypass authentication
func TestAuthBypass(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
	}{
		{"no_auth", nil},
		{"empty_token", map[string]string{"Authorization": ""}},
		{"malformed", map[string]string{"Authorization": "Bearer "}},
		{"jwt_none_alg", map[string]string{"Authorization": "Bearer eyJhbGciOiJub25lIn0.e30."}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("PUT",
				"http://localhost:8000/secure-bucket/data",
				strings.NewReader("test"))

			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			resp, _ := http.DefaultClient.Do(req)
			if resp != nil {
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode == 200 {
					t.Errorf("Auth bypass succeeded with: %s", tt.name)
				}
			}
		})
	}
}
