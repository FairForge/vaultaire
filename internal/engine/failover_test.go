package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestCircuitBreaker_AllowWhenClosed(t *testing.T) {
	cb := NewBackendCircuitBreaker()
	assert.True(t, cb.Allow())
	assert.Equal(t, StateClosed, cb.State())
}

func TestCircuitBreaker_OpensAfterFailures(t *testing.T) {
	cb := NewBackendCircuitBreaker()

	for i := 0; i < failureThreshold; i++ {
		assert.True(t, cb.Allow())
		cb.RecordFailure()
	}

	assert.Equal(t, StateOpen, cb.State())
	assert.False(t, cb.Allow())
}

func TestCircuitBreaker_DoesNotOpenOnScatteredFailures(t *testing.T) {
	cb := NewBackendCircuitBreaker()

	// 4 failures (below threshold) should not open
	for i := 0; i < failureThreshold-1; i++ {
		cb.RecordFailure()
	}

	assert.Equal(t, StateClosed, cb.State())
	assert.True(t, cb.Allow())
}

func TestCircuitBreaker_HalfOpenAfterTimeout(t *testing.T) {
	cb := NewBackendCircuitBreaker()

	for i := 0; i < failureThreshold; i++ {
		cb.RecordFailure()
	}
	assert.Equal(t, StateOpen, cb.State())

	// Simulate time passing
	cb.mu.Lock()
	cb.lastOpenedAt = time.Now().Add(-openDuration - time.Second)
	cb.mu.Unlock()

	assert.Equal(t, StateHalfOpen, cb.State())
	assert.True(t, cb.Allow())
}

func TestCircuitBreaker_ClosesOnSuccess(t *testing.T) {
	cb := NewBackendCircuitBreaker()

	for i := 0; i < failureThreshold; i++ {
		cb.RecordFailure()
	}
	assert.Equal(t, StateOpen, cb.State())

	// Advance past open duration
	cb.mu.Lock()
	cb.lastOpenedAt = time.Now().Add(-openDuration - time.Second)
	cb.mu.Unlock()

	assert.True(t, cb.Allow())
	cb.RecordSuccess()

	assert.Equal(t, StateClosed, cb.State())
	assert.True(t, cb.Allow())
}

func TestCircuitBreaker_ReopensOnHalfOpenFailure(t *testing.T) {
	cb := NewBackendCircuitBreaker()

	for i := 0; i < failureThreshold; i++ {
		cb.RecordFailure()
	}

	cb.mu.Lock()
	cb.lastOpenedAt = time.Now().Add(-openDuration - time.Second)
	cb.mu.Unlock()

	assert.True(t, cb.Allow())
	cb.RecordFailure()

	assert.Equal(t, StateOpen, cb.State())
}

func TestFailoverManager_TriesNextOnFailure(t *testing.T) {
	logger := zap.NewNop()
	fm := NewFailoverManager(logger)
	fm.Register("backend-a")
	fm.Register("backend-b")

	callOrder := []string{}
	used, err := fm.Execute(context.Background(), []string{"backend-a", "backend-b"}, func(name string) error {
		callOrder = append(callOrder, name)
		if name == "backend-a" {
			return fmt.Errorf("backend-a failed")
		}
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, "backend-b", used)
	assert.Equal(t, []string{"backend-a", "backend-b"}, callOrder)
}

func TestFailoverManager_AllFail(t *testing.T) {
	logger := zap.NewNop()
	fm := NewFailoverManager(logger)
	fm.Register("backend-a")
	fm.Register("backend-b")

	_, err := fm.Execute(context.Background(), []string{"backend-a", "backend-b"}, func(name string) error {
		return fmt.Errorf("%s failed", name)
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "all backends failed")
}

func TestFailoverManager_SkipsOpenCircuitBreaker(t *testing.T) {
	logger := zap.NewNop()
	fm := NewFailoverManager(logger)
	fm.Register("backend-a")
	fm.Register("backend-b")

	// Trip backend-a's circuit breaker
	for i := 0; i < failureThreshold; i++ {
		fm.breakers["backend-a"].RecordFailure()
	}

	callOrder := []string{}
	used, err := fm.Execute(context.Background(), []string{"backend-a", "backend-b"}, func(name string) error {
		callOrder = append(callOrder, name)
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, "backend-b", used)
	assert.Equal(t, []string{"backend-b"}, callOrder)
}

func TestFailoverManager_GetAllStatuses(t *testing.T) {
	logger := zap.NewNop()
	fm := NewFailoverManager(logger)
	fm.Register("a")
	fm.Register("b")

	statuses := fm.GetAllStatuses()
	assert.Equal(t, "closed", statuses["a"])
	assert.Equal(t, "closed", statuses["b"])

	for i := 0; i < failureThreshold; i++ {
		fm.breakers["a"].RecordFailure()
	}

	statuses = fm.GetAllStatuses()
	assert.Equal(t, "open", statuses["a"])
	assert.Equal(t, "closed", statuses["b"])
}

func TestResolveStorageClass_Mapping(t *testing.T) {
	drivers := map[string]Driver{
		"idrive":   &mockDriver{name: "idrive"},
		"lyve":     &mockDriver{name: "lyve"},
		"geyser":   &mockDriver{name: "geyser"},
		"onedrive": &mockDriver{name: "onedrive"},
		"local":    &mockDriver{name: "local"},
	}

	tests := []struct {
		class           string
		expectedBackend string
		expectedClass   string
	}{
		{"STANDARD", "idrive", "STANDARD"},
		{"STANDARD_IA", "lyve", "STANDARD_IA"},
		{"GLACIER", "geyser", "GLACIER"},
		{"DEEP_ARCHIVE", "geyser", "DEEP_ARCHIVE"},
		{"ONEZONE_IA", "onedrive", "ONEZONE_IA"},
		{"REDUCED_REDUNDANCY", "local", "REDUCED_REDUNDANCY"},
		{"", "idrive", "STANDARD"},
	}

	for _, tt := range tests {
		backend, class := ResolveStorageClass(tt.class, "idrive", drivers)
		assert.Equal(t, tt.expectedBackend, backend, "class=%s", tt.class)
		assert.Equal(t, tt.expectedClass, class, "class=%s", tt.class)
	}
}

func TestResolveStorageClass_FallbackWhenMissing(t *testing.T) {
	drivers := map[string]Driver{
		"idrive": &mockDriver{name: "idrive"},
		"local":  &mockDriver{name: "local"},
	}

	// GLACIER maps to geyser which isn't registered → falls back to primary
	backend, class := ResolveStorageClass("GLACIER", "idrive", drivers)
	assert.Equal(t, "idrive", backend)
	assert.Equal(t, "GLACIER", class)
}

func TestResolveStorageClass_UnknownClass(t *testing.T) {
	drivers := map[string]Driver{
		"idrive": &mockDriver{name: "idrive"},
	}

	backend, class := ResolveStorageClass("INVALID_CLASS", "idrive", drivers)
	assert.Equal(t, "idrive", backend)
	assert.Equal(t, "STANDARD", class)
}

func TestBackendToStorageClass(t *testing.T) {
	assert.Equal(t, "STANDARD", BackendToStorageClass("idrive"))
	assert.Equal(t, "STANDARD_IA", BackendToStorageClass("lyve"))
	assert.Equal(t, "GLACIER", BackendToStorageClass("geyser"))
	assert.Equal(t, "ONEZONE_IA", BackendToStorageClass("onedrive"))
	assert.Equal(t, "REDUCED_REDUNDANCY", BackendToStorageClass("local"))
	assert.Equal(t, "STANDARD", BackendToStorageClass("s3"))
	assert.Equal(t, "STANDARD", BackendToStorageClass("unknown"))
}

func TestEnginePut_WithStorageClass(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, &Config{DefaultBackend: "idrive"})

	idriveMock := &mockDriver{name: "idrive"}
	geyserMock := &mockDriver{name: "geyser"}
	eng.AddDriver("idrive", idriveMock)
	eng.AddDriver("geyser", geyserMock)

	ctx := context.Background()
	data := strings.NewReader("test data")

	backend, err := eng.Put(ctx, "container", "artifact.dat", data, WithStorageClass("GLACIER"))
	require.NoError(t, err)
	assert.Equal(t, "geyser", backend)
	assert.Equal(t, 1, geyserMock.putCount)
	assert.Equal(t, 0, idriveMock.putCount)
}

func TestEnginePut_StorageClassFallback(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, &Config{DefaultBackend: "idrive"})

	idriveMock := &mockDriver{name: "idrive"}
	eng.AddDriver("idrive", idriveMock)

	ctx := context.Background()
	data := strings.NewReader("test data")

	// GLACIER maps to geyser which doesn't exist → falls back to primary (idrive)
	backend, err := eng.Put(ctx, "container", "artifact.dat", data, WithStorageClass("GLACIER"))
	require.NoError(t, err)
	assert.Equal(t, "idrive", backend)
	assert.Equal(t, 1, idriveMock.putCount)
}

func TestEngineGet_FailoverOnError(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, &Config{DefaultBackend: "primary"})

	primaryMock := &mockDriver{name: "primary", getErr: fmt.Errorf("primary down")}
	backupMock := &mockDriver{name: "backup", getData: "hello from backup"}
	eng.AddDriver("primary", primaryMock)
	eng.AddDriver("backup", backupMock)

	// Record object on primary
	eng.objectBackends.Store("container/obj.txt", "primary")

	ctx := context.Background()
	reader, err := eng.Get(ctx, "container", "obj.txt")
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	data, _ := io.ReadAll(reader)
	assert.Equal(t, "hello from backup", string(data))
}

func TestEngineGet_AllBackendsFail(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, &Config{DefaultBackend: "primary"})

	primaryMock := &mockDriver{name: "primary", getErr: fmt.Errorf("down")}
	backupMock := &mockDriver{name: "backup", getErr: fmt.Errorf("also down")}
	eng.AddDriver("primary", primaryMock)
	eng.AddDriver("backup", backupMock)

	ctx := context.Background()
	_, err := eng.Get(ctx, "container", "obj.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "all backends failed")
}

func TestEnginePut_FailoverOnError(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, &Config{DefaultBackend: "primary"})

	primaryMock := &mockDriver{name: "primary", putErr: fmt.Errorf("write failed")}
	backupMock := &mockDriver{name: "backup"}
	eng.AddDriver("primary", primaryMock)
	eng.AddDriver("backup", backupMock)

	ctx := context.Background()
	data := strings.NewReader("hello")

	backend, err := eng.Put(ctx, "container", "obj.txt", data)
	require.NoError(t, err)
	assert.Equal(t, "backup", backend)
}

func TestBuildCandidateList(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, &Config{DefaultBackend: "primary"})
	eng.AddDriver("primary", &mockDriver{name: "primary"})
	eng.AddDriver("backup", &mockDriver{name: "backup"})
	eng.AddDriver("archive", &mockDriver{name: "archive"})

	candidates := eng.buildCandidateList("archive")
	assert.Equal(t, "archive", candidates[0])
	assert.Equal(t, "primary", candidates[1])
	assert.Len(t, candidates, 3)
}

func TestGetFailoverStatus(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, &Config{DefaultBackend: "primary"})
	eng.AddDriver("primary", &mockDriver{name: "primary"})
	eng.AddDriver("backup", &mockDriver{name: "backup"})

	statuses := eng.GetFailoverStatus()
	assert.Equal(t, "closed", statuses["primary"])
	assert.Equal(t, "closed", statuses["backup"])
}

// mockDriver implements the Driver interface for testing.
type mockDriver struct {
	name     string
	getErr   error
	putErr   error
	getData  string
	putCount int
}

func (d *mockDriver) Name() string { return d.name }

func (d *mockDriver) Get(_ context.Context, _, _ string) (io.ReadCloser, error) {
	if d.getErr != nil {
		return nil, d.getErr
	}
	data := d.getData
	if data == "" {
		data = "mock data"
	}
	return io.NopCloser(strings.NewReader(data)), nil
}

func (d *mockDriver) Put(_ context.Context, _, _ string, _ io.Reader, _ ...PutOption) error {
	d.putCount++
	return d.putErr
}

func (d *mockDriver) Delete(_ context.Context, _, _ string) error {
	return nil
}

func (d *mockDriver) List(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}

func (d *mockDriver) Exists(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}

func (d *mockDriver) HealthCheck(_ context.Context) error {
	return nil
}

// TestIsBackendFailure_ClassifiesErrors documents which errors count toward
// opening the circuit breaker. Client-level outcomes must not.
func TestIsBackendFailure_ClassifiesErrors(t *testing.T) {
	benign := []error{
		os.ErrNotExist,
		&os.PathError{Op: "remove", Path: "/x/y", Err: os.ErrNotExist},
		NotFoundError{Container: "c", Artifact: "a"},
		ErrQuotaExceeded,
		ErrInvalidInput,
		PermissionError{TenantID: "t", Action: "write"},
		fmt.Errorf("remove /data/c/k: no such file or directory"),
		fmt.Errorf("operation error S3: GetObject, NoSuchKey: key does not exist"),
		fmt.Errorf("api error NotFound: ... https response error status code: 404"),
		fmt.Errorf("all backends failed: %w", NotFoundError{Container: "c", Artifact: "a"}),
	}
	for _, err := range benign {
		assert.False(t, isBackendFailure(err), "should NOT trip breaker: %v", err)
	}

	realFailures := []error{
		fmt.Errorf("dial tcp: connection refused"),
		fmt.Errorf("lookup e2-us-east-1.idrive.com: no such host"),
		fmt.Errorf("context deadline exceeded"),
		fmt.Errorf("https response error status code: 503"),
		fmt.Errorf("unexpected EOF"),
		ErrTimeout,
	}
	for _, err := range realFailures {
		assert.True(t, isBackendFailure(err), "SHOULD trip breaker: %v", err)
	}
}

// TestFailoverManager_NotFoundDoesNotOpenBreaker is the regression test for the
// load-test finding: a burst of not-found deletes on a single backend must not
// open its breaker (which would 503 every subsequent request for openDuration).
func TestFailoverManager_NotFoundDoesNotOpenBreaker(t *testing.T) {
	fm := NewFailoverManager(zap.NewNop())
	fm.Register("local")

	notFound := &os.PathError{Op: "remove", Path: "/data/load-test/k", Err: os.ErrNotExist}
	for i := 0; i < 3*failureThreshold; i++ {
		_, err := fm.Execute(context.Background(), []string{"local"}, func(string) error { return notFound })
		require.Error(t, err) // caller still sees the not-found
	}

	// Breaker must still be CLOSED — a real op should be attempted, not rejected.
	attempted := false
	used, err := fm.Execute(context.Background(), []string{"local"}, func(string) error {
		attempted = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, attempted, "breaker wrongly opened: real request was rejected after a 404 burst")
	assert.Equal(t, "local", used)

	// A genuine backend failure SHOULD still open it.
	for i := 0; i < failureThreshold; i++ {
		_, _ = fm.Execute(context.Background(), []string{"local"}, func(string) error {
			return fmt.Errorf("connection refused")
		})
	}
	assert.Equal(t, "open", fm.GetStatus("local"), "real failures must still open the breaker")
}
