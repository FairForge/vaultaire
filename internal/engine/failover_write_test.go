package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fileWritingDriver is a test driver that persists artifacts as real files, so
// tests can assert whether bytes actually landed on disk (the silent-failover
// bug wrote to the hub's local disk while reporting success).
type fileWritingDriver struct {
	dir string
}

func (d *fileWritingDriver) Name() string { return "local" }

func (d *fileWritingDriver) Put(_ context.Context, container, artifact string, data io.Reader, _ ...PutOption) error {
	path := filepath.Join(d.dir, container, artifact)
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	f, err := os.Create(path) // #nosec G304 — test helper, paths under t.TempDir()
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(f, data)
	return err
}

func (d *fileWritingDriver) Get(_ context.Context, container, artifact string) (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(d.dir, container, artifact)) // #nosec G304 — test helper
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (d *fileWritingDriver) Delete(_ context.Context, container, artifact string) error {
	return os.Remove(filepath.Join(d.dir, container, artifact))
}

func (d *fileWritingDriver) List(_ context.Context, _, _ string) ([]string, error) { return nil, nil }

func (d *fileWritingDriver) Exists(_ context.Context, container, artifact string) (bool, error) {
	_, err := os.Stat(filepath.Join(d.dir, container, artifact))
	return err == nil, nil
}

func (d *fileWritingDriver) HealthCheck(_ context.Context) error { return nil }

func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	require.NoError(t, err)
	return files
}

// WP-F (1.14): a durable-backend write failure must fail the request loudly —
// never silently fall over to the hub's local disk while returning success.
func TestEnginePut_BackendFailureFailsLoudly(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, &Config{DefaultBackend: "idrive"})

	dataDir := t.TempDir()
	failing := &mockDriver{
		name:   "idrive",
		putErr: fmt.Errorf("dial tcp: connection refused"),
		getErr: fmt.Errorf("dial tcp: connection refused"),
	}
	eng.AddDriver("idrive", failing)
	eng.AddDriver("local", &fileWritingDriver{dir: dataDir})

	ctx := context.Background()
	backend, err := eng.Put(ctx, "bucket", "obj.txt", strings.NewReader("customer data"))

	require.Error(t, err, "write must fail when the durable backend fails")
	assert.True(t, errors.Is(err, ErrAllBackendsUnavailable),
		"error must map to 503 ServiceUnavailable in the API layer, got: %v", err)
	assert.Empty(t, backend)

	// The whole point: nothing may be stranded on the hub's local disk.
	assert.Empty(t, filesUnder(t, dataDir), "no bytes may silently land on local disk")

	// And the object must not be readable back.
	_, getErr := eng.Get(ctx, "bucket", "obj.txt")
	require.Error(t, getErr)

	// The failure is counted so Stage 2.3 monitoring can alert on it.
	metrics, err := eng.GetMetrics(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, metrics["write_failures"])
}

// STORAGE_MODE=local (dev, CI, tests): local is the configured primary and
// must keep accepting writes.
func TestEnginePut_LocalPrimaryStillAcceptsWrites(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, &Config{DefaultBackend: "local"})

	dataDir := t.TempDir()
	eng.AddDriver("local", &fileWritingDriver{dir: dataDir})

	backend, err := eng.Put(context.Background(), "bucket", "obj.txt", strings.NewReader("dev data"))
	require.NoError(t, err)
	assert.Equal(t, "local", backend)
	assert.Len(t, filesUnder(t, dataDir), 1)
}

// An explicit REDUCED_REDUNDANCY storage class targets local on purpose —
// that is a requested write, not a silent fallback.
func TestEnginePut_ExplicitLocalTargetAllowed(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, &Config{DefaultBackend: "idrive"})

	dataDir := t.TempDir()
	eng.AddDriver("idrive", &mockDriver{name: "idrive"})
	eng.AddDriver("local", &fileWritingDriver{dir: dataDir})

	backend, err := eng.Put(context.Background(), "bucket", "obj.txt",
		strings.NewReader("data"), WithStorageClass("REDUCED_REDUNDANCY"))
	require.NoError(t, err)
	assert.Equal(t, "local", backend)
	assert.Len(t, filesUnder(t, dataDir), 1)
}

// Failover between durable backends is still allowed — only the silent hop to
// local disk is banned.
func TestEnginePut_DurableToDurableFailoverPreserved(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, &Config{DefaultBackend: "idrive"})

	dataDir := t.TempDir()
	eng.AddDriver("idrive", &mockDriver{name: "idrive", putErr: fmt.Errorf("dial tcp: connection refused")})
	eng.AddDriver("s3", &mockDriver{name: "s3"})
	eng.AddDriver("local", &fileWritingDriver{dir: dataDir})

	backend, err := eng.Put(context.Background(), "bucket", "obj.txt", strings.NewReader("data"))
	require.NoError(t, err)
	assert.Equal(t, "s3", backend)
	assert.Empty(t, filesUnder(t, dataDir))
}

// Client-level outcomes (quota exceeded) keep their error identity so the API
// layer still maps them to 403 — they must NOT be rebranded as 503.
func TestEnginePut_ClientErrorsKeepIdentity(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, &Config{DefaultBackend: "idrive"})
	eng.AddDriver("idrive", &mockDriver{name: "idrive", putErr: ErrQuotaExceeded})

	_, err := eng.Put(context.Background(), "bucket", "obj.txt", strings.NewReader("data"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrQuotaExceeded))
	assert.False(t, errors.Is(err, ErrAllBackendsUnavailable),
		"quota exhaustion must not be reported as backend unavailability")
}

func TestBuildWriteCandidateList(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, &Config{DefaultBackend: "idrive"})
	eng.AddDriver("idrive", &mockDriver{name: "idrive"})
	eng.AddDriver("s3", &mockDriver{name: "s3"})
	eng.AddDriver("local", &fileWritingDriver{dir: t.TempDir()})

	t.Run("local excluded when durable primary", func(t *testing.T) {
		candidates := eng.buildWriteCandidateList("idrive")
		assert.NotContains(t, candidates, "local")
		assert.Contains(t, candidates, "idrive")
		assert.Contains(t, candidates, "s3")
	})

	t.Run("local included when explicitly targeted", func(t *testing.T) {
		candidates := eng.buildWriteCandidateList("local")
		assert.Equal(t, "local", candidates[0])
	})

	t.Run("local included when it is the primary", func(t *testing.T) {
		localPrimary := NewEngine(nil, logger, &Config{DefaultBackend: "local"})
		localPrimary.AddDriver("local", &fileWritingDriver{dir: t.TempDir()})
		localPrimary.AddDriver("idrive", &mockDriver{name: "idrive"})
		candidates := localPrimary.buildWriteCandidateList("idrive")
		assert.Contains(t, candidates, "local")
	})
}
