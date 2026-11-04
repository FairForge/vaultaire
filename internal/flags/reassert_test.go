package flags

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// RP96-10: a same-value re-assert of a flag (the dashboard's toggle clicked
// on an ON row) rewrote updated_at, and the bucket page's verified line
// reads updated_at as "on since" — it hid the line until the next cycle. The
// row is written only when enabled changes; the audit row as before.
func TestFlags_ReassertingAFlagKeepsItsSince(t *testing.T) {
	db := openTestDB(t)
	key := testKey(t, db)
	svc := New(db, zap.NewNop())
	svc.Register(key, false)
	ctx := context.Background()
	tenant := "tenant-reassert-" + key
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM audit_logs WHERE resource = $1`, "flag:"+key) })

	require.NoError(t, svc.Set(ctx, key, tenant, true, "first"))
	_, err := db.Exec(`UPDATE feature_flags SET updated_at = now() - interval '3 days' WHERE flag_key = $1 AND tenant_id = $2`, key, tenant)
	require.NoError(t, err)
	since := func() (time.Time, string) {
		var at time.Time
		var by string
		require.NoError(t, db.QueryRow(`SELECT updated_at, updated_by FROM feature_flags WHERE flag_key = $1 AND tenant_id = $2`, key, tenant).Scan(&at, &by))
		return at, by
	}
	before, _ := since()

	require.NoError(t, svc.Set(ctx, key, tenant, true, "second"))
	after, by := since()
	assert.True(t, after.Equal(before), "a same-value re-assert keeps updated_at (%v → %v)", before, after)
	assert.Equal(t, "first", by, "and who turned it on")
	var audits int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM audit_logs WHERE action = 'flag.set' AND resource = $1`, "flag:"+key).Scan(&audits))
	assert.Equal(t, 2, audits, "both clicks are in the trail")

	require.NoError(t, svc.Set(ctx, key, tenant, false, "third"))
	off, by := since()
	assert.True(t, off.After(before.Add(time.Hour)), "a change writes updated_at")
	assert.Equal(t, "third", by)
	assert.False(t, svc.Enabled(key, tenant))
}
