package engine

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestLocationStore_RecordAndLookup(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	store := NewLocationStore(db, zap.NewNop())

	mock.ExpectExec("INSERT INTO object_locations").
		WithArgs("tenant1", "bucket1", "key1", "idrive", "STANDARD", int64(1024)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = store.RecordLocation(context.Background(), "tenant1", "bucket1", "key1", "idrive", "STANDARD", 1024)
	require.NoError(t, err)

	rows := sqlmock.NewRows([]string{"backend_name"}).AddRow("idrive")
	mock.ExpectQuery("SELECT backend_name FROM object_locations").
		WithArgs("tenant1", "bucket1", "key1").
		WillReturnRows(rows)

	backend, err := store.LookupBackend(context.Background(), "tenant1", "bucket1", "key1")
	require.NoError(t, err)
	assert.Equal(t, "idrive", backend)
}

func TestLocationStore_LookupMiss(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	store := NewLocationStore(db, zap.NewNop())

	mock.ExpectQuery("SELECT backend_name FROM object_locations").
		WithArgs("tenant1", "bucket1", "missing").
		WillReturnRows(sqlmock.NewRows([]string{"backend_name"}))

	backend, err := store.LookupBackend(context.Background(), "tenant1", "bucket1", "missing")
	require.NoError(t, err)
	assert.Equal(t, "", backend)
}

func TestLocationStore_Remove(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	store := NewLocationStore(db, zap.NewNop())

	mock.ExpectExec("DELETE FROM object_locations").
		WithArgs("tenant1", "bucket1", "key1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = store.RemoveLocation(context.Background(), "tenant1", "bucket1", "key1")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLocationStore_Upsert(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	store := NewLocationStore(db, zap.NewNop())

	mock.ExpectExec("INSERT INTO object_locations").
		WithArgs("tenant1", "bucket1", "key1", "idrive", "STANDARD", int64(100)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO object_locations").
		WithArgs("tenant1", "bucket1", "key1", "geyser", "GLACIER", int64(200)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = store.RecordLocation(context.Background(), "tenant1", "bucket1", "key1", "idrive", "STANDARD", 100)
	require.NoError(t, err)
	err = store.RecordLocation(context.Background(), "tenant1", "bucket1", "key1", "geyser", "GLACIER", 200)
	require.NoError(t, err)

	rows := sqlmock.NewRows([]string{"backend_name"}).AddRow("geyser")
	mock.ExpectQuery("SELECT backend_name FROM object_locations").
		WithArgs("tenant1", "bucket1", "key1").
		WillReturnRows(rows)

	backend, err := store.LookupBackend(context.Background(), "tenant1", "bucket1", "key1")
	require.NoError(t, err)
	assert.Equal(t, "geyser", backend)
}

func TestLocationStore_CountByBackend(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	store := NewLocationStore(db, zap.NewNop())

	rows := sqlmock.NewRows([]string{"backend_name", "count"}).
		AddRow("idrive", int64(10)).
		AddRow("geyser", int64(5))
	mock.ExpectQuery("SELECT backend_name, COUNT").
		WillReturnRows(rows)

	counts, err := store.CountByBackend(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(10), counts["idrive"])
	assert.Equal(t, int64(5), counts["geyser"])
	assert.Len(t, counts, 2)
}

func TestLocationStore_NilDB(t *testing.T) {
	store := NewLocationStore(nil, zap.NewNop())

	err := store.RecordLocation(context.Background(), "t", "b", "k", "idrive", "STANDARD", 100)
	assert.NoError(t, err)

	backend, err := store.LookupBackend(context.Background(), "t", "b", "k")
	assert.NoError(t, err)
	assert.Equal(t, "", backend)

	err = store.RemoveLocation(context.Background(), "t", "b", "k")
	assert.NoError(t, err)

	counts, err := store.CountByBackend(context.Background())
	assert.NoError(t, err)
	assert.Empty(t, counts)

	err = store.TouchLastAccessed(context.Background(), "t", "b", "k")
	assert.NoError(t, err)
}

func TestEngine_PutRecordsLocation(t *testing.T) {
	logger := zap.NewNop()
	eng := NewEngine(nil, logger, nil)

	drv := &stubDriver{name: "primary", data: make(map[string][]byte), healthy: true}
	eng.AddDriver("primary", drv)
	eng.SetPrimary("primary")

	backend, err := eng.Put(context.Background(), "bucket", "key", strings.NewReader("data"))
	require.NoError(t, err)
	assert.Equal(t, "primary", backend)

	v, ok := eng.objectBackends.Load(objectKey("bucket", "key"))
	assert.True(t, ok)
	assert.Equal(t, "primary", v.(string))
}

func TestEngine_GetFallsBackToLocationStore(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	logger := zap.NewNop()
	eng := NewEngine(nil, logger, nil)
	eng.locations = NewLocationStore(db, logger)

	drv := &stubDriver{name: "geyser", data: make(map[string][]byte), healthy: true}
	drv.data["mybucket/myobj"] = []byte("archived-data")
	eng.AddDriver("geyser", drv)

	localDrv := &stubDriver{name: "local", data: make(map[string][]byte), healthy: true}
	eng.AddDriver("local", localDrv)
	eng.SetPrimary("local")

	rows := sqlmock.NewRows([]string{"backend_name"}).AddRow("geyser")
	mock.ExpectQuery("SELECT backend_name FROM object_locations").
		WithArgs("default", "mybucket", "myobj").
		WillReturnRows(rows)

	reader, err := eng.Get(context.Background(), "mybucket", "myobj")
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	b, _ := io.ReadAll(reader)
	assert.Equal(t, "archived-data", string(b))

	v, ok := eng.objectBackends.Load(objectKey("mybucket", "myobj"))
	assert.True(t, ok, "should seed sync.Map after DB lookup")
	assert.Equal(t, "geyser", v.(string))
}
