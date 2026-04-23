package drivers

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// geyserS3Mock mimics S3 responses for Geyser storage tests.
type geyserS3Mock struct {
	mu      sync.RWMutex
	objects map[string][]byte // key -> data
	headers map[string]http.Header
}

func newGeyserS3Mock() *geyserS3Mock {
	return &geyserS3Mock{
		objects: make(map[string][]byte),
		headers: make(map[string]http.Header),
	}
}

func (m *geyserS3Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, key := parseBucketKey(r.URL.Path)

	switch r.Method {
	case http.MethodPut:
		data, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.objects[key] = data
		m.headers[key] = r.Header.Clone()
		m.mu.Unlock()
		w.Header().Set("ETag", `"geyser-etag"`)
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			m.handleList(w, r)
			return
		}
		m.mu.RLock()
		data, ok := m.objects[key]
		m.mu.RUnlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		_, _ = w.Write(data)

	case http.MethodDelete:
		m.mu.Lock()
		delete(m.objects, key)
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case http.MethodHead:
		if key == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		m.mu.RLock()
		data, ok := m.objects[key]
		m.mu.RUnlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

func (m *geyserS3Mock) handleList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	m.mu.RLock()
	defer m.mu.RUnlock()

	var entries []s3Entry
	for k, v := range m.objects {
		if prefix != "" && !strings.HasPrefix(k, prefix) {
			continue
		}
		entries = append(entries, s3Entry{
			Key:          k,
			Size:         int64(len(v)),
			LastModified: "2026-01-01T00:00:00Z",
			ETag:         `"geyser-etag"`,
			StorageClass: "STANDARD",
		})
	}

	result := listBucketResult{
		Xmlns:       "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:        "test-bucket",
		Prefix:      prefix,
		IsTruncated: false,
		Contents:    entries,
	}

	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}

func newTestGeyserDriver(t *testing.T, mock *geyserS3Mock) *GeyserDriver {
	t.Helper()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)

	logger, _ := zap.NewDevelopment()
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		Region:       "us-west-2",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		UsePathStyle: true,
	})

	return &GeyserDriver{
		client:   client,
		bucket:   "test-bucket",
		tenantID: "tenant1",
		logger:   logger,
		endpoint: srv.URL,
	}
}

// ── Put/Get/Delete round-trip ────────────────────────────────────────────────

func TestGeyser_PutGetDeleteRoundtrip(t *testing.T) {
	mock := newGeyserS3Mock()
	driver := newTestGeyserDriver(t, mock)
	ctx := context.Background()

	// Put
	err := driver.Put(ctx, "photos", "cat.jpg", strings.NewReader("meow"))
	require.NoError(t, err)

	// Get
	rc, err := driver.Get(ctx, "photos", "cat.jpg")
	require.NoError(t, err)
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, "meow", string(data))

	// Delete
	require.NoError(t, driver.Delete(ctx, "photos", "cat.jpg"))

	// Get after delete should fail
	_, err = driver.Get(ctx, "photos", "cat.jpg")
	require.Error(t, err)
}

// ── Put materializes Content-Length ──────────────────────────────────────────

func TestGeyser_PutMaterializes(t *testing.T) {
	mock := newGeyserS3Mock()
	driver := newTestGeyserDriver(t, mock)
	ctx := context.Background()

	err := driver.Put(ctx, "docs", "readme.md", strings.NewReader("hello world"))
	require.NoError(t, err)

	key := "t-tenant1/docs/readme.md"
	mock.mu.RLock()
	h := mock.headers[key]
	mock.mu.RUnlock()
	require.NotNil(t, h)
	assert.NotEmpty(t, h.Get("Content-Length"), "Content-Length header must be set")
}

// ── Key format ───────────────────────────────────────────────────────────────

func TestGeyser_KeyFormat(t *testing.T) {
	mock := newGeyserS3Mock()
	driver := newTestGeyserDriver(t, mock)
	ctx := context.Background()

	require.NoError(t, driver.Put(ctx, "bucket", "file.bin", strings.NewReader("x")))

	mock.mu.RLock()
	_, ok := mock.objects["t-tenant1/bucket/file.bin"]
	mock.mu.RUnlock()
	assert.True(t, ok, "key should follow t-{tenantID}/{container}/{artifact}")
}

// ── Key format with context tenant ───────────────────────────────────────────

func TestGeyser_KeyFormatWithContextTenant(t *testing.T) {
	mock := newGeyserS3Mock()
	driver := newTestGeyserDriver(t, mock)
	ctx := context.WithValue(context.Background(), common.TenantIDKey, "ctx-tenant")

	require.NoError(t, driver.Put(ctx, "bucket", "file.bin", strings.NewReader("x")))

	mock.mu.RLock()
	_, ok := mock.objects["t-ctx-tenant/bucket/file.bin"]
	mock.mu.RUnlock()
	assert.True(t, ok, "should use tenant ID from context")
}

// ── Endpoint override ────────────────────────────────────────────────────────

func TestGeyser_EndpointOverride(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	driver, err := NewGeyserDriver("k", "s", "bucket", "t",
		logger, WithGeyserEndpoint("https://lon1.geyserdata.com"))
	require.NoError(t, err)
	assert.Equal(t, "https://lon1.geyserdata.com", driver.endpoint)
}

// ── Default endpoint ─────────────────────────────────────────────────────────

func TestGeyser_DefaultEndpoint(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	driver, err := NewGeyserDriver("k", "s", "bucket", "t", logger)
	require.NoError(t, err)
	assert.Equal(t, "https://la1.geyserdata.com", driver.endpoint)
}

// ── List with prefix ─────────────────────────────────────────────────────────

func TestGeyser_ListWithPrefix(t *testing.T) {
	mock := newGeyserS3Mock()
	driver := newTestGeyserDriver(t, mock)
	ctx := context.Background()

	require.NoError(t, driver.Put(ctx, "docs", "a.txt", strings.NewReader("a")))
	require.NoError(t, driver.Put(ctx, "docs", "b.txt", strings.NewReader("b")))
	require.NoError(t, driver.Put(ctx, "other", "c.txt", strings.NewReader("c")))

	results, err := driver.List(ctx, "docs", "")
	require.NoError(t, err)
	assert.Len(t, results, 2)
	assert.ElementsMatch(t, []string{"a.txt", "b.txt"}, results)
}

// ── HealthCheck ──────────────────────────────────────────────────────────────

func TestGeyser_HealthCheck(t *testing.T) {
	mock := newGeyserS3Mock()
	driver := newTestGeyserDriver(t, mock)

	err := driver.HealthCheck(context.Background())
	require.NoError(t, err)
}

// ── Name ─────────────────────────────────────────────────────────────────────

func TestGeyser_Name(t *testing.T) {
	mock := newGeyserS3Mock()
	driver := newTestGeyserDriver(t, mock)
	assert.Equal(t, "geyser", driver.Name())
}

// ── Exists ───────────────────────────────────────────────────────────────────

func TestGeyser_Exists(t *testing.T) {
	mock := newGeyserS3Mock()
	driver := newTestGeyserDriver(t, mock)
	ctx := context.Background()

	require.NoError(t, driver.Put(ctx, "bucket", "yes.txt", strings.NewReader("here")))

	exists, err := driver.Exists(ctx, "bucket", "yes.txt")
	require.NoError(t, err)
	assert.True(t, exists)

	exists, err = driver.Exists(ctx, "bucket", "nope.txt")
	require.NoError(t, err)
	assert.False(t, exists)
}
