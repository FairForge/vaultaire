package engine

import (
	"bytes"
	"context"
	"io"
	"testing"

	"go.uber.org/zap"
)

func TestCoreEngine_ImplementsInterface(t *testing.T) {
	// This test ensures CoreEngine implements Engine interface
	var _ Engine = (*CoreEngine)(nil)

	// Test construction
	logger := zap.NewNop()
	engine := NewEngine(nil, logger, nil)

	if engine == nil {
		t.Fatal("NewEngine returned nil")
	}

	// Test health check
	err := engine.HealthCheck(context.Background())
	if err != nil {
		t.Errorf("HealthCheck failed: %v", err)
	}
}

type stubDriver struct {
	name    string
	data    map[string][]byte
	healthy bool
}

func (d *stubDriver) Name() string { return d.name }
func (d *stubDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	key := container + "/" + artifact
	if b, ok := d.data[key]; ok {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	return nil, &NotFoundError{Container: container, Artifact: artifact}
}
func (d *stubDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) error {
	key := container + "/" + artifact
	b, _ := io.ReadAll(data)
	d.data[key] = b
	return nil
}
func (d *stubDriver) Delete(ctx context.Context, container, artifact string) error { return nil }
func (d *stubDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	return nil, nil
}
func (d *stubDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	return false, nil
}
func (d *stubDriver) HealthCheck(ctx context.Context) error {
	if !d.healthy {
		return context.DeadlineExceeded
	}
	return nil
}

func TestGetDriver(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, nil)

	drv := &stubDriver{name: "test-driver", data: make(map[string][]byte), healthy: true}
	eng.AddDriver("idrive-eu-west-1", drv)

	got, ok := eng.GetDriver("idrive-eu-west-1")
	if !ok {
		t.Fatal("GetDriver should find registered driver")
	}
	if got.Name() != "test-driver" {
		t.Errorf("expected Name() = %q, got %q", "test-driver", got.Name())
	}

	_, ok = eng.GetDriver("nonexistent")
	if ok {
		t.Error("GetDriver should return false for unregistered driver")
	}
}

func TestHintBackend(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, nil)

	drv := &stubDriver{name: "region-drv", data: make(map[string][]byte), healthy: true}
	drv.data["bucket/key"] = []byte("hello")
	eng.AddDriver("idrive-eu-west-1", drv)

	eng.HintBackend("bucket", "key", "idrive-eu-west-1")

	reader, err := eng.Get(context.Background(), "bucket", "key")
	if err != nil {
		t.Fatalf("Get after HintBackend failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	b, _ := io.ReadAll(reader)
	if string(b) != "hello" {
		t.Errorf("expected %q, got %q", "hello", string(b))
	}
}
