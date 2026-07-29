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

// ── Login / MFA ───────────────────────────────────────────────────────────────

// TestLogin_ReturnsChallenge verifies the POST /api/login wire format and that
// the MFA challenge payload is surfaced to the caller.
func TestLogin_ReturnsChallenge(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/login" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		if got := r.Header.Get("X-Source"); got != "UI" {
			t.Errorf("expected X-Source: UI header, got %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode login body: %v", err)
		}
		if body["emailAddress"] != "op@example.com" {
			t.Errorf("expected emailAddress op@example.com, got %q", body["emailAddress"])
		}
		if body["password"] != "hunter2" {
			t.Errorf("expected password hunter2, got %q", body["password"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated) // live server returns 201
		_, _ = w.Write([]byte(`{"hash":"challenge-hash","id":"challenge-id","responseType":"MFA","totpUser":false}`))
	})

	client := newTestClient(t, handler)
	ch, err := client.Login(context.Background(), "op@example.com", "hunter2")
	if err != nil {
		t.Fatalf("Login returned error: %v", err)
	}
	if ch.Hash != "challenge-hash" {
		t.Errorf("expected hash challenge-hash, got %q", ch.Hash)
	}
	if ch.ID != "challenge-id" {
		t.Errorf("expected challenge id challenge-id, got %q", ch.ID)
	}
	if ch.ResponseType != "MFA" {
		t.Errorf("expected responseType MFA, got %q", ch.ResponseType)
	}
	if ch.TOTPUser {
		t.Error("expected totpUser=false")
	}
}

// TestVerifyMFA_SetsSession verifies that PUT /api/login stores the session,
// that subsequent requests carry accessToken/userId cookies with the NEW
// session values (not the stale constructor ones), and that httpOnly cookies
// set by the server are captured by the jar and forwarded.
func TestVerifyMFA_SetsSession(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/login":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode verify body: %v", err)
			}
			if body["hash"] != "challenge-hash" {
				t.Errorf("expected hash challenge-hash, got %q", body["hash"])
			}
			if body["token"] != "123456" {
				t.Errorf("expected token 123456, got %q", body["token"])
			}
			w.Header().Set("Set-Cookie", "vailSession=httponly-opaque; Path=/; HttpOnly")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"sess-123","user":{"id":"user-456","emailAddress":"op@example.com"}}`))

		case r.Method == http.MethodGet && r.URL.Path == "/api/keepalive":
			seen := map[string][]string{}
			for _, ck := range r.Cookies() {
				seen[ck.Name] = append(seen[ck.Name], ck.Value)
			}
			if len(seen["accessToken"]) != 1 || seen["accessToken"][0] != "sess-123" {
				t.Errorf("expected exactly one accessToken cookie sess-123, got %v", seen["accessToken"])
			}
			if len(seen["userId"]) != 1 || seen["userId"][0] != "user-456" {
				t.Errorf("expected exactly one userId cookie user-456, got %v", seen["userId"])
			}
			if len(seen["vailSession"]) != 1 || seen["vailSession"][0] != "httponly-opaque" {
				t.Errorf("expected server-set vailSession cookie forwarded, got %v", seen["vailSession"])
			}
			writeEnvelope(t, w, struct{}{})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	client := newTestClient(t, handler) // constructor seeds fake-token/fake-user
	if err := client.VerifyMFA(context.Background(), "challenge-hash", "123456"); err != nil {
		t.Fatalf("VerifyMFA returned error: %v", err)
	}
	if client.accessToken != "sess-123" {
		t.Errorf("expected stored accessToken sess-123, got %q", client.accessToken)
	}
	if client.userID != "user-456" {
		t.Errorf("expected stored userID user-456, got %q", client.userID)
	}
	if err := client.keepalive(context.Background()); err != nil {
		t.Fatalf("keepalive after VerifyMFA returned error: %v", err)
	}
}

// TestVerifyMFA_InvalidCode verifies a 400 from PUT /api/login surfaces as an error.
func TestVerifyMFA_InvalidCode(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/api/login" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"Invalid token"}`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	err := client.VerifyMFA(context.Background(), "challenge-hash", "000000")
	if err == nil {
		t.Fatal("expected error for invalid MFA code, got nil")
	}
}

// ── Restore operations ────────────────────────────────────────────────────────

// TestRestoreToCache_Success verifies the request path and payload shape,
// including that an empty versionID is omitted from the body.
func TestRestoreToCache_Success(t *testing.T) {
	var got map[string]string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/buckets/bucket-1/restoreToCache" {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode restoreToCache body: %v", err)
			}
			writeEnvelope(t, w, struct{}{})
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	if err := client.RestoreToCache(context.Background(), "bucket-1", "restic/data/pack-a1", ""); err != nil {
		t.Fatalf("RestoreToCache returned error: %v", err)
	}
	if got["path"] != "restic/data/pack-a1" {
		t.Errorf("expected path restic/data/pack-a1, got %q", got["path"])
	}
	if _, has := got["versionId"]; has {
		t.Error("expected empty versionId to be omitted from payload")
	}
}

// TestRestoreToCloud_Success verifies the restore-to-integration payload.
func TestRestoreToCloud_Success(t *testing.T) {
	var got map[string]string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/buckets/bucket-1/restore" {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode restore body: %v", err)
			}
			writeEnvelope(t, w, struct{}{})
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	if err := client.RestoreToCloud(context.Background(), "bucket-1", "restic/data/pack-a1", "int-9", ""); err != nil {
		t.Fatalf("RestoreToCloud returned error: %v", err)
	}
	if got["path"] != "restic/data/pack-a1" {
		t.Errorf("expected path restic/data/pack-a1, got %q", got["path"])
	}
	if got["integrationId"] != "int-9" {
		t.Errorf("expected integrationId int-9, got %q", got["integrationId"])
	}
}

// ── Cloud integrations ────────────────────────────────────────────────────────

// TestListCloudIntegrations_Empty verifies parsing of the live-observed bare
// [] response (no envelope).
func TestListCloudIntegrations_Empty(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/buckets/bucket-1/cloudIntegrations" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	ints, err := client.ListCloudIntegrations(context.Background(), "bucket-1")
	if err != nil {
		t.Fatalf("ListCloudIntegrations returned error: %v", err)
	}
	if len(ints) != 0 {
		t.Errorf("expected 0 integrations, got %d", len(ints))
	}
}

// ── cloudSync ─────────────────────────────────────────────────────────────────

// TestCreateCloudSync_Success verifies the nested payload shape and that the
// SYNC action is defaulted when unset.
func TestCreateCloudSync_Success(t *testing.T) {
	var got map[string]interface{}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/cloudSync" {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode cloudSync body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"job-1","status":"PENDING"}`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	err := client.CreateCloudSync(context.Background(), CreateCloudSyncRequest{
		Source: CloudSyncSource{
			Type:      "WASABI",
			Region:    "us-west-1",
			Bucket:    "source-bucket",
			AccessKey: "AKTEST",
			SecretKey: "SKTEST",
		},
		BucketID: "bucket-1",
	})
	if err != nil {
		t.Fatalf("CreateCloudSync returned error: %v", err)
	}

	if got["action"] != "SYNC" {
		t.Errorf("expected action SYNC (defaulted), got %v", got["action"])
	}
	if got["bucketId"] != "bucket-1" {
		t.Errorf("expected bucketId bucket-1, got %v", got["bucketId"])
	}
	source, ok := got["source"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected nested source object, got %T", got["source"])
	}
	if source["type"] != "WASABI" {
		t.Errorf("expected source.type WASABI, got %v", source["type"])
	}
	if source["bucket"] != "source-bucket" {
		t.Errorf("expected source.bucket source-bucket, got %v", source["bucket"])
	}
	if source["accessKey"] != "AKTEST" || source["secretKey"] != "SKTEST" {
		t.Errorf("expected source credentials AKTEST/SKTEST, got %v/%v", source["accessKey"], source["secretKey"])
	}
}

// TestGetCloudSyncStatus_RSQLQuery verifies the RSQL query parameter and
// job-list parsing.
func TestGetCloudSyncStatus_RSQLQuery(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/cloudSync" {
			if q := r.URL.Query().Get("query"); q != "bucketId==bucket-1" {
				t.Errorf("expected query bucketId==bucket-1, got %q", q)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"job-1","bucketId":"bucket-1","status":"INPROGRESS"}]`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	jobs, err := client.GetCloudSyncStatus(context.Background(), "bucket-1")
	if err != nil {
		t.Fatalf("GetCloudSyncStatus returned error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].Status != "INPROGRESS" {
		t.Errorf("expected status INPROGRESS, got %q", jobs[0].Status)
	}
}

// ── Tape / site / event info ──────────────────────────────────────────────────

// TestGetTapeCollections_ParsesTapeDetail verifies envelope unwrapping and the
// tape-detail field tags against a raw JSON fixture.
func TestGetTapeCollections_ParsesTapeDetail(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/tapeCollections" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"body":[{"id":"tc-1","name":"Stored3Lib","tapes":[{"barcode":"140241L9","serial":"HPE-1925523288","type":"LTO-9","available":17500000000000,"total":17500000000000,"writeProtected":false}]}],"status":"OK","headers":{"authId":""}}`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	cols, err := client.GetTapeCollections(context.Background())
	if err != nil {
		t.Fatalf("GetTapeCollections returned error: %v", err)
	}
	if len(cols) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(cols))
	}
	if cols[0].Name != "Stored3Lib" {
		t.Errorf("expected name Stored3Lib, got %q", cols[0].Name)
	}
	if len(cols[0].Tapes) != 1 {
		t.Fatalf("expected 1 tape, got %d", len(cols[0].Tapes))
	}
	tape := cols[0].Tapes[0]
	if tape.Barcode != "140241L9" {
		t.Errorf("expected barcode 140241L9, got %q", tape.Barcode)
	}
	if tape.Serial != "HPE-1925523288" {
		t.Errorf("expected serial HPE-1925523288, got %q", tape.Serial)
	}
	if tape.TotalBytes != 17500000000000 {
		t.Errorf("expected total 17.5TB, got %d", tape.TotalBytes)
	}
	if tape.WriteProtected {
		t.Error("expected writeProtected=false")
	}
}

// TestGetSites_ParsesBareJSON verifies the bare-JSON (non-envelope) fallback
// against the live-observed /api/sites response shape.
func TestGetSites_ParsesBareJSON(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/sites" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"site-la","name":"Los Angeles US"},{"id":"site-lon","name":"London UK"},{"id":"site-sp","name":"São Paulo, Brazil"}]`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	sites, err := client.GetSites(context.Background())
	if err != nil {
		t.Fatalf("GetSites returned error: %v", err)
	}
	if len(sites) != 3 {
		t.Fatalf("expected 3 sites, got %d", len(sites))
	}
	if sites[2].Name != "São Paulo, Brazil" {
		t.Errorf("expected São Paulo site, got %q", sites[2].Name)
	}
}

// TestGetEvents_Success verifies audit-log parsing.
func TestGetEvents_Success(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/events" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"body":[{"id":"ev-1","type":"LOGIN","createdAt":"2026-07-29T15:00:00Z"}],"status":"OK","headers":{"authId":""}}`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	events, err := client.GetEvents(context.Background())
	if err != nil {
		t.Fatalf("GetEvents returned error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "LOGIN" {
		t.Errorf("expected event type LOGIN, got %q", events[0].Type)
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
			Jar:       client.httpClient.Jar, // keep the cookie jar the login flow relies on
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
