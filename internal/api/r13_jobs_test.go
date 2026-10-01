package api

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Review R13-03: the hourly rollup must include yesterday, or the interval
// between the last rollup of a day and midnight is never counted.
func TestCDNRollup_IncludesYesterday(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	tenantID := "cdnroll-" + uuid.New().String()[:8]
	_, err = db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1,$2,$3,$4,$5)`,
		tenantID, "rollup", tenantID+"@test.local", "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM cdn_stats_daily WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM cdn_access_log WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	// One hit at 23:30 yesterday (the tail no rollup of yesterday could
	// have seen if the last one ran at 23:00), one today, one 3 days ago.
	for _, at := range []string{
		"CURRENT_DATE - INTERVAL '30 minutes'",
		"NOW()",
		"CURRENT_DATE - INTERVAL '3 days'",
	} {
		_, err = db.Exec(`INSERT INTO cdn_access_log (tenant_id, bucket, object_key, bytes_sent, country, referer, accessed_at)
			VALUES ($1, 'pub', 'k', 100, 'US', '', `+at+`)`, tenantID)
		require.NoError(t, err)
	}

	ct := NewCDNAnalyticsTracker(db)
	ct.SetLogger(zap.NewNop())
	ct.runRollup()

	rows, err := db.Query(`SELECT date, requests, bytes_sent FROM cdn_stats_daily WHERE tenant_id = $1 ORDER BY date`, tenantID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var dates []string
	for rows.Next() {
		var d time.Time
		var req, bytes int64
		require.NoError(t, rows.Scan(&d, &req, &bytes))
		dates = append(dates, d.Format("2006-01-02"))
		assert.Equal(t, int64(1), req)
		assert.Equal(t, int64(100), bytes)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rows: %v", err)
	}
	today := time.Now().UTC()
	// The DB's CURRENT_DATE is its session timezone's date; the test DB is
	// local. Compare against the DB's own idea of today/yesterday.
	var dbToday, dbYesterday string
	require.NoError(t, db.QueryRow(`SELECT CURRENT_DATE::text, (CURRENT_DATE - 1)::text`).Scan(&dbToday, &dbYesterday))
	_ = today
	assert.Equal(t, []string{dbYesterday, dbToday}, dates, "yesterday's tail is rolled up, three days ago is not touched")
}

// Review R13-09: a failed alert e-mail is retried on the next pass instead
// of being stamped as fired for the month.
// Review R13-09 (a failed send must not stamp last_fired_at) is pinned by
// TestEgressAlerts_FailedSendIsRetriedNextPass in bandwidth_alerts_test.go
// since WP-R10-9 moved the alerter onto the shared egress allowance.

// Review R13-20: reclaim drains the backlog in batches instead of stopping
// at one LIMIT per run.
func TestSmartDemotion_ReclaimDrainsBacklogBeyondOneBatch(t *testing.T) {
	f := setupDemotionFixture(t, 10*tb, "standard")
	f.runner.MaxObjectsPerTenant = 2
	require.NoError(t, os.MkdirAll(filepath.Join(f.hotDir, f.tenantID+"_b"), 0o755))
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("old%d", i)
		h := f.object("b", key, 100, 30, 20, onBackend("geyser"))
		require.NoError(t, os.WriteFile(filepath.Join(f.hotDir, f.tenantID+"_b", key), []byte("hot"), 0o600))
		_, err := f.db.Exec(`INSERT INTO smart_demotions (tenant_id,bucket,object_key,etag,size_bytes,hot_backend,cold_backend,reason,demoted_at)
			VALUES ($1,'b',$2,$3,100,'idrive','geyser','idle',$4)`, f.tenantID, key, h.etag, f.now.Add(-48*time.Hour))
		require.NoError(t, err)
	}

	res, err := f.runner.RunOnce(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, 5, res.HotReclaimed, "%+v", res)
	for i := 0; i < 5; i++ {
		assert.False(t, f.hotExists("b", fmt.Sprintf("old%d", i)))
	}
}

func TestEmailHash_NeverContainsTheAddress(t *testing.T) {
	h := emailHash("Someone@Example.com ")
	assert.Len(t, h, 8)
	assert.Equal(t, h, emailHash("someone@example.com"))
	assert.NotContains(t, h, "example")
}
