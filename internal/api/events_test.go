package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newWebhookTestServer(t *testing.T) (*Server, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	logger, _ := zap.NewDevelopment()

	s := &Server{
		logger: logger,
		router: chi.NewRouter(),
		db:     db,
		config: &config.Config{Server: config.ServerConfig{Port: 8000}},
	}

	s.router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Request-Id", uuid.New().String())
			ctx := context.WithValue(r.Context(), tenantIDKey, "test-tenant")
			ctx = context.WithValue(ctx, userIDKey, "test-user")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})

	s.router.Route("/api/v1/webhooks", func(r chi.Router) {
		r.Post("/", s.handleCreateWebhook)
		r.Get("/", s.handleListWebhooks)
		r.Patch("/{id}", s.handleUpdateWebhook)
		r.Delete("/{id}", s.handleDeleteWebhook)
		r.Get("/{id}/deliveries", s.handleListDeliveries)
		r.Post("/{id}/test", s.handleTestWebhook)
	})
	s.router.Get("/api/v1/events", s.handleListEvents)

	return s, mock, func() { _ = db.Close() }
}

func TestEmitEvent(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	logger, _ := zap.NewDevelopment()

	mock.ExpectExec(`INSERT INTO events`).
		WithArgs(sqlmock.AnyArg(), "object.created", "tenant-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery(`SELECT id, url, event_filter, secret FROM webhook_endpoints`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "url", "event_filter", "secret"}))

	emitEvent(context.Background(), db, logger, "object.created", "tenant-1", map[string]interface{}{
		"bucket": "my-bucket", "key": "file.txt", "size": 1024,
	})

	time.Sleep(100 * time.Millisecond)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestEmitEvent_NilDB(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	emitEvent(context.Background(), nil, logger, "object.created", "tenant-1", nil)
}

func TestListEvents(t *testing.T) {
	s, mock, cleanup := newWebhookTestServer(t)
	defer cleanup()

	now := time.Now().UTC()
	earlier := now.Add(-1 * time.Hour)

	mock.ExpectQuery(`SELECT id, type, data, created_at FROM events`).
		WithArgs("test-tenant", 21).
		WillReturnRows(sqlmock.NewRows([]string{"id", "type", "data", "created_at"}).
			AddRow("evt-1", "object.created", []byte(`{"bucket":"b1"}`), now).
			AddRow("evt-2", "object.deleted", []byte(`{"bucket":"b1"}`), earlier))

	mock.ExpectQuery(`SELECT COUNT`).
		WithArgs("test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	req := httptest.NewRequest("GET", "/api/v1/events", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "list", resp["object"])

	data := resp["data"].([]interface{})
	assert.Len(t, data, 2)

	first := data[0].(map[string]interface{})
	assert.Equal(t, "event", first["object"])
	assert.Equal(t, "evt-1", first["id"])
	assert.Equal(t, "object.created", first["type"])
}

func TestListEvents_TypeFilter(t *testing.T) {
	s, mock, cleanup := newWebhookTestServer(t)
	defer cleanup()

	now := time.Now().UTC()

	mock.ExpectQuery(`SELECT id, type, data, created_at FROM events.*type = \$2`).
		WithArgs("test-tenant", "object.created", 21).
		WillReturnRows(sqlmock.NewRows([]string{"id", "type", "data", "created_at"}).
			AddRow("evt-1", "object.created", []byte(`{}`), now))

	mock.ExpectQuery(`SELECT COUNT`).
		WithArgs("test-tenant", "object.created").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	req := httptest.NewRequest("GET", "/api/v1/events?type=object.created", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	data := resp["data"].([]interface{})
	assert.Len(t, data, 1)
}

func TestListEvents_TenantIsolation(t *testing.T) {
	s, mock, cleanup := newWebhookTestServer(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT id, type, data, created_at FROM events.*WHERE tenant_id = \$1`).
		WithArgs("test-tenant", 21).
		WillReturnRows(sqlmock.NewRows([]string{"id", "type", "data", "created_at"}))

	mock.ExpectQuery(`SELECT COUNT`).
		WithArgs("test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	req := httptest.NewRequest("GET", "/api/v1/events", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	data := resp["data"].([]interface{})
	assert.Empty(t, data)
	assert.Equal(t, float64(0), resp["total_count"])
}

func TestCreateWebhook(t *testing.T) {
	s, mock, cleanup := newWebhookTestServer(t)
	defer cleanup()

	mock.ExpectExec(`INSERT INTO webhook_endpoints`).
		WithArgs(sqlmock.AnyArg(), "test-tenant", "https://example.com/hook", pq.Array([]string{"object.created"}), sqlmock.AnyArg(), true, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"url":"https://example.com/hook","events":["object.created"]}`
	req := httptest.NewRequest("POST", "/api/v1/webhooks", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "webhook", resp["object"])
	assert.NotEmpty(t, resp["id"])
	assert.Equal(t, "https://example.com/hook", resp["url"])
	assert.True(t, resp["enabled"].(bool))

	secret := resp["secret"].(string)
	assert.True(t, len(secret) > 6)
	assert.Equal(t, "whsec_", secret[:6])
}

func TestCreateWebhook_ValidationErrors(t *testing.T) {
	s, _, cleanup := newWebhookTestServer(t)
	defer cleanup()

	tests := []struct {
		name string
		body string
		code string
	}{
		{"missing url", `{"events":["object.created"]}`, "missing_url"},
		{"missing events", `{"url":"https://example.com/hook"}`, "missing_events"},
		{"invalid events", `{"url":"https://example.com/hook","events":["invalid.type"]}`, "invalid_events"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/webhooks", bytes.NewBufferString(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusBadRequest, w.Code)
			var resp map[string]interface{}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			errBody := resp["error"].(map[string]interface{})
			assert.Equal(t, tt.code, errBody["code"])
		})
	}
}

func TestListWebhooks(t *testing.T) {
	s, mock, cleanup := newWebhookTestServer(t)
	defer cleanup()

	now := time.Now().UTC()

	mock.ExpectQuery(`SELECT id, url, event_filter, enabled, created_at, updated_at FROM webhook_endpoints`).
		WithArgs("test-tenant", 21).
		WillReturnRows(sqlmock.NewRows([]string{"id", "url", "event_filter", "enabled", "created_at", "updated_at"}).
			AddRow("wh-1", "https://example.com/a", pq.Array([]string{"object.*"}), true, now, now).
			AddRow("wh-2", "https://example.com/b", pq.Array([]string{"bucket.created"}), false, now, now))

	mock.ExpectQuery(`SELECT COUNT`).
		WithArgs("test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	req := httptest.NewRequest("GET", "/api/v1/webhooks", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	data := resp["data"].([]interface{})
	assert.Len(t, data, 2)

	first := data[0].(map[string]interface{})
	assert.Equal(t, "webhook", first["object"])
	assert.Nil(t, first["secret"], "secret must not be exposed in list")
}

func TestUpdateWebhook(t *testing.T) {
	s, mock, cleanup := newWebhookTestServer(t)
	defer cleanup()

	now := time.Now().UTC()

	mock.ExpectQuery(`SELECT url, event_filter, enabled, created_at FROM webhook_endpoints`).
		WithArgs("wh-123", "test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"url", "event_filter", "enabled", "created_at"}).
			AddRow("https://old.com/hook", pq.Array([]string{"object.*"}), true, now))

	mock.ExpectExec(`UPDATE webhook_endpoints`).
		WithArgs("https://new.com/hook", pq.Array([]string{"object.*"}), true, sqlmock.AnyArg(), "wh-123", "test-tenant").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"url":"https://new.com/hook"}`
	req := httptest.NewRequest("PATCH", "/api/v1/webhooks/wh-123", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "https://new.com/hook", resp["url"])
}

func TestDeleteWebhook(t *testing.T) {
	s, mock, cleanup := newWebhookTestServer(t)
	defer cleanup()

	mock.ExpectExec(`DELETE FROM webhook_endpoints`).
		WithArgs("wh-123", "test-tenant").
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest("DELETE", "/api/v1/webhooks/wh-123", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp["deleted"].(bool))
}

func TestDeleteWebhook_NotFound(t *testing.T) {
	s, mock, cleanup := newWebhookTestServer(t)
	defer cleanup()

	mock.ExpectExec(`DELETE FROM webhook_endpoints`).
		WithArgs("wh-missing", "test-tenant").
		WillReturnResult(sqlmock.NewResult(0, 0))

	req := httptest.NewRequest("DELETE", "/api/v1/webhooks/wh-missing", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestWebhookTestFire(t *testing.T) {
	s, mock, cleanup := newWebhookTestServer(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("wh-123", "test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	mock.ExpectExec(`INSERT INTO events`).
		WithArgs(sqlmock.AnyArg(), "webhook.test", "test-tenant", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery(`SELECT id, url, event_filter, secret FROM webhook_endpoints`).
		WithArgs("test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"id", "url", "event_filter", "secret"}))

	req := httptest.NewRequest("POST", "/api/v1/webhooks/wh-123/test", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "webhook_test", resp["object"])
	assert.NotEmpty(t, resp["event_id"])

	time.Sleep(100 * time.Millisecond)
}

func TestDeliveryHistory(t *testing.T) {
	s, mock, cleanup := newWebhookTestServer(t)
	defer cleanup()

	now := time.Now().UTC()

	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("wh-123", "test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	mock.ExpectQuery(`SELECT id, event_id, status, response_code, latency_ms, retry_count, created_at FROM webhook_deliveries`).
		WithArgs("wh-123", 21).
		WillReturnRows(sqlmock.NewRows([]string{"id", "event_id", "status", "response_code", "latency_ms", "retry_count", "created_at"}).
			AddRow("del-1", "evt-1", "delivered", 200, 42, 0, now).
			AddRow("del-2", "evt-2", "failed", 500, 150, 1, now.Add(-time.Minute)))

	mock.ExpectQuery(`SELECT COUNT`).
		WithArgs("wh-123").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	req := httptest.NewRequest("GET", "/api/v1/webhooks/wh-123/deliveries", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	data := resp["data"].([]interface{})
	assert.Len(t, data, 2)

	first := data[0].(map[string]interface{})
	assert.Equal(t, "webhook_delivery", first["object"])
	assert.Equal(t, "delivered", first["status"])
	assert.Equal(t, float64(200), first["response_code"])
}

func TestMatchesEventFilter(t *testing.T) {
	tests := []struct {
		filter    []string
		eventType string
		expected  bool
	}{
		{nil, "object.created", true},
		{[]string{}, "object.created", true},
		{[]string{"*"}, "object.created", true},
		{[]string{"object.created"}, "object.created", true},
		{[]string{"object.deleted"}, "object.created", false},
		{[]string{"object.*"}, "object.created", true},
		{[]string{"object.*"}, "bucket.created", false},
		{[]string{"object.created", "bucket.*"}, "bucket.deleted", true},
	}

	for _, tt := range tests {
		result := matchesWebhookFilter(tt.filter, tt.eventType)
		assert.Equal(t, tt.expected, result, "filter=%v event=%s", tt.filter, tt.eventType)
	}
}

func TestIsValidEventFilter(t *testing.T) {
	assert.True(t, isValidEventFilter([]string{"object.created"}))
	assert.True(t, isValidEventFilter([]string{"object.*"}))
	assert.True(t, isValidEventFilter([]string{"*"}))
	assert.True(t, isValidEventFilter([]string{"object.created", "bucket.deleted"}))
	assert.False(t, isValidEventFilter([]string{"invalid.type"}))
	assert.False(t, isValidEventFilter([]string{"foo.*"}))
}

func TestGenerateWebhookSecret(t *testing.T) {
	secret, err := generateWebhookSecret()
	require.NoError(t, err)
	assert.True(t, len(secret) > 6)
	assert.Equal(t, "whsec_", secret[:6])

	secret2, err := generateWebhookSecret()
	require.NoError(t, err)
	assert.NotEqual(t, secret, secret2)
}

func TestGenerateWebhookSignature(t *testing.T) {
	payload := []byte(`{"type":"object.created"}`)
	sig := generateWebhookSignature(payload, "test-secret")
	assert.True(t, len(sig) > 7)
	assert.Equal(t, "sha256=", sig[:7])

	sig2 := generateWebhookSignature(payload, "test-secret")
	assert.Equal(t, sig, sig2, "same input must produce same signature")

	sig3 := generateWebhookSignature(payload, "different-secret")
	assert.NotEqual(t, sig, sig3, "different secret must produce different signature")
}
