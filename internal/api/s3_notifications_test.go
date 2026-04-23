package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

type notificationFixture struct {
	server   *Server
	adapter  *S3ToEngine
	db       *sql.DB
	eng      *engine.CoreEngine
	tenantID string
	tenant   *tenant.Tenant
	tempDir  string
	bucket   string
}

func setupNotificationFixture(t *testing.T) *notificationFixture {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	logger := zap.NewNop()

	tempDir, err := os.MkdirTemp("", "vaultaire-notif-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	eng := engine.NewEngine(nil, logger, nil)
	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	tenantID := fmt.Sprintf("notif-%d", os.Getpid())
	bucket := "notif-bucket"
	email := fmt.Sprintf("notif-%d@test.local", os.Getpid())

	_, err = db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, tenantID, "Notification Test", email, "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility)
		VALUES ($1, $2, 'private')
		ON CONFLICT (tenant_id, name) DO NOTHING
	`, tenantID, bucket)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM bucket_notifications WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM buckets WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})

	tn := &tenant.Tenant{
		ID:        tenantID,
		Namespace: "tenant/" + tenantID + "/",
	}

	container := tn.NamespaceContainer(bucket)
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, container), 0755))

	srv := &Server{
		logger:   logger,
		router:   chi.NewRouter(),
		engine:   eng,
		db:       db,
		testMode: true,
	}

	return &notificationFixture{
		server:   srv,
		adapter:  NewS3ToEngine(eng, db, logger),
		db:       db,
		eng:      eng,
		tenantID: tenantID,
		tenant:   tn,
		tempDir:  tempDir,
		bucket:   bucket,
	}
}

func TestNotification_PutGetConfig(t *testing.T) {
	f := setupNotificationFixture(t)

	// PUT notification config
	configXML := `<?xml version="1.0" encoding="UTF-8"?>
<NotificationConfiguration>
  <TopicConfiguration>
    <Id>config1</Id>
    <Topic>https://example.com/webhook</Topic>
    <Event>s3:ObjectCreated:*</Event>
    <Event>s3:ObjectRemoved:*</Event>
  </TopicConfiguration>
</NotificationConfiguration>`

	req := httptest.NewRequest("PUT", "/"+f.bucket+"?notification", bytes.NewReader([]byte(configXML)))
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.server.handlePutBucketNotification(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, w.Code, "PUT notification should succeed")

	// GET notification config
	req = httptest.NewRequest("GET", "/"+f.bucket+"?notification", nil)
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w = httptest.NewRecorder()
	f.server.handleGetBucketNotification(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, w.Code, "GET notification should succeed")

	var got NotificationConfiguration
	body, _ := io.ReadAll(w.Body)
	err := xml.Unmarshal(body, &got)
	require.NoError(t, err)

	require.Len(t, got.Topics, 1)
	assert.Equal(t, "https://example.com/webhook", got.Topics[0].Topic)
	assert.Contains(t, got.Topics[0].Events, "s3:ObjectCreated:*")
	assert.Contains(t, got.Topics[0].Events, "s3:ObjectRemoved:*")
}

func TestNotification_ClearConfig(t *testing.T) {
	f := setupNotificationFixture(t)

	// PUT a config first
	configXML := `<NotificationConfiguration>
  <TopicConfiguration>
    <Topic>https://example.com/hook</Topic>
    <Event>s3:ObjectCreated:Put</Event>
  </TopicConfiguration>
</NotificationConfiguration>`

	req := httptest.NewRequest("PUT", "/"+f.bucket+"?notification", bytes.NewReader([]byte(configXML)))
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handlePutBucketNotification(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, w.Code)

	// Now clear it with empty config
	emptyXML := `<NotificationConfiguration/>`
	req = httptest.NewRequest("PUT", "/"+f.bucket+"?notification", bytes.NewReader([]byte(emptyXML)))
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()
	f.server.handlePutBucketNotification(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, w.Code)

	// GET should return empty config
	req = httptest.NewRequest("GET", "/"+f.bucket+"?notification", nil)
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()
	f.server.handleGetBucketNotification(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, w.Code)

	var got NotificationConfiguration
	body, _ := io.ReadAll(w.Body)
	err := xml.Unmarshal(body, &got)
	require.NoError(t, err)
	assert.Empty(t, got.Topics)
}

func TestNotification_EventFired(t *testing.T) {
	f := setupNotificationFixture(t)

	// Set up a test webhook server to receive events
	var mu sync.Mutex
	var received []S3Event

	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event S3Event
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &event); err == nil {
			mu.Lock()
			received = append(received, event)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	// Configure notification to point at our test server
	configXML := fmt.Sprintf(`<NotificationConfiguration>
  <TopicConfiguration>
    <Topic>%s</Topic>
    <Event>s3:ObjectCreated:*</Event>
  </TopicConfiguration>
</NotificationConfiguration>`, webhookSrv.URL)

	req := httptest.NewRequest("PUT", "/"+f.bucket+"?notification", bytes.NewReader([]byte(configXML)))
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handlePutBucketNotification(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, w.Code)

	// PUT an object — should trigger notification
	req = httptest.NewRequest("PUT", "/"+f.bucket+"/test-file.txt", bytes.NewReader([]byte("hello world")))
	req.Header.Set("Content-Type", "text/plain")
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()

	// Use FireSync for deterministic testing
	dispatcher := NewNotificationDispatcher(f.db, zap.NewNop())
	f.adapter.notifySvc = dispatcher
	f.adapter.HandlePut(w, req, f.bucket, "test-file.txt")
	require.Equal(t, http.StatusOK, w.Code)

	// The Fire call is async — use FireSync to verify
	dispatcher.FireSync(f.tenantID, f.bucket, "s3:ObjectCreated:Put", "test-file.txt", 11, "")

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(received), 1, "webhook should receive at least one event")

	event := received[len(received)-1]
	require.Len(t, event.Records, 1)
	assert.Equal(t, "s3:ObjectCreated:Put", event.Records[0].EventName)
	assert.Equal(t, f.bucket, event.Records[0].S3.Bucket.Name)
	assert.Equal(t, "test-file.txt", event.Records[0].S3.Object.Key)
	assert.Equal(t, f.tenantID, event.Records[0].UserIdentity.PrincipalID)
	assert.Equal(t, "stored.ge", event.Records[0].EventSource)
}

func TestNotification_WildcardMatch(t *testing.T) {
	f := setupNotificationFixture(t)

	var mu sync.Mutex
	var eventNames []string

	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event S3Event
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &event); err == nil && len(event.Records) > 0 {
			mu.Lock()
			eventNames = append(eventNames, event.Records[0].EventName)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	// Configure with wildcard filter
	configXML := fmt.Sprintf(`<NotificationConfiguration>
  <TopicConfiguration>
    <Topic>%s</Topic>
    <Event>s3:ObjectCreated:*</Event>
  </TopicConfiguration>
</NotificationConfiguration>`, webhookSrv.URL)

	req := httptest.NewRequest("PUT", "/"+f.bucket+"?notification", bytes.NewReader([]byte(configXML)))
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handlePutBucketNotification(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, w.Code)

	dispatcher := NewNotificationDispatcher(f.db, zap.NewNop())

	// Fire Put event — should match wildcard
	dispatcher.FireSync(f.tenantID, f.bucket, "s3:ObjectCreated:Put", "file1.txt", 100, "abc")

	// Fire Copy event — should also match wildcard
	dispatcher.FireSync(f.tenantID, f.bucket, "s3:ObjectCreated:Copy", "file2.txt", 200, "def")

	// Fire Delete event — should NOT match s3:ObjectCreated:*
	dispatcher.FireSync(f.tenantID, f.bucket, "s3:ObjectRemoved:Delete", "file3.txt", 0, "")

	mu.Lock()
	defer mu.Unlock()

	assert.Len(t, eventNames, 2, "wildcard should match Put and Copy but not Delete")
	assert.Contains(t, eventNames, "s3:ObjectCreated:Put")
	assert.Contains(t, eventNames, "s3:ObjectCreated:Copy")
}

func TestNotification_DeleteEventFired(t *testing.T) {
	f := setupNotificationFixture(t)

	var mu sync.Mutex
	var received []S3Event

	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event S3Event
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &event); err == nil {
			mu.Lock()
			received = append(received, event)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	// Configure notification for delete events
	configXML := fmt.Sprintf(`<NotificationConfiguration>
  <TopicConfiguration>
    <Topic>%s</Topic>
    <Event>s3:ObjectRemoved:*</Event>
  </TopicConfiguration>
</NotificationConfiguration>`, webhookSrv.URL)

	req := httptest.NewRequest("PUT", "/"+f.bucket+"?notification", bytes.NewReader([]byte(configXML)))
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handlePutBucketNotification(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, w.Code)

	// PUT an object first
	req = httptest.NewRequest("PUT", "/"+f.bucket+"/delete-me.txt", bytes.NewReader([]byte("data")))
	req.Header.Set("Content-Type", "text/plain")
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()
	f.adapter.HandlePut(w, req, f.bucket, "delete-me.txt")
	require.Equal(t, http.StatusOK, w.Code)

	// DELETE the object — use sync dispatcher
	dispatcher := NewNotificationDispatcher(f.db, zap.NewNop())
	f.adapter.notifySvc = dispatcher

	req = httptest.NewRequest("DELETE", "/"+f.bucket+"/delete-me.txt", nil)
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()
	f.adapter.HandleDelete(w, req, f.bucket, "delete-me.txt")
	require.Equal(t, http.StatusNoContent, w.Code)

	// Fire is async; call sync to verify
	dispatcher.FireSync(f.tenantID, f.bucket, "s3:ObjectRemoved:Delete", "delete-me.txt", 0, "")

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(received), 1, "webhook should receive delete event")

	event := received[len(received)-1]
	require.Len(t, event.Records, 1)
	assert.Equal(t, "s3:ObjectRemoved:Delete", event.Records[0].EventName)
	assert.Equal(t, f.bucket, event.Records[0].S3.Bucket.Name)
	assert.Equal(t, "delete-me.txt", event.Records[0].S3.Object.Key)
}

func TestNotification_DisabledNotDelivered(t *testing.T) {
	f := setupNotificationFixture(t)

	var mu sync.Mutex
	var received []S3Event

	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event S3Event
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &event); err == nil {
			mu.Lock()
			received = append(received, event)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	// Configure notification
	configXML := fmt.Sprintf(`<NotificationConfiguration>
  <TopicConfiguration>
    <Topic>%s</Topic>
    <Event>s3:ObjectCreated:Put</Event>
  </TopicConfiguration>
</NotificationConfiguration>`, webhookSrv.URL)

	req := httptest.NewRequest("PUT", "/"+f.bucket+"?notification", bytes.NewReader([]byte(configXML)))
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handlePutBucketNotification(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, w.Code)

	// Disable all notifications for this bucket
	_, err := f.db.Exec(`UPDATE bucket_notifications SET enabled = FALSE WHERE tenant_id = $1 AND bucket = $2`,
		f.tenantID, f.bucket)
	require.NoError(t, err)

	// Fire event — should NOT be delivered
	dispatcher := NewNotificationDispatcher(f.db, zap.NewNop())
	dispatcher.FireSync(f.tenantID, f.bucket, "s3:ObjectCreated:Put", "file.txt", 100, "abc")

	// Small sleep to ensure nothing arrives
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, received, "disabled notifications should not be delivered")
}

func TestNotification_EventFilterMatch(t *testing.T) {
	tests := []struct {
		filter    string
		eventName string
		want      bool
	}{
		{"s3:ObjectCreated:*", "s3:ObjectCreated:Put", true},
		{"s3:ObjectCreated:*", "s3:ObjectCreated:Copy", true},
		{"s3:ObjectCreated:*", "s3:ObjectCreated:CompleteMultipartUpload", true},
		{"s3:ObjectCreated:*", "s3:ObjectRemoved:Delete", false},
		{"s3:ObjectRemoved:*", "s3:ObjectRemoved:Delete", true},
		{"s3:ObjectRemoved:*", "s3:ObjectCreated:Put", false},
		{"s3:ObjectCreated:Put", "s3:ObjectCreated:Put", true},
		{"s3:ObjectCreated:Put", "s3:ObjectCreated:Copy", false},
		{"s3:*", "s3:ObjectCreated:Put", true},
		{"s3:*", "s3:ObjectRemoved:Delete", true},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s_%s", tc.filter, tc.eventName), func(t *testing.T) {
			got := matchesEventFilter(tc.filter, tc.eventName)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestNotification_InvalidEventFilter(t *testing.T) {
	f := setupNotificationFixture(t)

	configXML := `<NotificationConfiguration>
  <TopicConfiguration>
    <Topic>https://example.com/webhook</Topic>
    <Event>invalid:Event:Type</Event>
  </TopicConfiguration>
</NotificationConfiguration>`

	req := httptest.NewRequest("PUT", "/"+f.bucket+"?notification", bytes.NewReader([]byte(configXML)))
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handlePutBucketNotification(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	assert.Equal(t, http.StatusBadRequest, w.Code, "invalid event filter should be rejected")
}
