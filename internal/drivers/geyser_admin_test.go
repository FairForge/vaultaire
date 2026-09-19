// internal/drivers/geyser_admin_test.go
package drivers

import (
	"context"
	"encoding/json"
	"fmt"
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
		return
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
		return
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
			writePaged(t, w, []GeyserInvoice{
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
		return
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
	// Fixture matches the live wire capture of 2026-09-19: a bare paged
	// {content, page} wrapper (no envelope) with tapeUsage entries.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/tapeCollections" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"content":[{"id":"tc-1","name":"Stored3","size":20,"tapeCount":1,"dualCopy":false,"compression":false,"encryption":false,"datacenter":{"id":"dc-la1","name":"LA1","geo":"Los Angeles, CA"},"sustainability":{"percentSaved":97.25},"tapeUsage":[{"barcode":"140241L9","serialNumber":"HPE-1925523288","type":"lto9","availableCapacity":17538514681856,"totalCapacity":17549999734784,"writeProtected":false,"tapeId":"tape-1","lastAccessed":"2026-08-06T11:12:11.700Z"}]}],"page":{"size":10,"number":0,"totalElements":1,"totalPages":1}}`))
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
	col := cols[0]
	if col.Name != "Stored3" {
		t.Errorf("expected name Stored3, got %q", col.Name)
	}
	if col.Size != 20 {
		t.Errorf("expected size 20 TB, got %d", col.Size)
	}
	if col.Datacenter.Name != "LA1" {
		t.Errorf("expected datacenter LA1, got %q", col.Datacenter.Name)
	}
	if len(col.TapeUsage) != 1 {
		t.Fatalf("expected 1 tape, got %d", len(col.TapeUsage))
	}
	tape := col.TapeUsage[0]
	if tape.Barcode != "140241L9" {
		t.Errorf("expected barcode 140241L9, got %q", tape.Barcode)
	}
	if tape.SerialNumber != "HPE-1925523288" {
		t.Errorf("expected serial HPE-1925523288, got %q", tape.SerialNumber)
	}
	if tape.TotalCapacity != 17549999734784 {
		t.Errorf("expected total 17.55TB, got %d", tape.TotalCapacity)
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
			_, _ = w.Write([]byte(`{"content":[{"geo":"London, UK","id":"site-lon"},{"geo":"Los Angeles, US","id":"site-la"},{"geo":"Sao Paulo, Brazil","id":"site-sp"}],"page":{"size":10,"number":0,"totalElements":3,"totalPages":1}}`))
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
	if sites[2].Geo != "Sao Paulo, Brazil" {
		t.Errorf("expected São Paulo site, got %q", sites[2].Geo)
	}
}

// TestGetEvents_Success verifies audit-log parsing.
func TestGetEvents_Success(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/events" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"content":[{"id":"ev-1","name":"login","description":"User logged in","result":"SUCCESS","severity":"INFO","created":"2026-09-19T04:01:29Z"}],"page":{"size":100,"number":0,"totalElements":1,"totalPages":1}}`))
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
	if events[0].Name != "login" {
		t.Errorf("expected event name login, got %q", events[0].Name)
	}
	if events[0].Result != "SUCCESS" {
		t.Errorf("expected result SUCCESS, got %q", events[0].Result)
	}
}

// ── Spec-sync additions (console OpenAPI, live-verified 2026-09-19) ──────────

// TestCreateBucket_AcceptsCreatedStatus verifies provisioning succeeds when the
// console reports the live-observed terminal status "CREATED" (the old code
// only accepted "ACTIVE", which the wire no longer returns).
func TestCreateBucket_AcceptsCreatedStatus(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/buckets":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"bkt-1","status":"PROVISIONING"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/buckets/bkt-1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"bkt-1","bucketName":"la2bench-bkt-1","status":"CREATED","endpoint":"https://la2.geyserdata.com"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	client := newTestClient(t, handler)
	status, err := client.CreateBucket(context.Background(), "la2bench")
	if err != nil {
		t.Fatalf("CreateBucket returned error: %v", err)
	}
	if status.Status != "CREATED" {
		t.Errorf("expected status CREATED, got %q", status.Status)
	}
	if status.Endpoint != "https://la2.geyserdata.com" {
		t.Errorf("expected la2 endpoint, got %q", status.Endpoint)
	}
}

func TestGetBucketAccess_ListsValidKeys(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/buckets/bkt-1/access" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"AKIAEXAMPLEKEY0000A","initialized":true,"userARN":"arn:aws:iam::000000000063:user/u-1"}]`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	keys, err := client.GetBucketAccess(context.Background(), "bkt-1")
	if err != nil {
		t.Fatalf("GetBucketAccess returned error: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != "AKIAEXAMPLEKEY0000A" {
		t.Fatalf("unexpected keys: %+v", keys)
	}
}

func TestBrowseBucket_ParsesLocation(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/buckets/bkt-1/browse" {
			if got := r.URL.Query().Get("prefix"); got != "canary/" {
				t.Errorf("expected prefix query canary/, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"contents":[{"isFolder":false,"key":"canary/c_1.bin","lastModified":"2026-09-19T05:05:00Z","location":"CACHE","size":262144,"versionId":"v-1"},{"isFolder":true,"key":"canary/sub/","size":0}]}`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	entries, err := client.BrowseBucket(context.Background(), "bkt-1", "canary/")
	if err != nil {
		t.Fatalf("BrowseBucket returned error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Location != "CACHE" {
		t.Errorf("expected location CACHE, got %q", entries[0].Location)
	}
	if !entries[1].IsFolder {
		t.Error("expected second entry to be a folder")
	}
}

func TestPresignUploadAndDownload(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
		}
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path != "dir/obj.bin" {
			t.Errorf("expected path dir/obj.bin, got %q (err %v)", req.Path, err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/buckets/bkt-1/upload":
			_, _ = w.Write([]byte(`{"url":"https://la2.geyserdata.com/put?sig=u"}`))
		case "/api/buckets/bkt-1/download":
			_, _ = w.Write([]byte(`{"url":"https://la2.geyserdata.com/get?sig=d"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	client := newTestClient(t, handler)
	up, err := client.PresignUpload(context.Background(), "bkt-1", "dir/obj.bin")
	if err != nil || up != "https://la2.geyserdata.com/put?sig=u" {
		t.Fatalf("PresignUpload = %q, %v", up, err)
	}
	down, err := client.PresignDownload(context.Background(), "bkt-1", "dir/obj.bin")
	if err != nil || down != "https://la2.geyserdata.com/get?sig=d" {
		t.Fatalf("PresignDownload = %q, %v", down, err)
	}
}

func TestGetBucketSizeHistory_Success(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/buckets/bkt-1/size" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"bucketId":"bkt-1","startTime":"2026-09-19T03:21:52Z","endTime":"2026-09-19T09:21:52Z","logicalSize":4194304,"id":"pt-1"}]`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	points, err := client.GetBucketSizeHistory(context.Background(), "bkt-1")
	if err != nil {
		t.Fatalf("GetBucketSizeHistory returned error: %v", err)
	}
	if len(points) != 1 || points[0].LogicalSize != 4194304 {
		t.Fatalf("unexpected points: %+v", points)
	}
}

func TestGetDatacenters_Paged(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/datacenters" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"content":[{"geo":"Los Angeles, US","id":"dc-la2","name":"LA2","type":"DATACENTER"},{"geo":"London, UK","id":"dc-lon1","name":"LON1","type":"DATACENTER"}],"page":{"size":10,"number":0,"totalElements":2,"totalPages":1}}`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	dcs, err := client.GetDatacenters(context.Background())
	if err != nil {
		t.Fatalf("GetDatacenters returned error: %v", err)
	}
	if len(dcs) != 2 || dcs[0].Name != "LA2" || dcs[1].Geo != "London, UK" {
		t.Fatalf("unexpected datacenters: %+v", dcs)
	}
}

func TestGetDatacenterPricing_ParsesListAndWholesale(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/datacenterpricing" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"content":[{"id":"pr-1","datacenterId":"dc-la2","datacenterCustomerListPrice":{"tb":1.55,"compression":0.33,"encryption":0.33},"datacenterResellerCost":{"tb":1.27,"compression":0.28,"encryption":0.28}}],"page":{"size":10,"number":0,"totalElements":1,"totalPages":1}}`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	rows, err := client.GetDatacenterPricing(context.Background())
	if err != nil {
		t.Fatalf("GetDatacenterPricing returned error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].ListPrice.TB != 1.55 {
		t.Errorf("expected list $1.55/TB, got %v", rows[0].ListPrice.TB)
	}
	if rows[0].ResellerCost.TB != 1.27 {
		t.Errorf("expected wholesale $1.27/TB, got %v", rows[0].ResellerCost.TB)
	}
}

func TestEstimate_Success(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/estimates" {
			var req GeyserEstimateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode estimate request: %v", err)
			}
			if len(req.BucketParamsList) != 1 || req.BucketParamsList[0].Size != 20 {
				t.Errorf("unexpected request payload: %+v", req)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"subtotal":155.0,"total":155.0,"resellerMargin":18.06,"discount":0.0,"miscBilling":[{"feature":"TAPE","label":"Minimum TB Count Balance","amount":80.0,"rate":1.55,"total":124.0}]}`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	client := newTestClient(t, handler)
	est, err := client.Estimate(context.Background(), GeyserEstimateRequest{
		BucketParamsList: []GeyserEstimateBucketParams{{
			DualCopy: "false", Size: 20, DatacenterID: "dc-la2",
		}},
		NewCustomer: true,
	})
	if err != nil {
		t.Fatalf("Estimate returned error: %v", err)
	}
	if est.Total != 155.0 || est.ResellerMargin != 18.06 {
		t.Fatalf("unexpected estimate: %+v", est)
	}
	if len(est.MiscBilling) != 1 || est.MiscBilling[0].Amount != 80.0 {
		t.Fatalf("unexpected misc billing: %+v", est.MiscBilling)
	}
}

func TestCreateAndResizeTapeCollection(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/tapeCollections":
			var req GeyserTapeCollectionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name != "Stored3LA2" || req.Size != 1 {
				t.Errorf("unexpected create payload: %+v (err %v)", req, err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"tc-la2","name":"Stored3LA2","size":1}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/tapeCollections/tc-la2":
			var req GeyserTapeCollectionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Size != 2 {
				t.Errorf("unexpected resize payload: %+v (err %v)", req, err)
			}
			_, _ = w.Write([]byte(`{"id":"tc-la2","name":"Stored3LA2","size":2}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	client := newTestClient(t, handler)
	req := GeyserTapeCollectionRequest{
		Name: "Stored3LA2", Size: 1, DatacenterID: "dc-la2", CustomerID: "cust-1",
		Color: "#3146FF", Icon: "hard-drive-icon-outline",
	}
	created, err := client.CreateTapeCollection(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateTapeCollection returned error: %v", err)
	}
	if created.ID != "tc-la2" {
		t.Fatalf("unexpected created collection: %+v", created)
	}

	req.Size = 2
	resized, err := client.UpdateTapeCollection(context.Background(), "tc-la2", req)
	if err != nil {
		t.Fatalf("UpdateTapeCollection returned error: %v", err)
	}
	if resized.Size != 2 {
		t.Fatalf("expected size 2 after resize, got %d", resized.Size)
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

// writePaged writes v as a bare paged response ({content, page}) — the shape
// the console's list endpoints return on the live wire as of 2026-09-19.
func writePaged(t *testing.T, w http.ResponseWriter, v interface{}) {
	t.Helper()
	content, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("writePaged: marshal content: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w, `{"content":%s,"page":{"size":10,"number":0,"totalElements":1,"totalPages":1}}`, content); err != nil {
		t.Fatalf("writePaged: write response: %v", err)
	}
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
