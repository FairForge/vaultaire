package handlers

import (
	"context"
	"database/sql"
	"io"
	"testing"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R13-1: the object browser shows the attic column, the restore status and
// the Restore button for ATTIC objects only. A downstairs object the Smart
// tier parked on the cold backend is not "in the attic" — it downloads like
// any other object and reading it brings it back.

// fakeArchive is a cold backend with a restore concept.
type fakeArchive struct{ restores int }

func (d *fakeArchive) Name() string { return "geyser" }
func (d *fakeArchive) Get(context.Context, string, string) (io.ReadCloser, error) {
	return nil, engine.ErrArchived
}
func (d *fakeArchive) Put(context.Context, string, string, io.Reader, ...engine.PutOption) error {
	return nil
}
func (d *fakeArchive) Delete(context.Context, string, string) error           { return nil }
func (d *fakeArchive) List(context.Context, string, string) ([]string, error) { return nil, nil }
func (d *fakeArchive) Exists(context.Context, string, string) (bool, error)   { return true, nil }
func (d *fakeArchive) HealthCheck(context.Context) error                      { return nil }
func (d *fakeArchive) RestoreObject(context.Context, string, string, int32) error {
	d.restores++
	return nil
}
func (d *fakeArchive) RestoreStatus(context.Context, string, string) (*engine.RestoreStatus, error) {
	return &engine.RestoreStatus{}, nil
}

func classFixtureRows(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("no test database: %v", err)
	}
	tenantID := "tenant-" + uuid.New().String()[:8]
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, tenantID) })
	_, err = db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor)
		VALUES ($1, 'b', 'attic.zip', 9, 'e1', 'geyser', 'vault'),
		       ($1, 'b', 'demoted.jpg', 9, 'e2', 'geyser', 'standard'),
		       ($1, 'b', 'hot.txt', 9, 'e3', 'idrive', 'standard')`, tenantID)
	require.NoError(t, err)
	return db, tenantID
}

func TestBucketObjects_OnlyAtticObjectsAreArchived(t *testing.T) {
	// Arrange
	db, tenantID := classFixtureRows(t)
	data := map[string]any{}

	// Act
	populateBucketObjects(context.Background(), db, tenantID, "b", "", "", data)

	// Assert
	objects, _ := data["Objects"].([]ObjectRow)
	require.Len(t, objects, 3)
	archived := map[string]bool{}
	for _, o := range objects {
		archived[o.Key] = o.IsArchived
	}
	assert.Equal(t, map[string]bool{"attic.zip": true, "demoted.jpg": false, "hot.txt": false}, archived)
	assert.Equal(t, true, data["HasArchived"])
}

func TestObjectRestorerFor_DemotedDownstairsObjectHasNoRestore(t *testing.T) {
	// Arrange
	db, tenantID := classFixtureRows(t)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("geyser", &fakeArchive{})

	// Act
	attic, err := objectRestorerFor(context.Background(), eng, db, tenantID, "b", "attic.zip")
	require.NoError(t, err)
	demoted, err := objectRestorerFor(context.Background(), eng, db, tenantID, "b", "demoted.jpg")
	require.NoError(t, err)

	// Assert
	assert.NotNil(t, attic, "an attic object can be restored")
	assert.Nil(t, demoted, "a downstairs object is never restored by hand: the Restore button and the status probe do nothing for it")
}
