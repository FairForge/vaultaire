package api

// WP-PLAT-1 PR C: the cascade. The erasure of a platform erases its
// children first — each through the ordinary runner path, objects, keys
// and rows — then the platform's rows; EraseRows refuses a platform while a
// child row remains. A child whose own erasure is due is erased alone.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/account"
)

// seedChild adds a child of the fixture's tenant holding one object on the
// primary and one key; scheduledAt sets its own erasure date (nil = none).
func (f *deletionFixture) seedChild(scheduledAt *time.Time) (id, blob string) {
	f.t.Helper()
	id = "tenant-ch-" + uuid.NewString()[:12]
	b := "child-bkt-" + id[10:16]
	f.exec(`INSERT INTO tenants (id, name, email, parent_tenant_id, label, deletion_scheduled_at) VALUES ($1, 'c', $2, $3, 'c', $4)`,
		id, "platform+"+id+"@platform.invalid", f.tenantID, scheduledAt)
	f.exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes) VALUES ($1, 0, 9)`, id)
	f.exec(`INSERT INTO api_keys (id, user_id, tenant_id, name, key_id, secret_hash) VALUES ($1, $2, $3, 'child', $4, 'h')`,
		uuid.NewString(), f.userID, id, "VKCH"+id[10:22])
	f.exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, $2)`, id, b)
	blob = filepath.Join(f.primary, id+"_"+b, "o.txt")
	require.NoError(f.t, os.MkdirAll(filepath.Dir(blob), 0o750))
	require.NoError(f.t, os.WriteFile(blob, []byte("child bytes"), 0o600))
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name) VALUES ($1, $2, 'o.txt', 9, 'e', 'local')`, id, b)
	f.t.Cleanup(func() { cleanupPlatform(f.db, f.tenantID) })
	return id, blob
}

func TestPlatform_EraseParentCascades(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	f.exec(`UPDATE tenants SET platform = TRUE WHERE id = $1`, f.tenantID)
	a, blobA := f.seedChild(nil)
	b, blobB := f.seedChild(nil)
	ctx := context.Background()

	// The row erase alone refuses a platform whose children remain.
	_, err := account.NewService(f.db, nil).EraseRows(ctx, f.userID, f.tenantID, f.email, time.Now())
	require.ErrorIs(t, err, account.ErrChildrenRemain)

	res, err := f.runner.RunOnce(ctx)
	require.NoError(t, err, "%+v", res)
	require.Equal(t, 1, res.Erased, "%+v", res)
	te := res.Tenants[0]
	assert.Equal(t, outcomeErased, te.Outcome)
	assert.ElementsMatch(t, []string{a, b}, te.ChildrenErased)

	for _, c := range []string{a, b} {
		for _, q := range []string{
			`SELECT COUNT(*) FROM tenants WHERE id = $1`,
			`SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1`,
			`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`,
			`SELECT COUNT(*) FROM buckets WHERE tenant_id = $1`,
			`SELECT COUNT(*) FROM tenant_quotas WHERE tenant_id = $1`,
		} {
			assert.Zero(t, f.count(q, c), "%s: %s", c, q)
		}
		assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'account.erased'`, c), "each child's erasure is recorded")
	}
	assert.False(t, fileExists(blobA), "the child's bytes are gone")
	assert.False(t, fileExists(blobB))
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM tenants WHERE id = $1`, f.tenantID), "then the platform")
}

// A child whose own date has passed is erased alone, by the same runner:
// its objects, keys and rows go; its platform stays.
func TestPlatform_ADueChildIsErasedAlone(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(24 * time.Hour)) // the platform is not due
	f.exec(`UPDATE tenants SET platform = TRUE WHERE id = $1`, f.tenantID)
	past := time.Now().Add(-time.Hour)
	c, blob := f.seedChild(&past)
	keep, keepBlob := f.seedChild(nil)
	f.runner.onlyDue = func(d account.Due) bool { return d.TenantID == c || d.TenantID == keep }

	res, err := f.runner.RunOnce(context.Background())
	require.NoError(t, err, "%+v", res)
	require.Equal(t, 1, res.Erased, "%+v", res)
	assert.Equal(t, c, res.Tenants[0].TenantID)
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM tenants WHERE id = $1`, c))
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1`, c))
	assert.False(t, fileExists(blob))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM tenants WHERE id = $1`, keep), "a child not due stays")
	assert.True(t, fileExists(keepBlob))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM tenants WHERE id = $1`, f.tenantID), "the platform stays")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id = $1`, f.userID), "and its user")
}
