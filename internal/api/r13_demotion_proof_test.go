package api

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// R13 live proof of one demotion through the real job code path (the
// drivers are LocalDrivers registered as idrive/geyser — a local build has
// no idrive/geyser drivers, so this is the closest a laptop gets). Prints
// the head row, the ledger row and both floor ledgers before/after. Run
// with R13_PROOF=1 -v; skipped otherwise.
func TestR13_DemotionLedgerProof(t *testing.T) {
	if os.Getenv("R13_PROOF") == "" {
		t.Skip("set R13_PROOF=1 to run the demotion proof")
	}
	f := setupDemotionFixture(t, 10*tb, "standard")
	f.runner.IdleAfter = 24 * time.Hour // the 1-day idle knob
	f.runner.MinAge = 0
	_, err := f.db.Exec(`INSERT INTO tenant_floor_quotas (tenant_id, floor, storage_limit_bytes, storage_used_bytes) VALUES ($1,'standard',$2,4096),($1,'vault',$3,0)`, f.tenantID, 8*tb, 2*tb)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id=$1`, f.tenantID) })
	_, err = f.db.Exec(`UPDATE tenant_quotas SET storage_used_bytes = 4096 WHERE tenant_id=$1`, f.tenantID)
	require.NoError(t, err)
	f.object("photos", "holiday-2025.jpg", 4096, 30, 2) // 30 d old, idle 2 d

	dump := func(label string) {
		fmt.Printf("\n== %s ==\n", label)
		var b, k, e, backend, floor string
		var size int64
		var la time.Time
		require.NoError(t, f.db.QueryRow(`SELECT bucket, object_key, etag, COALESCE(backend_name,''), floor, size_bytes, last_accessed FROM object_head_cache WHERE tenant_id=$1`, f.tenantID).Scan(&b, &k, &e, &backend, &floor, &size, &la))
		fmt.Printf("object_head_cache: %s/%s etag=%s backend_name=%s floor=%s size=%d last_accessed=%s\n", b, k, e, backend, floor, size, la.Format(time.RFC3339))
		rows, err := f.db.Query(`SELECT reason, hot_backend, cold_backend, demoted_at, hot_deleted_at IS NOT NULL, hot_outcome FROM smart_demotions WHERE tenant_id=$1`, f.tenantID)
		require.NoError(t, err)
		n := 0
		for rows.Next() {
			var reason, hot, cold, outcome string
			var at time.Time
			var deleted bool
			require.NoError(t, rows.Scan(&reason, &hot, &cold, &at, &deleted, &outcome))
			fmt.Printf("smart_demotions:   reason=%s %s→%s demoted_at=%s hot_deleted=%v outcome=%q\n", reason, hot, cold, at.Format(time.RFC3339), deleted, outcome)
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate rows: %v", err)
		}
		_ = rows.Close()
		if n == 0 {
			fmt.Println("smart_demotions:   (no row)")
		}
		frows, err := f.db.Query(`SELECT floor, storage_limit_bytes, storage_used_bytes FROM tenant_floor_quotas WHERE tenant_id=$1 ORDER BY floor`, f.tenantID)
		require.NoError(t, err)
		for frows.Next() {
			var fl string
			var lim, used int64
			require.NoError(t, frows.Scan(&fl, &lim, &used))
			fmt.Printf("tenant_floor_quotas: floor=%s limit=%d used=%d\n", fl, lim, used)
		}
		if err := frows.Err(); err != nil {
			t.Fatalf("iterate rows: %v", err)
		}
		_ = frows.Close()
		var used int64
		require.NoError(t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id=$1`, f.tenantID).Scan(&used))
		fmt.Printf("tenant_quotas:     used=%d  hot blob=%v cold blob=%v\n", used, f.hotExists("photos", "holiday-2025.jpg"), f.coldExists("photos", "holiday-2025.jpg"))
	}
	dump("BEFORE (flag on for this tenant, idle knob 1 day)")
	res, err := f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)
	fmt.Printf("\nRunOnce: %+v\n", res)
	dump("AFTER demotion")
	f.runner.now = func() time.Time { return f.now.Add(25 * time.Hour) }
	res, err = f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)
	fmt.Printf("\nRunOnce +25h: %+v\n", res)
	dump("AFTER reclaim (grace expired)")
}
