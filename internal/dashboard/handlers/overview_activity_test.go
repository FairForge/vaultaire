package handlers

// WP-R10-8 Part D (R10-25): the overview's "Recent Activity" listed the
// keyless 'RESERVE' rows of quota_usage_events. It lists the tenant's most
// recent writes from the head rows, never the system bucket.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOverviewActivity_RecentWritesFromTheHeadRows(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v)", err)
	}
	tid := fmt.Sprintf("activity-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, tid) })
	for i, k := range []string{"old.bin", "new.bin"} {
		_, err := db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, updated_at)
			VALUES ($1, 'photos', $2, $3, 'e', 'x', NOW() - make_interval(mins => $4))`, tid, k, 1000*(i+1), 10-i)
		require.NoError(t, err)
	}
	_, err = db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, $2, 'export.json', 5, 'e', 'x')`, tid, tenant.ExportsBucket)
	require.NoError(t, err)

	data := map[string]any{}
	populateActivity(context.Background(), db, tid, data)

	rows, ok := data["Activity"].([]ActivityRow)
	require.True(t, ok)
	require.Len(t, rows, 2, "the system bucket is not shown")
	assert.Equal(t, "photos/new.bin", rows[0].ObjectKey, "newest first")
	assert.Equal(t, "Upload", rows[0].Operation)
}
