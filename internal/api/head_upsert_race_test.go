package api

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// R15-17: two writers creating the SAME NEW key at once. atomicHeadUpsert
// captures the displaced row with SELECT … FOR UPDATE — but a row that does
// not exist yet cannot be locked, so both writers saw "no previous row",
// both reported displaced.Size = 0, and the first writer's reserved bytes
// were never released (settlePutQuota releases old.Size). The quota ledger
// then over-counts by one object's size until an admin reconciles.
//
// The barrier makes both writers sit between the probe and the insert; with
// the per-key advisory lock in place the second writer cannot reach the
// callback until the first commits (the barrier times out for the first
// writer, which is the point), so exactly one of them sees the other's size.
func TestAtomicHeadUpsert_ConcurrentFirstWritersAccountOnce(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — run `make test-db`", err)
	}
	tenantID := "upsert-race-" + uuid.NewString()[:8]
	const bucket, key = "b", "new-key.bin"
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, tenantID) })

	sizes := []int64{1000, 2000}
	displaced := make([]displacedRow, 2)
	errs := make([]error, 2)
	var arrived sync.WaitGroup
	arrived.Add(2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			displaced[i], errs[i] = atomicHeadUpsertReleasing(context.Background(), db, nil, tenantID, bucket, key,
				func(tx *sql.Tx) error {
					// Both writers are past the FOR UPDATE probe here (or the
					// other one is blocked by the fix): wait for the peer,
					// but never longer than the fix makes necessary.
					arrived.Done()
					waitOrTimeout(&arrived, 300*time.Millisecond)
					_, err := tx.Exec(`
						INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, floor)
						VALUES ($1, $2, $3, $4, 'etag', 'application/octet-stream', 'local', 'standard')
						ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET size_bytes = EXCLUDED.size_bytes`,
						tenantID, bucket, key, sizes[i])
					return err
				})
		}(i)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	var finalSize int64
	require.NoError(t, db.QueryRow(`SELECT size_bytes FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key).Scan(&finalSize))
	released := displaced[0].Size + displaced[1].Size
	assert.Equal(t, sizes[0]+sizes[1]-finalSize, released,
		"exactly the overwritten writer's bytes must be reported as displaced (reported %d and %d, final row %d)",
		displaced[0].Size, displaced[1].Size, finalSize)
}

// waitOrTimeout waits for wg or gives up after d.
func waitOrTimeout(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}
