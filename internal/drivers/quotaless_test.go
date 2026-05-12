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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// s3Mock is a minimal S3-compatible HTTP handler for unit tests.
type s3Mock struct {
	mu          sync.RWMutex
	objects     map[string][]byte // bucket/key -> data
	requests    atomic.Int32
	failN       int // fail the first N requests
	headerMu    sync.Mutex
	lastHeaders http.Header
}

type listBucketResult struct {
	XMLName     xml.Name  `xml:"ListBucketResult"`
	Xmlns       string    `xml:"xmlns,attr"`
	Name        string    `xml:"Name"`
	Prefix      string    `xml:"Prefix"`
	IsTruncated bool      `xml:"IsTruncated"`
	Contents    []s3Entry `xml:"Contents"`
}

type s3Entry struct {
	Key          string `xml:"Key"`
	Size         int64  `xml:"Size"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	StorageClass string `xml:"StorageClass"`
}

func newS3Mock() *s3Mock {
	return &s3Mock{objects: make(map[string][]byte)}
}

func (m *s3Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.headerMu.Lock()
	m.lastHeaders = r.Header.Clone()
	m.headerMu.Unlock()

	n := int(m.requests.Add(1))
	if n <= m.failN {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	bucket, key := parseBucketKey(r.URL.Path)

	switch r.Method {
	case http.MethodPut:
		data, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.objects[bucket+"/"+key] = data
		m.mu.Unlock()
		w.Header().Set("ETag", `"mock-etag"`)
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			m.handleListV2(w, r, bucket)
			return
		}
		m.mu.RLock()
		data, ok := m.objects[bucket+"/"+key]
		m.mu.RUnlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		_, _ = w.Write(data)

	case http.MethodDelete:
		m.mu.Lock()
		delete(m.objects, bucket+"/"+key)
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case http.MethodHead:
		m.mu.RLock()
		data, ok := m.objects[bucket+"/"+key]
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

func (m *s3Mock) handleListV2(w http.ResponseWriter, r *http.Request, bucket string) {
	prefix := r.URL.Query().Get("prefix")
	m.mu.RLock()
	defer m.mu.RUnlock()

	var entries []s3Entry
	for k, v := range m.objects {
		if !strings.HasPrefix(k, bucket+"/") {
			continue
		}
		objKey := k[len(bucket)+1:]
		if prefix != "" && !strings.HasPrefix(objKey, prefix) {
			continue
		}
		entries = append(entries, s3Entry{
			Key:          objKey,
			Size:         int64(len(v)),
			LastModified: time.Now().UTC().Format(time.RFC3339),
			ETag:         `"mock-etag"`,
			StorageClass: "STANDARD",
		})
	}

	result := listBucketResult{
		Xmlns:       "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:        bucket,
		Prefix:      prefix,
		IsTruncated: false,
		Contents:    entries,
	}

	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}

func parseBucketKey(path string) (string, string) {
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func newTestQuotalessDriver(t *testing.T, mock *s3Mock) (*QuotalessDriver, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)

	logger, _ := zap.NewDevelopment()
	driver, err := NewQuotalessDriver("test-key", "test-secret", srv.URL, logger)
	require.NoError(t, err)
	return driver, srv
}

// ── Endpoint discrimination ──────────────────────────────────────────────────

func TestQuotaless_EndpointDiscrimination(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	cases := []struct {
		endpoint string
	}{
		{"https://srv1.quotaless.cloud:8000"},
		{"https://us.quotaless.cloud:8000"},
		{"https://nl.quotaless.cloud:8000"},
		{"https://sg.quotaless.cloud:8000"},
		{"https://quotaless.cloud:8000"},
	}

	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			driver, err := NewQuotalessDriver("k", "s", tc.endpoint, logger)
			require.NoError(t, err)
			assert.Equal(t, tc.endpoint, driver.endpoint)
		})
	}
}

// ── Root path prefix ─────────────────────────────────────────────────────────

func TestQuotaless_RootPathPrefix(t *testing.T) {
	mock := newS3Mock()
	driver, _ := newTestQuotalessDriver(t, mock)
	ctx := context.Background()

	require.NoError(t, driver.Put(ctx, "mybucket", "myfile.txt", strings.NewReader("hello")))

	mock.mu.RLock()
	_, ok := mock.objects["data/personal-files/mybucket/myfile.txt"]
	mock.mu.RUnlock()
	assert.True(t, ok, "expected key with personal-files prefix in mock store")
}

// ── Path construction ────────────────────────────────────────────────────────

func TestQuotaless_PathConstruction(t *testing.T) {
	mock := newS3Mock()
	driver, _ := newTestQuotalessDriver(t, mock)
	ctx := context.Background()

	cases := []struct {
		container, artifact, wantKey string
	}{
		{"mybucket", "myfile.txt", "data/personal-files/mybucket/myfile.txt"},
		{"tenant1", "docs/report.pdf", "data/personal-files/tenant1/docs/report.pdf"},
		{"bucket", "a/b/c/deep.bin", "data/personal-files/bucket/a/b/c/deep.bin"},
	}

	for _, tc := range cases {
		t.Run(tc.wantKey, func(t *testing.T) {
			require.NoError(t, driver.Put(ctx, tc.container, tc.artifact, strings.NewReader("x")))
			mock.mu.RLock()
			_, ok := mock.objects[tc.wantKey]
			mock.mu.RUnlock()
			assert.True(t, ok, "expected key %s in mock store", tc.wantKey)
		})
	}
}

// ── Retry on failure ────────────────────────────────────────────────────────

func TestQuotaless_RetryOnFailure(t *testing.T) {
	mock := newS3Mock()
	mock.failN = 2 // first 2 requests fail, 3rd succeeds
	driver, _ := newTestQuotalessDriver(t, mock)
	ctx := context.Background()

	err := driver.Put(ctx, "bucket", "file.txt", strings.NewReader("data"))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, int(mock.requests.Load()), 3)
}

// ── Exhausted retries ───────────────────────────────────────────────────────

func TestQuotaless_ExhaustedRetries(t *testing.T) {
	mock := newS3Mock()
	mock.failN = 100 // always fail
	driver, _ := newTestQuotalessDriver(t, mock)
	ctx := context.Background()

	err := driver.Put(ctx, "bucket", "file.txt", strings.NewReader("data"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed after 3 attempts")
}

// ── Health check (TCP dial) ─────────────────────────────────────────────────

func TestQuotaless_HealthCheck_TCPDial(t *testing.T) {
	mock := newS3Mock()
	driver, srv := newTestQuotalessDriver(t, mock)

	err := driver.HealthCheck(context.Background())
	require.NoError(t, err)

	srv.Close()
	err = driver.HealthCheck(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "health check")
}

// ── List strips prefix ──────────────────────────────────────────────────────

func TestQuotaless_ListStripsPrefix(t *testing.T) {
	mock := newS3Mock()
	driver, _ := newTestQuotalessDriver(t, mock)
	ctx := context.Background()

	require.NoError(t, driver.Put(ctx, "bucket", "a.txt", strings.NewReader("aaa")))
	require.NoError(t, driver.Put(ctx, "bucket", "b.txt", strings.NewReader("bbb")))

	results, err := driver.List(ctx, "bucket", "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a.txt", "b.txt"}, results)
}

// ── Get round-trip ──────────────────────────────────────────────────────────

func TestQuotaless_GetRoundtrip(t *testing.T) {
	mock := newS3Mock()
	driver, _ := newTestQuotalessDriver(t, mock)
	ctx := context.Background()

	require.NoError(t, driver.Put(ctx, "bucket", "hello.txt", strings.NewReader("world")))

	rc, err := driver.Get(ctx, "bucket", "hello.txt")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "world", string(data))
}

// ── Delete ──────────────────────────────────────────────────────────────────

func TestQuotaless_Delete(t *testing.T) {
	mock := newS3Mock()
	driver, _ := newTestQuotalessDriver(t, mock)
	ctx := context.Background()

	require.NoError(t, driver.Put(ctx, "bucket", "del.txt", strings.NewReader("data")))
	require.NoError(t, driver.Delete(ctx, "bucket", "del.txt"))

	mock.mu.RLock()
	_, ok := mock.objects["data/personal-files/bucket/del.txt"]
	mock.mu.RUnlock()
	assert.False(t, ok, "object should be deleted")
}

// ── Exists ──────────────────────────────────────────────────────────────────

func TestQuotaless_Exists(t *testing.T) {
	mock := newS3Mock()
	driver, _ := newTestQuotalessDriver(t, mock)
	ctx := context.Background()

	require.NoError(t, driver.Put(ctx, "bucket", "exists.txt", strings.NewReader("yes")))

	exists, err := driver.Exists(ctx, "bucket", "exists.txt")
	require.NoError(t, err)
	assert.True(t, exists)

	exists, err = driver.Exists(ctx, "bucket", "nope.txt")
	require.NoError(t, err)
	assert.False(t, exists)
}

// ── Name ────────────────────────────────────────────────────────────────────

func TestQuotaless_Name(t *testing.T) {
	mock := newS3Mock()
	driver, _ := newTestQuotalessDriver(t, mock)
	assert.Equal(t, "quotaless", driver.Name())
}

// ── Get retry ───────────────────────────────────────────────────────────────

func TestQuotaless_GetRetryOnFailure(t *testing.T) {
	mock := newS3Mock()
	driver, _ := newTestQuotalessDriver(t, mock)
	ctx := context.Background()

	mock.mu.Lock()
	mock.objects["data/personal-files/bucket/retry.txt"] = []byte("retried")
	mock.mu.Unlock()

	mock.failN = 1 // first request fails
	rc, err := driver.Get(ctx, "bucket", "retry.txt")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	data, _ := io.ReadAll(rc)
	assert.Equal(t, "retried", string(data))
}

// ── UNSIGNED-PAYLOAD header ─────────────────────────────────────────────────

func TestQuotaless_UnsignedPayloadHeader(t *testing.T) {
	mock := newS3Mock()
	driver, _ := newTestQuotalessDriver(t, mock)
	ctx := context.Background()

	require.NoError(t, driver.Put(ctx, "bucket", "file.txt", strings.NewReader("data")))

	mock.headerMu.Lock()
	h := mock.lastHeaders.Get("X-Amz-Content-Sha256")
	mock.headerMu.Unlock()

	assert.Equal(t, "UNSIGNED-PAYLOAD", h)
}
