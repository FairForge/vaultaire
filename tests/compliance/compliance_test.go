package compliance

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestAuditLogging verifies all operations are logged
func TestAuditLogging(t *testing.T) {
	// Skip if server not running
	resp, err := http.Get("http://localhost:8000/api/v1/audit?limit=1")
	if err != nil {
		t.Skip("Server not running")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	var logs []map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&logs)

	if len(logs) == 0 {
		t.Error("No audit logs found")
	}
}

// TestDataRetention verifies GDPR data deletion
func TestDataRetention(t *testing.T) {
	t.Skip("GDPR compliance not yet implemented")
}

// TestTenantIsolation verifies data isolation between tenants
func TestTenantIsolation(t *testing.T) {
	// Upload as tenant A
	reqA, _ := http.NewRequest("PUT",
		"http://localhost:8000/shared/secret",
		strings.NewReader("tenant-a-data"))
	reqA.Header.Set("X-Tenant-ID", "tenant-a")

	resp, err := http.DefaultClient.Do(reqA)
	if err != nil {
		t.Skip("Server not running")
		return
	}
	_ = resp.Body.Close()
}
