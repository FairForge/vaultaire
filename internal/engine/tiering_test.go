package engine

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestTieringEngine_NilDB(t *testing.T) {
	logger := zap.NewNop()
	te := NewTieringEngine(nil, nil, nil, &sync.Map{}, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		te.Start(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Start with nil DB should return immediately")
	}
}

func TestTieringEngine_MigrateObject(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	logger := zap.NewNop()
	locations := NewLocationStore(db, logger)

	src := &stubDriver{name: "idrive", data: map[string][]byte{
		"mybucket/photo.jpg": []byte("image-data"),
	}, healthy: true}
	dst := &stubDriver{name: "geyser", data: make(map[string][]byte), healthy: true}

	drivers := map[string]Driver{"idrive": src, "geyser": dst}
	var backends sync.Map

	te := NewTieringEngine(db, drivers, locations, &backends, logger)

	mock.ExpectExec("INSERT INTO object_locations").
		WithArgs("tenant1", "mybucket", "photo.jpg", "geyser", "GLACIER", int64(1024)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = te.MigrateObject(context.Background(), "tenant1", "mybucket", "photo.jpg", "idrive", "geyser", "GLACIER", 1024)
	require.NoError(t, err)

	assert.Equal(t, []byte("image-data"), dst.data["mybucket/photo.jpg"])

	v, ok := backends.Load("mybucket/photo.jpg")
	assert.True(t, ok)
	assert.Equal(t, "geyser", v)

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestTieringEngine_SkipsWhenTargetNotRegistered(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	logger := zap.NewNop()
	locations := NewLocationStore(db, logger)

	src := &stubDriver{name: "idrive", data: map[string][]byte{
		"mybucket/file.txt": []byte("data"),
	}, healthy: true}

	drivers := map[string]Driver{"idrive": src}
	var backends sync.Map

	te := NewTieringEngine(db, drivers, locations, &backends, logger)

	mock.ExpectQuery("SELECT id, tenant_id, bucket, min_age_days, target_backend, target_class").
		WillReturnRows(sqlmock.NewRows([]string{"id", "tenant_id", "bucket", "min_age_days", "target_backend", "target_class"}).
			AddRow(1, nil, nil, 30, "geyser", "GLACIER"))

	te.RunScan(context.Background())

	_, ok := backends.Load("mybucket/file.txt")
	assert.False(t, ok, "object should not have been migrated")

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestTieringEngine_ScanWithPolicy(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	logger := zap.NewNop()
	locations := NewLocationStore(db, logger)

	src := &stubDriver{name: "idrive", data: map[string][]byte{
		"bucket1/old-file.bin": []byte("old-data"),
	}, healthy: true}
	dst := &stubDriver{name: "geyser", data: make(map[string][]byte), healthy: true}
	drivers := map[string]Driver{"idrive": src, "geyser": dst}
	var backends sync.Map

	te := NewTieringEngine(db, drivers, locations, &backends, logger)

	mock.ExpectQuery("SELECT id, tenant_id, bucket, min_age_days, target_backend, target_class").
		WillReturnRows(sqlmock.NewRows([]string{"id", "tenant_id", "bucket", "min_age_days", "target_backend", "target_class"}).
			AddRow(1, nil, nil, 30, "geyser", "GLACIER"))

	mock.ExpectQuery("SELECT tenant_id, bucket, object_key, backend_name, size_bytes").
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "bucket", "object_key", "backend_name", "size_bytes"}).
			AddRow("t1", "bucket1", "old-file.bin", "idrive", 512))

	mock.ExpectExec("INSERT INTO object_locations").
		WithArgs("t1", "bucket1", "old-file.bin", "geyser", "GLACIER", int64(512)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	te.RunScan(context.Background())

	assert.Equal(t, []byte("old-data"), dst.data["bucket1/old-file.bin"])

	v, ok := backends.Load("bucket1/old-file.bin")
	assert.True(t, ok)
	assert.Equal(t, "geyser", v)

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestTieringEngine_StopCancels(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	logger := zap.NewNop()
	locations := NewLocationStore(db, logger)
	drivers := map[string]Driver{}
	var backends sync.Map

	te := NewTieringEngine(db, drivers, locations, &backends, logger)
	te.SetInterval(50 * time.Millisecond)

	mock.ExpectQuery("SELECT id, tenant_id, bucket, min_age_days, target_backend, target_class").
		WillReturnRows(sqlmock.NewRows([]string{"id", "tenant_id", "bucket", "min_age_days", "target_backend", "target_class"}))

	done := make(chan struct{})
	go func() {
		te.Start(context.Background())
		close(done)
	}()

	time.Sleep(80 * time.Millisecond)
	te.Stop()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Start should have returned after Stop")
	}
}

func TestTieringEngine_MigrateObject_SourceNotRegistered(t *testing.T) {
	logger := zap.NewNop()
	drivers := map[string]Driver{}
	var backends sync.Map

	te := NewTieringEngine(nil, drivers, nil, &backends, logger)

	err := te.MigrateObject(context.Background(), "t1", "b", "k", "missing-src", "geyser", "GLACIER", 100)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "source driver")
}

func TestTieringEngine_MigrateObject_TargetNotRegistered(t *testing.T) {
	logger := zap.NewNop()
	src := &stubDriver{name: "idrive", data: map[string][]byte{
		"b/k": []byte("x"),
	}, healthy: true}
	drivers := map[string]Driver{"idrive": src}
	var backends sync.Map

	te := NewTieringEngine(nil, drivers, nil, &backends, logger)

	err := te.MigrateObject(context.Background(), "t1", "b", "k", "idrive", "missing-dst", "GLACIER", 100)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "target driver")
}

// trackingStubDriver records calls for assertions.
type trackingStubDriver struct {
	stubDriver
	getCalls    []string
	putCalls    []string
	deleteCalls []string
}

func (d *trackingStubDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	d.getCalls = append(d.getCalls, container+"/"+artifact)
	return d.stubDriver.Get(ctx, container, artifact)
}

func (d *trackingStubDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) error {
	d.putCalls = append(d.putCalls, container+"/"+artifact)
	return d.stubDriver.Put(ctx, container, artifact, data, opts...)
}

func (d *trackingStubDriver) Delete(ctx context.Context, container, artifact string) error {
	d.deleteCalls = append(d.deleteCalls, container+"/"+artifact)
	return d.stubDriver.Delete(ctx, container, artifact)
}

func TestTieringEngine_MigrateObject_CallOrder(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	logger := zap.NewNop()
	locations := NewLocationStore(db, logger)

	src := &trackingStubDriver{
		stubDriver: stubDriver{name: "idrive", data: map[string][]byte{
			"bkt/obj": []byte("payload"),
		}, healthy: true},
	}
	dst := &trackingStubDriver{
		stubDriver: stubDriver{name: "geyser", data: make(map[string][]byte), healthy: true},
	}

	drivers := map[string]Driver{"idrive": src, "geyser": dst}
	var backends sync.Map

	te := NewTieringEngine(db, drivers, locations, &backends, logger)

	mock.ExpectExec("INSERT INTO object_locations").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = te.MigrateObject(context.Background(), "t1", "bkt", "obj", "idrive", "geyser", "GLACIER", 7)
	require.NoError(t, err)

	assert.Equal(t, []string{"bkt/obj"}, src.getCalls)
	assert.Equal(t, []string{"bkt/obj"}, dst.putCalls)
	assert.Equal(t, []string{"bkt/obj"}, src.deleteCalls)

	assert.Equal(t, []byte("payload"), dst.data["bkt/obj"])
}

func TestTieringEngine_MigrateObject_PutFailsNoDelete(t *testing.T) {
	logger := zap.NewNop()

	src := &trackingStubDriver{
		stubDriver: stubDriver{name: "idrive", data: map[string][]byte{
			"b/k": []byte("data"),
		}, healthy: true},
	}
	dst := &failingPutDriver{name: "geyser"}

	drivers := map[string]Driver{"idrive": src, "geyser": dst}
	var backends sync.Map

	te := NewTieringEngine(nil, drivers, nil, &backends, logger)

	err := te.MigrateObject(context.Background(), "t1", "b", "k", "idrive", "geyser", "GLACIER", 4)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "put to geyser")

	assert.Empty(t, src.deleteCalls, "should NOT delete source when put fails")
}

type failingPutDriver struct {
	name string
}

func (d *failingPutDriver) Name() string { return d.name }
func (d *failingPutDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}
func (d *failingPutDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) error {
	return io.ErrUnexpectedEOF
}
func (d *failingPutDriver) Delete(ctx context.Context, container, artifact string) error {
	return nil
}
func (d *failingPutDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	return nil, nil
}
func (d *failingPutDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	return false, nil
}
func (d *failingPutDriver) HealthCheck(ctx context.Context) error { return nil }
