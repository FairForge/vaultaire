package account

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stage 1a Prompt 7, Part 0 (the WP-R10-8 lock-order audit): EraseRows
// deleted the tenant's floor rows before its tenant_quotas row. A write
// committing during the erasure (a PUT that landed after the walk) holds the
// total and then updates its floor: the two waited on each other until
// Postgres aborted one. The erase now locks the total first.
func TestEraseRows_LocksTheTotalBeforeTheFloors(t *testing.T) {
	f := setupEraseFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	ctx := context.Background()

	writer, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Rollback() })
	_, err = writer.ExecContext(ctx, `UPDATE tenant_quotas SET storage_used_bytes = storage_used_bytes + 1 WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)

	eraseErr := make(chan error, 1)
	go func() {
		_, err := f.svc.EraseRows(ctx, f.userID, f.tenantID, f.email, time.Now())
		eraseErr <- err
	}()
	waited := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !waited; time.Sleep(5 * time.Millisecond) {
		var n int
		_ = f.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%tenant_quotas%'`).Scan(&n)
		waited = n > 0
	}
	require.True(t, waited, "the erase waits for the writer's total")

	_, wErr := writer.ExecContext(ctx, `UPDATE tenant_floor_quotas SET storage_used_bytes = storage_used_bytes + 1 WHERE tenant_id = $1 AND floor = 'standard'`, f.tenantID)
	if wErr == nil {
		wErr = writer.Commit()
	}

	assert.NoError(t, wErr, "the writer's commit")
	select {
	case err := <-eraseErr:
		assert.NoError(t, err, "the erase")
	case <-time.After(15 * time.Second):
		t.Fatal("the erase never finished")
	}
}
