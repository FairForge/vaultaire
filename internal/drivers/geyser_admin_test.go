// internal/drivers/geyser_admin_test.go
package drivers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"
)

// ── sanitizeBucketName ────────────────────────────────────────────────────────

func TestSanitizeBucketName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{"with-hyphens", "withhyphens"},
		{"with_underscores", "withunderscores"},
		{"tenant-abc_123", "tenantabc123"},
		{"UPPERCASE", "UPPERCASE"},
		{"mixed-CASE_123!", "mixedCASE123"},
		{"---", ""},          // all stripped → empty
		{"", ""},             // already empty
		{"abc123", "abc123"}, // already clean
	}

	for _, tc := range cases {
		got := sanitizeBucketName(tc.input)
		if got != tc.want {
			t.Errorf("sanitizeBucketName(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ── CreateBucket ──────────────────────────────────────────────────────────────

// TestCreateBucket_EmptyNameAfterSanitize verifies that a name that sanitizes
// to empty string is rejected before any HTTP call is made.
func TestCreateBucket_EmptyNameAfterSanitize(t *testing.T) {
	client := newTestClient(t, nil) // nil handler — any HTTP call would panic
	_, err := client.CreateBucket(context.Background(), "---")
	if err == nil {
		t.Fatal("expected error for unsanitizable name, got nil")
	}
}

// TestCreateBucket_MissingConfig verifies that an incomplete
// GeyserProvisioningConfig is rejected before any HTTP call is made.
func TestCreateBucket_MissingConfig(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	// Deliberately empty config — no datacenter/customer/collection IDs.
	client := NewGeyserAdminClient("fake-token", "fake-user", GeyserProvisioningConfig{}, logger)
	_, err := client.CreateBucket(context.Background(), "validname")
	if err == nil {
		t.Fatal("expected error for empty provisioning config, got nil")
	}
}

// TestCreateBucket_ProvisioningThenActive simulates the happy path:
// POST returns PROVISIONING, first poll returns PROVISIONING,
// second poll returns ACTIVE.
func TestCreateBucket_ProvisioningThenActive(t *testing.T) {
	pollCount := 0

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/buckets":
			writeEnvelope(t, w, createBucketResponse{
				ID:     "test-bucket-id",
				Status: "PROVISIONING",
			})

		case r.Method == http.MethodGet && r.URL.Path == "/api/buckets/test-bucket-id":
			pollCount++
			status := "PROVISIONING"
			if pollCount >= 2 {
				status = "ACTIVE"
			}
			writeEnvelope(t, w, GeyserBucketStatus{
				ID:         "test-bucket-id",
				BucketName: "testbucket",
				Status:     status,
			})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	client := newTestClient(t, handler)
	bucket, err := client.waitForActive(context.Background(), "test-bucket-id", 30*testSecond, testSecond)
	if err != nil {
		t.Fatalf("waitForActive returned error: %v", err)
	}
	if bucket.Status != "ACTIVE" {
		t.Errorf("expected status ACTIVE, got %q", bucket.Status)
	}
	if pollCount < 2 {
		t.Errorf("expected at least 2 polls, got %d", pollCount)
	}
}

// ── DeleteBucket ─────────────────────────────────────────────────────────────

// TestDeleteBucket_Success verifies the DELETE request is sent to the correct path.
func TestDeleteBucket_Success(t *testing.T) {
	called := false

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/buckets/some-bucket-id" {
			called = true
			w.WriteHeader(http.StatusOK)
		} else {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	client := newTestClient(t, handler)
	if err := client.DeleteBucket(context.Background(), "some-bucket-id"); err != nil {
		t.Fatalf("DeleteBucket returned error: %v", err)
	}
	if !called {
		t.Error("DELETE request was never made")
	}
}

// ── GetInvoices ───────────────────────────────────────────────────────────────

// TestGetInvoices_Success verifies invoice parsing using the real API shape.
func TestGetInvoices_Success(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/invoices" {
			writeEnvelope(t, w, []GeyserInvoice{
				{
					ID:        "inv-1",
					Month:     1,
					Year:      2026,
					IsInvoice: true,
					Subtotal:  27.90,
					Total:     155.00,
					TapeCollectionInvoices: []GeyserTapeCollectionInvoice{
						{
							Name:    "Stored3",
							TBCount: 18.0,
							TBRate:  1.55,
							TBCost:  27.90,
							Cost:    27.90,
						},
					},
					MiscBilling: []GeyserMiscBilling{
						{
							Feature: "TAPE",
							Label:   "Minimum TBs Count Balance",
							Amount:  82.0,
							Rate:    1.55,
							Total:   127.10,
						},
					},
				},
				{
					ID:        "inv-2",
					Month:     2,
					Year:      2026,
					IsInvoice: false, // pending estimate
					Subtotal:  31.00,
					Total:     155.00,
				},
			})
		} else {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	client := newTestClient(t, handler)
	invoices, err := client.GetInvoices(context.Background())
	if err != nil {
		t.Fatalf("GetInvoices returned error: %v", err)
	}
	if len(invoices) != 2 {
		t.Fatalf("expected 2 invoices, got %d", len(invoices))
	}

	first := invoices[0]
	if first.ID != "inv-1" {
		t.Errorf("expected id inv-1, got %q", first.ID)
	}
	if !first.IsInvoice {
		t.Error("expected first invoice to be finalised (IsInvoice=true)")
	}
	if first.Total != 155.00 {
		t.Errorf("expected total 155.00, got %v", first.Total)
	}
	if len(first.TapeCollectionInvoices) != 1 {
		t.Errorf("expected 1 tape collection line, got %d", len(first.TapeCollectionInvoices))
	}
	if first.TapeCollectionInvoices[0].TBCount != 18.0 {
		t.Errorf("expected TBCount 18.0, got %v", first.TapeCollectionInvoices[0].TBCount)
	}

	second := invoices[1]
	if second.IsInvoice {
		t.Error("expected second invoice to be an estimate (IsInvoice=false)")
	}
}

// ── Test helpers ──────────────────────────────────────────────────────────────

// testSecond is a short duration used in tests to keep poll intervals fast
// without hitting real wall time. 10ms * 30 = 300ms max wait per test.
const testSecond = 10 * time.Millisecond

// newTestClient creates a GeyserAdminClient pointed at a local httptest.Server.
// provConfig is pre-filled with fake but non-empty values so CreateBucket's
// config validation does not fire during tests focused on other behaviour.
func newTestClient(t *testing.T, handler http.Handler) *GeyserAdminClient {
	t.Helper()

	var srv *httptest.Server
	if handler != nil {
		srv = httptest.NewServer(handler)
		t.Cleanup(srv.Close)
	}

	logger, _ := zap.NewDevelopment()
	cfg := GeyserProvisioningConfig{
		DatacenterID:     "test-dc-id",
		CustomerID:       "test-customer-id",
		TapeCollectionID: "test-collection-id",
	}
	client := NewGeyserAdminClient("fake-token", "fake-user", cfg, logger)

	if srv != nil {
		client.httpClient = &http.Client{
			Transport: rewriteTransport{target: srv.URL, inner: http.DefaultTransport},
		}
	}

	return client
}

// rewriteTransport redirects all outbound requests to a test server URL,
// preserving the path and query string. This lets us test code that has
// the real base URL baked in as a constant.
type rewriteTransport struct {
	target string
	inner  http.RoundTripper
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = rt.target[len("http://"):]
	return rt.inner.RoundTrip(req)
}

// writeEnvelope serialises v into a geyserEnvelope and writes it as JSON.
func writeEnvelope(t *testing.T, w http.ResponseWriter, v interface{}) {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("writeEnvelope: marshal body: %v", err)
	}
	env := geyserEnvelope{
		Body:   json.RawMessage(body),
		Status: "OK",
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(env); err != nil {
		t.Fatalf("writeEnvelope: write response: %v", err)
	}
}
