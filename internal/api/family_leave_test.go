package api

// WP-FAMILY-1 PR B (Stage 1a Prompt 56): leaving. The organizer removes a
// member, or the member leaves, or the organizer is erased: the member
// DETACHES — a root of its own again, on the free tier, its bytes out of
// the pool; over the free tier it is read-only (writes refused), and it is
// never deleted. A member is suspended only for its own abuse.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/usage"
)

// familyOf makes a family of the fixture's organizer with one member who
// holds `used` bytes (a bucket and a head row of its own, counted).
func (f *platformFixture) familyOf(used int64) familyPerson {
	f.t.Helper()
	m := f.person("famleave")
	tok, err := f.invite(m.user.Email)
	require.NoError(f.t, err)
	code, out := f.join(m, tok)
	require.Equal(f.t, http.StatusOK, code, out)
	b := "mine-" + uuid.NewString()[:8]
	_, err = f.db.Exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, $2)`, m.user.TenantID, b)
	require.NoError(f.t, err)
	_, err = f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag) VALUES ($1, $2, 'photo.jpg', $3, 'e')`,
		m.user.TenantID, b, used)
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, m.user.TenantID) })
	res, err := usage.NewQuotaManager(f.db).ReserveOnFloor(context.Background(), m.user.TenantID, usage.FloorStandard, used)
	require.NoError(f.t, err)
	require.True(f.t, res.OK)
	return m
}

func (f *platformFixture) assertDetached(member string, used int64) {
	f.t.Helper()
	var parent, kind *string
	var root string
	require.NoError(f.t, f.db.QueryRow(`SELECT parent_tenant_id, child_kind, pool_root_id FROM tenants WHERE id = $1`, member).Scan(&parent, &kind, &root))
	assert.Nil(f.t, parent, "a root again")
	assert.Nil(f.t, kind)
	assert.Equal(f.t, member, root, "its own pool")
	var have, pool, limit int64
	require.NoError(f.t, f.db.QueryRow(`SELECT storage_used_bytes, pool_used_bytes, storage_limit_bytes FROM tenant_quotas WHERE tenant_id = $1`, member).
		Scan(&have, &pool, &limit))
	assert.Equal(f.t, used, have, "its bytes are its own")
	assert.Equal(f.t, used, pool, "counted in its own pool")
	assert.Equal(f.t, usage.FreeTierLimits.StorageBytes, limit, "the free tier")
	assert.Equal(f.t, 1, countRows(f.t, f.db, `SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, member), "never deleted")
}

func TestFamily_RemovedMemberKeepsItsData(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()
	poolBefore := int64(countRows(t, f.db, `SELECT pool_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.user.TenantID))
	const used = 1 << 20
	m := f.familyOf(used)
	require.Equal(t, poolBefore+used, int64(countRows(t, f.db, `SELECT pool_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.user.TenantID)))

	// The organizer caps a member, then removes them. A management key never does.
	key, _ := f.mgmtKey(nil, auth.ScopeManageWrite)
	code, out := f.call(http.MethodDelete, "/api/v1/manage/family/members/"+m.user.TenantID, key, nil)
	assert.Equal(t, http.StatusForbidden, code, out)
	code, out = f.call(http.MethodPut, "/api/v1/manage/family/members/"+m.user.TenantID+"/cap", f.jwt, map[string]any{"storage_limit_bytes": 2 << 20})
	require.Equal(t, http.StatusOK, code, out)
	assert.EqualValues(t, 2<<20, countRows(t, f.db, `SELECT storage_limit_bytes FROM tenant_quotas WHERE tenant_id = $1`, m.user.TenantID))
	code, out = f.call(http.MethodDelete, "/api/v1/manage/family/members/"+uuid.NewString(), f.jwt, nil)
	assert.Equal(t, http.StatusNotFound, code, "no such member: %v", out)
	code, out = f.call(http.MethodDelete, "/api/v1/manage/family/members/"+m.user.TenantID, f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	f.assertDetached(m.user.TenantID, used)
	assert.Equal(t, poolBefore, int64(countRows(t, f.db, `SELECT pool_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.user.TenantID)),
		"its bytes left the organizer's pool")
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'account.sponsor_detached'`, m.user.TenantID))

	// Over the free tier: read-only, never deleted.
	_, err := f.db.Exec(`UPDATE tenant_quotas SET storage_used_bytes = storage_limit_bytes + 1, pool_used_bytes = storage_limit_bytes + 1 WHERE tenant_id = $1`, m.user.TenantID)
	require.NoError(t, err)
	res, err := usage.NewQuotaManager(f.db).ReserveOnFloor(context.Background(), m.user.TenantID, usage.FloorStandard, 1)
	require.NoError(t, err)
	assert.False(t, res.OK, "over the free tier, a write is refused (the S3 path's 403 QuotaExceeded)")
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, m.user.TenantID))

	// A member leaves on its own.
	m2 := f.familyOf(4096)
	code, out = f.call(http.MethodPost, "/api/v1/manage/family/leave", f.jwt, nil)
	assert.Equal(t, http.StatusConflict, code, "the organizer is in no family: %v", out)
	code, out = f.call(http.MethodPost, "/api/v1/manage/family/leave", m2.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	f.assertDetached(m2.user.TenantID, 4096)
}

func TestFamily_SponsorErasureDetachesNeverErases(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	ctx := context.Background()
	authSvc := auth.NewAuthService(nil, f.db)
	mu, _, _, err := authSvc.CreateUserWithTenant(ctx, "famerase-"+uuid.NewString()[:8]+"@family.example", "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	member := mu.TenantID
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM object_head_cache WHERE tenant_id = $1`, `DELETE FROM buckets WHERE tenant_id = $1`} {
			_, _ = f.db.Exec(q, member)
		}
		cleanupPlatform(f.db, member)
	})
	_, token, err := auth.InviteFamilyMember(ctx, f.db, f.tenantID, f.userID, mu.Email, time.Now())
	require.NoError(t, err)
	_, err = auth.AcceptFamilyInvitation(ctx, f.db, token, mu.ID, mu.Email, member, time.Now())
	require.NoError(t, err)
	f.exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, 'my-library')`, member)
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name) VALUES ($1, 'my-library', 'a.jpg', 13, 'e', 'local')`, member)
	f.exec(`UPDATE tenant_quotas SET storage_used_bytes = 13 WHERE tenant_id = $1`, member)
	f.exec(`UPDATE tenant_quotas SET pool_used_bytes = pool_used_bytes + 13 WHERE tenant_id = $1`, f.tenantID)

	res, err := f.runner.RunOnce(ctx)
	require.NoError(t, err, "%+v", res)
	require.Equal(t, 1, res.Erased, "%+v", res)
	assert.Equal(t, []string{member}, res.Tenants[0].ChildrenDetached, "the member was detached, through a real invitation")
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM tenants WHERE id = $1`, f.tenantID), "the organizer is gone")
	var parent *string
	var root string
	require.NoError(t, f.db.QueryRow(`SELECT parent_tenant_id, pool_root_id FROM tenants WHERE id = $1`, member).Scan(&parent, &root))
	assert.Nil(t, parent)
	assert.Equal(t, member, root)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, member), "its library untouched")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id = $1`, mu.ID), "its sign-in untouched")
	var limit int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_limit_bytes FROM tenant_quotas WHERE tenant_id = $1`, member).Scan(&limit))
	assert.Equal(t, usage.FreeTierLimits.StorageBytes, limit, "the free tier of its own")
}

func TestFamily_MemberSuspendedOnlyForItsOwnAbuse(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()
	a := f.familyOf(10)
	b := f.familyOf(10)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = f.db.Exec(`UPDATE tenants SET suspended_at = NULL WHERE id IN ($1, $2, $3)`, f.user.TenantID, a.user.TenantID, b.user.TenantID)
	})

	_, err := f.db.Exec(`UPDATE tenants SET suspended_at = NOW() WHERE id = $1`, f.user.TenantID)
	require.NoError(t, err)
	assert.True(t, isTenantSuspended(ctx, f.db, f.user.TenantID))
	assert.False(t, isTenantSuspended(ctx, f.db, a.user.TenantID), "the organizer's suspension never suspends a member")
	assert.False(t, isTenantSuspended(ctx, f.db, b.user.TenantID))
	_, err = f.db.Exec(`UPDATE tenants SET suspended_at = NULL WHERE id = $1`, f.user.TenantID)
	require.NoError(t, err)

	_, err = f.db.Exec(`UPDATE tenants SET suspended_at = NOW() WHERE id = $1`, a.user.TenantID)
	require.NoError(t, err)
	assert.True(t, isTenantSuspended(ctx, f.db, a.user.TenantID), "a member's own abuse suspends it")
	assert.False(t, isTenantSuspended(ctx, f.db, b.user.TenantID), "never its family")
	assert.False(t, isTenantSuspended(ctx, f.db, f.user.TenantID), "nor its organizer")
}

// RP92-1: a member that holds a paid house of its own (one granted before
// the house writers refused a sponsored member, or by an operator) keeps it
// when it detaches — never the free tier over what it paid for.
func TestFamily_DetachKeepsAPaidHouse(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()
	m := f.familyOf(4096)
	_, err := f.db.Exec(`INSERT INTO entitlements (tenant_id, std_tb, vault_tb, period, paid_through, renewal, provider)
		VALUES ($1, 2, 1, 'monthly', NOW() + interval '20 days', 'auto', 'manual')`, m.user.TenantID)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO tenant_floor_quotas (tenant_id, floor, storage_limit_bytes, storage_used_bytes)
		VALUES ($1, 'standard', $2, 4096), ($1, 'vault', $3, 0)
		ON CONFLICT (tenant_id, floor) DO UPDATE SET storage_limit_bytes = EXCLUDED.storage_limit_bytes`,
		m.user.TenantID, 2*usage.TB, usage.TB)
	require.NoError(t, err)

	code, out := f.call(http.MethodDelete, "/api/v1/manage/family/members/"+m.user.TenantID, f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)

	var limit int64
	var tier string
	require.NoError(t, f.db.QueryRow(`SELECT storage_limit_bytes, tier FROM tenant_quotas WHERE tenant_id = $1`, m.user.TenantID).Scan(&limit, &tier))
	assert.Equal(t, 3*usage.TB, limit, "the house it paid for, not the free tier")
	assert.Equal(t, "standard", tier)
	assert.EqualValues(t, 2*usage.TB, countRows(t, f.db, `SELECT storage_limit_bytes FROM tenant_floor_quotas WHERE tenant_id = $1 AND floor = 'standard'`, m.user.TenantID))
	assert.EqualValues(t, usage.TB, countRows(t, f.db, `SELECT storage_limit_bytes FROM tenant_floor_quotas WHERE tenant_id = $1 AND floor = 'vault'`, m.user.TenantID))

	// A LAPSED entitlement is no house: the free tier, as before.
	m2 := f.familyOf(4096)
	_, err = f.db.Exec(`INSERT INTO entitlements (tenant_id, std_tb, period, paid_through, renewal, provider, lapsed_at)
		VALUES ($1, 1, 'monthly', NOW() - interval '20 days', 'prepaid', 'manual', NOW() - interval '1 day')`, m2.user.TenantID)
	require.NoError(t, err)
	code, out = f.call(http.MethodPost, "/api/v1/manage/family/leave", m2.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	f.assertDetached(m2.user.TenantID, 4096)
}
