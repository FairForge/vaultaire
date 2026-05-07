package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestExtractS3Metadata(t *testing.T) {
	r := httptest.NewRequest("PUT", "/bucket/key", nil)
	r.Header.Set("X-Amz-Meta-Color", "blue")
	r.Header.Set("X-Amz-Meta-Author", "Alice")
	r.Header.Set("Content-Type", "text/plain")

	meta := extractS3Metadata(r)
	assert.Equal(t, "blue", meta["color"])
	assert.Equal(t, "Alice", meta["author"])
	assert.Len(t, meta, 2)
}

func TestSetS3MetadataHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	metaJSON, _ := json.Marshal(map[string]string{"color": "blue", "size": "large"})
	setS3MetadataHeaders(w, metaJSON)

	assert.Equal(t, "blue", w.Header().Get("X-Amz-Meta-color"))
	assert.Equal(t, "large", w.Header().Get("X-Amz-Meta-size"))
}

func TestSetS3MetadataHeaders_Empty(t *testing.T) {
	w := httptest.NewRecorder()
	setS3MetadataHeaders(w, nil)
	setS3MetadataHeaders(w, []byte("{}"))
	assert.Empty(t, w.Header().Get("X-Amz-Meta-color"))
}

func TestValidateMetadata(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		meta := map[string]string{"key": "value"}
		assert.NoError(t, validateMetadata(meta))
	})

	t.Run("too many keys", func(t *testing.T) {
		meta := make(map[string]string)
		for i := 0; i < 51; i++ {
			meta[fmt.Sprintf("key-%03d", i)] = "v"
		}
		assert.Error(t, validateMetadata(meta))
	})

	t.Run("value too long", func(t *testing.T) {
		meta := map[string]string{"key": strings.Repeat("x", 501)}
		assert.Error(t, validateMetadata(meta))
	})

	t.Run("total too large", func(t *testing.T) {
		meta := make(map[string]string)
		for i := 0; i < 40; i++ {
			meta[fmt.Sprintf("key-%03d", i)] = strings.Repeat("v", 60)
		}
		assert.Error(t, validateMetadata(meta))
	})
}

func TestMergeMetadata(t *testing.T) {
	t.Run("add keys", func(t *testing.T) {
		existing := json.RawMessage(`{"color":"blue"}`)
		patch := map[string]interface{}{"size": "large"}
		result, err := mergeMetadata(existing, patch)
		require.NoError(t, err)

		var merged map[string]string
		require.NoError(t, json.Unmarshal(result, &merged))
		assert.Equal(t, "blue", merged["color"])
		assert.Equal(t, "large", merged["size"])
	})

	t.Run("delete key with null", func(t *testing.T) {
		existing := json.RawMessage(`{"color":"blue","size":"large"}`)
		patch := map[string]interface{}{"color": nil}
		result, err := mergeMetadata(existing, patch)
		require.NoError(t, err)

		var merged map[string]string
		require.NoError(t, json.Unmarshal(result, &merged))
		assert.Empty(t, merged["color"])
		assert.Equal(t, "large", merged["size"])
	})

	t.Run("overwrite key", func(t *testing.T) {
		existing := json.RawMessage(`{"color":"blue"}`)
		patch := map[string]interface{}{"color": "red"}
		result, err := mergeMetadata(existing, patch)
		require.NoError(t, err)

		var merged map[string]string
		require.NoError(t, json.Unmarshal(result, &merged))
		assert.Equal(t, "red", merged["color"])
	})

	t.Run("non-string value rejected", func(t *testing.T) {
		existing := json.RawMessage(`{}`)
		patch := map[string]interface{}{"count": float64(42)}
		_, err := mergeMetadata(existing, patch)
		assert.Error(t, err)
	})

	t.Run("empty existing", func(t *testing.T) {
		patch := map[string]interface{}{"key": "val"}
		result, err := mergeMetadata(nil, patch)
		require.NoError(t, err)

		var merged map[string]string
		require.NoError(t, json.Unmarshal(result, &merged))
		assert.Equal(t, "val", merged["key"])
	})
}

func TestManagementBucketMetadata_Get(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	logger := zap.NewNop()
	s := &Server{
		logger: logger,
		router: chi.NewRouter(),
		db:     db,
		config: newStubConfig(),
	}

	s.router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Request-Id", uuid.New().String())
			ctx := context.WithValue(r.Context(), tenantIDKey, "test-tenant")
			ctx = context.WithValue(ctx, userIDKey, "test-user")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	s.router.Get("/api/v1/manage/buckets/{name}", s.handleMgmtGetBucket)

	now := time.Now()

	mock.ExpectQuery(`SELECT name, COALESCE`).
		WithArgs("test-tenant", "my-bucket").
		WillReturnRows(sqlmock.NewRows([]string{"name", "metadata", "created_at"}).
			AddRow("my-bucket", []byte(`{"color":"blue","env":"prod"}`), now))

	req := httptest.NewRequest("GET", "/api/v1/manage/buckets/my-bucket", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "bucket", resp["object"])

	meta := resp["metadata"].(map[string]interface{})
	assert.Equal(t, "blue", meta["color"])
	assert.Equal(t, "prod", meta["env"])
}

func TestManagementBucketMetadata_Patch(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	logger := zap.NewNop()
	s := &Server{
		logger: logger,
		router: chi.NewRouter(),
		db:     db,
		config: newStubConfig(),
	}

	s.router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Request-Id", uuid.New().String())
			ctx := context.WithValue(r.Context(), tenantIDKey, "test-tenant")
			ctx = context.WithValue(ctx, userIDKey, "test-user")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	s.router.Patch("/api/v1/manage/buckets/{name}", s.handleMgmtPatchBucket)

	now := time.Now()

	mock.ExpectQuery(`SELECT COALESCE`).
		WithArgs("test-tenant", "my-bucket").
		WillReturnRows(sqlmock.NewRows([]string{"metadata", "created_at"}).
			AddRow([]byte(`{"color":"blue"}`), now))

	mock.ExpectExec(`UPDATE buckets SET metadata`).
		WithArgs(sqlmock.AnyArg(), "test-tenant", "my-bucket").
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := bytes.NewBufferString(`{"metadata":{"color":null,"env":"prod"}}`)
	req := httptest.NewRequest("PATCH", "/api/v1/manage/buckets/my-bucket", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "bucket", resp["object"])

	meta := resp["metadata"].(map[string]interface{})
	assert.Nil(t, meta["color"])
	assert.Equal(t, "prod", meta["env"])
}

func newStubConfig() *config.Config {
	return &config.Config{Server: config.ServerConfig{Port: 8000}}
}
