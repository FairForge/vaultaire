package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newIdempotencyTestRouter(t *testing.T) (chi.Router, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	logger := zap.NewNop()
	im := newIdempotencyMiddleware(db, logger)

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), tenantIDKey, "test-tenant")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	r.Use(im.Middleware)

	r.Post("/test", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "created"})
	})
	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	r.Delete("/test", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"deleted": "true"})
	})
	r.Post("/error", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad"})
	})

	return r, mock, func() { _ = db.Close() }
}

func TestIdempotency_FirstRequest(t *testing.T) {
	r, mock, cleanup := newIdempotencyTestRouter(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT method, path, response_status, response_headers, response_body FROM idempotency_cache`).
		WithArgs("test-tenant", "key-1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"method", "path", "response_status", "response_headers", "response_body"}))

	mock.ExpectExec(`INSERT INTO idempotency_cache`).
		WithArgs("test-tenant", "key-1", "POST", "/test", 201, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	req := httptest.NewRequest("POST", "/test", bytes.NewBufferString(`{}`))
	req.Header.Set(idempotencyKeyHeader, "key-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Empty(t, w.Header().Get(idempotencyReplayHeader))

	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "created", resp["status"])
}

func TestIdempotency_ReplayedRequest(t *testing.T) {
	r, mock, cleanup := newIdempotencyTestRouter(t)
	defer cleanup()

	cachedHeaders, _ := json.Marshal(map[string]string{"Content-Type": "application/json"})
	cachedBody, _ := json.Marshal(map[string]string{"status": "created"})

	mock.ExpectQuery(`SELECT method, path, response_status, response_headers, response_body FROM idempotency_cache`).
		WithArgs("test-tenant", "key-replay", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"method", "path", "response_status", "response_headers", "response_body"}).
			AddRow("POST", "/test", 201, cachedHeaders, cachedBody))

	req := httptest.NewRequest("POST", "/test", bytes.NewBufferString(`{}`))
	req.Header.Set(idempotencyKeyHeader, "key-replay")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "true", w.Header().Get(idempotencyReplayHeader))
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "created", resp["status"])
}

func TestIdempotency_DifferentKey(t *testing.T) {
	r, mock, cleanup := newIdempotencyTestRouter(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT method, path, response_status, response_headers, response_body FROM idempotency_cache`).
		WithArgs("test-tenant", "key-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"method", "path", "response_status", "response_headers", "response_body"}))

	mock.ExpectExec(`INSERT INTO idempotency_cache`).
		WithArgs("test-tenant", "key-a", "POST", "/test", 201, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectQuery(`SELECT method, path, response_status, response_headers, response_body FROM idempotency_cache`).
		WithArgs("test-tenant", "key-b", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"method", "path", "response_status", "response_headers", "response_body"}))

	mock.ExpectExec(`INSERT INTO idempotency_cache`).
		WithArgs("test-tenant", "key-b", "POST", "/test", 201, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	req1 := httptest.NewRequest("POST", "/test", bytes.NewBufferString(`{}`))
	req1.Header.Set(idempotencyKeyHeader, "key-a")
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	assert.Equal(t, http.StatusCreated, w1.Code)
	assert.Empty(t, w1.Header().Get(idempotencyReplayHeader))

	req2 := httptest.NewRequest("POST", "/test", bytes.NewBufferString(`{}`))
	req2.Header.Set(idempotencyKeyHeader, "key-b")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusCreated, w2.Code)
	assert.Empty(t, w2.Header().Get(idempotencyReplayHeader))
}

func TestIdempotency_SkipGET(t *testing.T) {
	r, _, cleanup := newIdempotencyTestRouter(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set(idempotencyKeyHeader, "key-get")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(idempotencyReplayHeader))
}

func TestIdempotency_NoHeader(t *testing.T) {
	r, _, cleanup := newIdempotencyTestRouter(t)
	defer cleanup()

	req := httptest.NewRequest("POST", "/test", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Empty(t, w.Header().Get(idempotencyReplayHeader))
}

func TestIdempotency_TooLong(t *testing.T) {
	r, _, cleanup := newIdempotencyTestRouter(t)
	defer cleanup()

	longKey := strings.Repeat("x", 257)
	req := httptest.NewRequest("POST", "/test", bytes.NewBufferString(`{}`))
	req.Header.Set(idempotencyKeyHeader, longKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]interface{})
	assert.Equal(t, ErrTypeInvalidRequest, errObj["type"])
	assert.Equal(t, "idempotency_key_too_long", errObj["code"])
}

func TestIdempotency_MethodMismatch(t *testing.T) {
	r, mock, cleanup := newIdempotencyTestRouter(t)
	defer cleanup()

	cachedHeaders, _ := json.Marshal(map[string]string{"Content-Type": "application/json"})
	cachedBody, _ := json.Marshal(map[string]string{"deleted": "true"})

	mock.ExpectQuery(`SELECT method, path, response_status, response_headers, response_body FROM idempotency_cache`).
		WithArgs("test-tenant", "key-reuse", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"method", "path", "response_status", "response_headers", "response_body"}).
			AddRow("DELETE", "/test", 200, cachedHeaders, cachedBody))

	req := httptest.NewRequest("POST", "/test", bytes.NewBufferString(`{}`))
	req.Header.Set(idempotencyKeyHeader, "key-reuse")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]interface{})
	assert.Equal(t, "idempotency_key_reuse", errObj["code"])
}

func TestIdempotency_ErrorNotCached(t *testing.T) {
	r, mock, cleanup := newIdempotencyTestRouter(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT method, path, response_status, response_headers, response_body FROM idempotency_cache`).
		WithArgs("test-tenant", "key-err", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"method", "path", "response_status", "response_headers", "response_body"}))

	req := httptest.NewRequest("POST", "/error", bytes.NewBufferString(`{}`))
	req.Header.Set(idempotencyKeyHeader, "key-err")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, w.Header().Get(idempotencyReplayHeader))

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestIdempotency_DeleteMethod(t *testing.T) {
	r, mock, cleanup := newIdempotencyTestRouter(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT method, path, response_status, response_headers, response_body FROM idempotency_cache`).
		WithArgs("test-tenant", "key-del", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"method", "path", "response_status", "response_headers", "response_body"}))

	mock.ExpectExec(`INSERT INTO idempotency_cache`).
		WithArgs("test-tenant", "key-del", "DELETE", "/test", 200, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	req := httptest.NewRequest("DELETE", "/test", nil)
	req.Header.Set(idempotencyKeyHeader, "key-del")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get(idempotencyReplayHeader))
}
