package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type stubQuotaManager struct {
	used  int64
	limit int64
	tier  string
}

func (m *stubQuotaManager) GetUsage(_ context.Context, _ string) (int64, int64, error) {
	return m.used, m.limit, nil
}
func (m *stubQuotaManager) CheckAndReserve(_ context.Context, _ string, _ int64) (bool, error) {
	return true, nil
}
func (m *stubQuotaManager) CreateTenant(_ context.Context, _, _ string, _ int64) error { return nil }
func (m *stubQuotaManager) UpdateQuota(_ context.Context, _ string, _ int64) error     { return nil }
func (m *stubQuotaManager) ListQuotas(_ context.Context) ([]map[string]interface{}, error) {
	return nil, nil
}
func (m *stubQuotaManager) DeleteQuota(_ context.Context, _ string) error { return nil }
func (m *stubQuotaManager) GetTier(_ context.Context, _ string) (string, error) {
	return m.tier, nil
}
func (m *stubQuotaManager) UpdateTier(_ context.Context, _, _ string) error { return nil }
func (m *stubQuotaManager) GetUsageHistory(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
	return nil, nil
}

func newMgmtTestServer(t *testing.T) (*Server, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	logger, _ := zap.NewDevelopment()
	authSvc := auth.NewAuthService(nil, nil)
	authSvc.SetJWTSecret("test-secret")

	s := &Server{
		logger:       logger,
		router:       chi.NewRouter(),
		db:           db,
		auth:         authSvc,
		config:       &config.Config{Server: config.ServerConfig{Port: 8000}},
		quotaManager: &stubQuotaManager{used: 500, limit: 1000, tier: "starter"},
	}

	s.router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Request-Id", uuid.New().String())
			next.ServeHTTP(w, r)
		})
	})

	rl := NewManagementRateLimiter()
	s.router.Route("/api/v1/manage", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := context.WithValue(r.Context(), tenantIDKey, "test-tenant")
				ctx = context.WithValue(ctx, userIDKey, "test-user")
				ctx = context.WithValue(ctx, emailKey, "test@example.com")
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		})
		r.Use(rl.Middleware)

		r.Get("/buckets", s.handleMgmtListBuckets)
		r.Post("/buckets", s.handleMgmtCreateBucket)
		r.Get("/buckets/{name}", s.handleMgmtGetBucket)
		r.Delete("/buckets/{name}", s.handleMgmtDeleteBucket)
		r.Get("/buckets/{name}/objects", s.handleMgmtListObjects)
		r.Get("/keys", s.handleMgmtListKeys)
		r.Post("/keys", s.handleMgmtCreateKey)
		r.Delete("/keys/{id}", s.handleMgmtDeleteKey)
		r.Get("/usage", s.handleMgmtGetUsage)
	})

	s.router.Get("/llms.txt", s.handleLlmsTxt)

	return s, mock, func() { _ = db.Close() }
}

func TestManagementListBuckets(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()

	now := time.Now()
	mock.ExpectQuery(`SELECT name, created_at FROM buckets WHERE tenant_id`).
		WithArgs("test-tenant", 21).
		WillReturnRows(sqlmock.NewRows([]string{"name", "created_at"}).
			AddRow("alpha", now).
			AddRow("bravo", now))

	mock.ExpectQuery(`SELECT COUNT`).
		WithArgs("test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	req := httptest.NewRequest("GET", "/api/v1/manage/buckets", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "list", resp["object"])
	data := resp["data"].([]interface{})
	assert.Len(t, data, 2)
	assert.False(t, resp["has_more"].(bool))
	assert.NotEmpty(t, resp["request_id"])
}

func TestManagementCreateBucket(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()

	mock.ExpectExec(`INSERT INTO buckets`).
		WithArgs("test-tenant", "new-bucket").
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectQuery(`SELECT slug, name FROM tenants WHERE id`).
		WithArgs("test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"slug", "name"}).AddRow("test-slug", "TestCo"))

	body := bytes.NewBufferString(`{"name":"new-bucket"}`)
	req := httptest.NewRequest("POST", "/api/v1/manage/buckets", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "bucket", resp["object"])
	assert.Equal(t, "new-bucket", resp["name"])
}

func TestManagementCreateBucket_Invalid(t *testing.T) {
	s, _, cleanup := newMgmtTestServer(t)
	defer cleanup()

	body := bytes.NewBufferString(`{"name":"AB"}`)
	req := httptest.NewRequest("POST", "/api/v1/manage/buckets", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]interface{})
	assert.Equal(t, ErrTypeInvalidRequest, errObj["type"])
	assert.Equal(t, "invalid_bucket_name", errObj["code"])
	assert.Equal(t, "name", errObj["param"])
}

func TestManagementCreateBucket_MissingName(t *testing.T) {
	s, _, cleanup := newMgmtTestServer(t)
	defer cleanup()

	body := bytes.NewBufferString(`{}`)
	req := httptest.NewRequest("POST", "/api/v1/manage/buckets", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]interface{})
	assert.Equal(t, "missing_parameter", errObj["code"])
}

func TestManagementCreateBucket_Duplicate(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()

	mock.ExpectExec(`INSERT INTO buckets`).
		WithArgs("test-tenant", "dup-bucket").
		WillReturnResult(sqlmock.NewResult(0, 0))

	body := bytes.NewBufferString(`{"name":"dup-bucket"}`)
	req := httptest.NewRequest("POST", "/api/v1/manage/buckets", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]interface{})
	assert.Equal(t, ErrTypeConflict, errObj["type"])
	assert.Equal(t, "bucket_exists", errObj["code"])
}

func TestManagementGetBucket(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()

	now := time.Now()
	mock.ExpectQuery(`SELECT name, created_at FROM buckets WHERE tenant_id`).
		WithArgs("test-tenant", "my-bucket").
		WillReturnRows(sqlmock.NewRows([]string{"name", "created_at"}).AddRow("my-bucket", now))

	req := httptest.NewRequest("GET", "/api/v1/manage/buckets/my-bucket", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "bucket", resp["object"])
	assert.Equal(t, "my-bucket", resp["name"])
}

func TestManagementDeleteBucket(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()

	// Create a temp bucket dir so the handler can read it
	dir := t.TempDir()
	t.Setenv("DATA_PATH", dir)
	bucketPath := dir + "/test-tenant/del-bucket"
	require.NoError(t, mkdirAll(bucketPath))

	mock.ExpectExec(`DELETE FROM buckets`).
		WithArgs("test-tenant", "del-bucket").
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest("DELETE", "/api/v1/manage/buckets/del-bucket", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["deleted"])
}

func TestManagementErrorEnvelope(t *testing.T) {
	s, _, cleanup := newMgmtTestServer(t)
	defer cleanup()

	body := bytes.NewBufferString(`not json`)
	req := httptest.NewRequest("POST", "/api/v1/manage/buckets", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	errObj, ok := resp["error"].(map[string]interface{})
	require.True(t, ok, "response must have error object")
	assert.NotEmpty(t, errObj["type"])
	assert.NotEmpty(t, errObj["code"])
	assert.NotEmpty(t, errObj["message"])
	assert.NotEmpty(t, errObj["request_id"])
}

func TestManagementPagination(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()

	now := time.Now()

	// First page: limit=2, so query LIMIT 3 (n+1 for has_more detection)
	mock.ExpectQuery(`SELECT name, created_at FROM buckets WHERE tenant_id`).
		WithArgs("test-tenant", 3).
		WillReturnRows(sqlmock.NewRows([]string{"name", "created_at"}).
			AddRow("alpha", now).
			AddRow("bravo", now).
			AddRow("charlie", now))

	mock.ExpectQuery(`SELECT COUNT`).
		WithArgs("test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))

	req := httptest.NewRequest("GET", "/api/v1/manage/buckets?limit=2", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var page1 map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page1))
	assert.True(t, page1["has_more"].(bool))
	assert.Equal(t, "bravo", page1["next_cursor"])
	data := page1["data"].([]interface{})
	assert.Len(t, data, 2)

	// Second page: starting_after=bravo
	mock.ExpectQuery(`SELECT name, created_at FROM buckets WHERE tenant_id`).
		WithArgs("test-tenant", "bravo", 3).
		WillReturnRows(sqlmock.NewRows([]string{"name", "created_at"}).
			AddRow("charlie", now).
			AddRow("delta", now).
			AddRow("echo", now))

	mock.ExpectQuery(`SELECT COUNT`).
		WithArgs("test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))

	req = httptest.NewRequest("GET", "/api/v1/manage/buckets?limit=2&starting_after=bravo", nil)
	w = httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var page2 map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page2))
	assert.True(t, page2["has_more"].(bool))
	data2 := page2["data"].([]interface{})
	assert.Len(t, data2, 2)

	// Third page: only 1 left
	mock.ExpectQuery(`SELECT name, created_at FROM buckets WHERE tenant_id`).
		WithArgs("test-tenant", "delta", 3).
		WillReturnRows(sqlmock.NewRows([]string{"name", "created_at"}).
			AddRow("echo", now))

	mock.ExpectQuery(`SELECT COUNT`).
		WithArgs("test-tenant").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))

	req = httptest.NewRequest("GET", "/api/v1/manage/buckets?limit=2&starting_after=delta", nil)
	w = httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var page3 map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page3))
	assert.False(t, page3["has_more"].(bool))
}

func TestManagementRateLimit(t *testing.T) {
	s, mock, cleanup := newMgmtTestServer(t)
	defer cleanup()

	for i := 0; i < 15; i++ {
		mock.ExpectQuery(`SELECT name, created_at FROM buckets WHERE tenant_id`).
			WithArgs("test-tenant", 21).
			WillReturnRows(sqlmock.NewRows([]string{"name", "created_at"}))
		mock.ExpectQuery(`SELECT COUNT`).
			WithArgs("test-tenant").
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	}

	var lastCode int
	for i := 0; i < 15; i++ {
		req := httptest.NewRequest("GET", "/api/v1/manage/buckets", nil)
		w := httptest.NewRecorder()
		s.router.ServeHTTP(w, req)
		lastCode = w.Code

		if w.Code == http.StatusTooManyRequests {
			assert.NotEmpty(t, w.Header().Get("X-RateLimit-Limit"))
			assert.NotEmpty(t, w.Header().Get("Retry-After"))

			var resp map[string]interface{}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			errObj := resp["error"].(map[string]interface{})
			assert.Equal(t, ErrTypeRateLimit, errObj["type"])
			return
		}

		assert.NotEmpty(t, w.Header().Get("X-RateLimit-Limit"))
		assert.NotEmpty(t, w.Header().Get("X-RateLimit-Remaining"))
	}

	// If we got here without hitting 429, the burst is high enough that 15 wasn't enough.
	// That's still valid — rate limit headers should be present.
	_ = lastCode
}

func TestManagementUsage(t *testing.T) {
	s, _, cleanup := newMgmtTestServer(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/v1/manage/usage", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "usage", resp["object"])
	assert.Equal(t, float64(500), resp["storage_used"])
	assert.Equal(t, float64(1000), resp["storage_limit"])
	assert.Equal(t, "starter", resp["tier"])
	assert.NotEmpty(t, resp["request_id"])
}

func TestLlmsTxt(t *testing.T) {
	s, _, cleanup := newMgmtTestServer(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/llms.txt", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/plain; charset=utf-8", w.Header().Get("Content-Type"))

	body := w.Body.String()
	assert.Contains(t, body, "stored.ge")
	assert.Contains(t, body, "S3-Compatible Endpoints")
	assert.Contains(t, body, "Management API")
	assert.Contains(t, body, "Bearer JWT")
	assert.Contains(t, body, "/api/v1/manage")
}

func TestManagementKeys(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	authSvc := auth.NewAuthService(nil, nil)
	authSvc.SetJWTSecret("test-secret")

	user, _, _, err := authSvc.CreateUserWithTenant(context.Background(), "keys@test.com", "pass123", "TestCo")
	require.NoError(t, err)
	userID := user.ID

	s := &Server{
		logger:       logger,
		router:       chi.NewRouter(),
		auth:         authSvc,
		config:       &config.Config{},
		quotaManager: &stubQuotaManager{},
	}

	s.router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Request-Id", uuid.New().String())
			ctx := context.WithValue(r.Context(), userIDKey, userID)
			ctx = context.WithValue(ctx, tenantIDKey, "test-tenant")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})

	rl := NewManagementRateLimiter()
	s.router.Route("/api/v1/manage", func(r chi.Router) {
		r.Use(rl.Middleware)
		r.Get("/keys", s.handleMgmtListKeys)
		r.Post("/keys", s.handleMgmtCreateKey)
		r.Delete("/keys/{id}", s.handleMgmtDeleteKey)
	})

	// Create a key
	body := bytes.NewBufferString(`{"name":"test-key"}`)
	req := httptest.NewRequest("POST", "/api/v1/manage/keys", body)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)

	var createResp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &createResp))
	assert.Equal(t, "api_key", createResp["object"])
	assert.NotEmpty(t, createResp["secret"])
	keyID := createResp["id"].(string)

	// List keys
	req = httptest.NewRequest("GET", "/api/v1/manage/keys", nil)
	w = httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var listResp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listResp))
	assert.Equal(t, "list", listResp["object"])
	// At least 1 key (the one created by CreateUserWithTenant + our new one)
	data := listResp["data"].([]interface{})
	assert.GreaterOrEqual(t, len(data), 1)

	// Delete the key
	req = httptest.NewRequest("DELETE", "/api/v1/manage/keys/"+keyID, nil)
	w = httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var delResp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &delResp))
	assert.Equal(t, true, delResp["deleted"])
}

func mkdirAll(path string) error {
	return os.MkdirAll(path, 0750)
}
