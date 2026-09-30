package api

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// R2-15 (Review R15): two concurrent PUTs of one key. The backend's last
// writer and the head row's last writer may still differ until WP-R2-1
// (write-new-key-then-swap) — that part is documented, not asserted here.
// What R2 proved by walking the arithmetic and what this pins: the quota
// ledger stays EXACT under the race (atomicHeadUpsert serialises the rows
// with FOR UPDATE and the displaced size is released once), one head row
// survives, and it describes one of the bodies that was actually sent.
func TestConcurrentSameKeyPut_QuotaStaysExact(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 64<<20)

	// Several rounds on a NEW key each time: the race that matters is the
	// first write — two writers that both find no head row under FOR UPDATE
	// (nothing to lock yet) and both capture "displaced = 0", so one body's
	// bytes were reserved and never released (R15-17).
	const writers, rounds = 16, 6
	for round := 0; round < rounds; round++ {
		key := fmt.Sprintf("race-%d.bin", round)
		bodies := make([][]byte, writers)
		for i := range bodies {
			bodies[i] = testBytes(1024 * (i + 1)) // 1 KiB … 16 KiB, all distinct sizes
		}

		var wg sync.WaitGroup
		codes := make([]int, writers)
		start := make(chan struct{})
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				codes[i] = f.put(t, key, bodies[i])
			}(i)
		}
		close(start)
		wg.Wait()

		for i, c := range codes {
			assert.Equal(t, 200, c, "round %d writer %d", round, i)
		}

		var rowCount int
		var size int64
		require.NoError(t, f.db.QueryRow(
			`SELECT COUNT(*), COALESCE(MAX(size_bytes), 0) FROM object_head_cache WHERE tenant_id = $1 AND object_key = $2`,
			f.tenantID, key).Scan(&rowCount, &size))
		assert.Equal(t, 1, rowCount, "round %d: exactly one head row survives", round)
		assert.True(t, size >= 1024 && size <= 16*1024 && size%1024 == 0, "round %d: head row size %d must be one of the bodies", round, size)

		var sum int64
		require.NoError(t, f.db.QueryRow(
			`SELECT COALESCE(SUM(size_bytes), 0) FROM object_head_cache WHERE tenant_id = $1`, f.tenantID).Scan(&sum))
		assert.Equal(t, sum, f.used(t), "round %d: tenant_quotas.storage_used_bytes == SUM(size_bytes) after the race", round)

		require.Equal(t, 204, f.del(t, key))
		assert.Equal(t, int64(0), f.used(t), "round %d: delete releases exactly what was billed", round)
	}
}
