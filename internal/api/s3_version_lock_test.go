package api

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Prompt 2b Part 0 (0.3, 0.5) — the version row of a key is written under
// one transaction-scoped advisory lock on the key, on ONE connection.
// "Before" numbers are the same scenarios run against 0b376cf.

func onePool(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestReassertVersionRow_NeedsOneConnection(t *testing.T) {
	// Arrange: a versioned bucket whose version row was lost; a pool of one
	// connection. Before: the transaction held the connection and the
	// versioning status was read through the pool — a 3 s stall (the whole
	// budget), then "disabled", and no version row was written.
	f := longOpFixture(t, 0)
	_, err := f.db.Exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = 'test-bucket'`, f.tenantID)
	require.NoError(t, err)
	f.complete(t, "one.bin")
	_, err = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1 AND object_key = 'one.bin'`, f.tenantID)
	require.NoError(t, err)
	f.server.db = onePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Act
	start := time.Now()
	err = f.server.reassertVersionRow(ctx, f.tenantID, "test-bucket", "one.bin")
	took := time.Since(start)

	// Assert
	require.NoError(t, err)
	assert.Less(t, took, time.Second)
	assert.Equal(t, 1, f.latestVersions(t, "one.bin"))
}

// holdVersionWrites holds every version write between its "previous latest
// → not latest" update and its insert until two writers got there (or
// 300 ms passed: under the key lock the second waits for the first's commit).
func holdVersionWrites(t *testing.T) {
	var mu sync.Mutex
	arrived := 0
	versionRowUpdatedHook = func() {
		mu.Lock()
		arrived++
		mu.Unlock()
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := arrived
			mu.Unlock()
			if n >= 2 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Cleanup(func() { versionRowUpdatedHook = func() {} })
}

func TestRecordObjectVersion_ConcurrentWritesLeaveOneLatestVersion(t *testing.T) {
	// Arrange: two writes of the same key on a versioned bucket (two PUTs, a
	// copy and a PUT, a Complete retry racing a write), held between their
	// two statements. Before: each cleared the latest flag, then each
	// inserted a latest row — 2 is_latest rows.
	f := longOpFixture(t, 0)
	_, err := f.db.Exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = 'test-bucket'`, f.tenantID)
	require.NoError(t, err)
	holdVersionWrites(t)

	// Act
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recordObjectVersion(context.Background(), f.db, f.tenantID, "test-bucket", "two.bin", 3, "etag", "text/plain", "local")
		}()
	}
	wg.Wait()

	// Assert
	assert.Equal(t, 1, f.latestVersions(t, "two.bin"))
}

func TestDeleteMarker_RacingAWriteLeavesOneLatestVersion(t *testing.T) {
	// Before: the marker path cleared the latest flag and inserted the
	// marker as two plain statements, outside any lock — racing a write, 2
	// latest rows.
	f := longOpFixture(t, 0)
	_, err := f.db.Exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = 'test-bucket'`, f.tenantID)
	require.NoError(t, err)
	holdVersionWrites(t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		recordObjectVersion(context.Background(), f.db, f.tenantID, "test-bucket", "m.bin", 3, "etag", "text/plain", "local")
	}()
	go func() {
		defer wg.Done()
		assert.NoError(t, writeDeleteMarker(context.Background(), f.db, f.tenantID, "test-bucket", "m.bin", generateVersionID()))
	}()
	wg.Wait()

	assert.Equal(t, 1, f.latestVersions(t, "m.bin"))
}
