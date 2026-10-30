package api

// WP-ACCT-1 (Stage 1a Prompt 53, PR C): the edges' rules
// (docs/ACCOUNTS.md). Suspension flows down managed and app edges only; the
// erasure cascades down them and DETACHES a sponsored child — a person's
// own account a sponsor pays for, which keeps its data. And RB10-1: a
// member leaves on its own; a sign-in with no tenant and no membership
// erases itself, the user rows only.

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/auth"
)

func TestTree_SponsoredNeverSuspendedOrErasedWithTheSponsor(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	ctx := context.Background()
	managed, managedBlob := f.seedChild(nil)

	// A sponsored child: a person's own account the sponsor pays for (made
	// by SQL: WP-FAMILY-1 makes the edge). Its own bucket and object.
	sp := "tenant-sp-" + uuid.NewString()[:12]
	f.exec(`INSERT INTO tenants (id, name, email, parent_tenant_id, child_kind) VALUES ($1, 'sp', $2, $3, 'sponsored')`,
		sp, sp+"@person.invalid", f.tenantID)
	f.exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes) VALUES ($1, 0, 13)`, sp)
	f.exec(`UPDATE tenant_quotas SET pool_used_bytes = pool_used_bytes + 13 WHERE tenant_id = $1`, f.tenantID)
	f.exec(`INSERT INTO tenant_floor_quotas (tenant_id, floor, storage_limit_bytes, storage_used_bytes) VALUES ($1, 'standard', 0, 13)`, sp)
	spBucket := "sp-bkt-" + sp[10:16]
	f.exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, $2)`, sp, spBucket)
	spBlob := filepath.Join(f.primary, sp+"_"+spBucket, "mine.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(spBlob), 0o750))
	require.NoError(t, os.WriteFile(spBlob, []byte("my own bytes"), 0o600))
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name) VALUES ($1, $2, 'mine.txt', 13, 'e', 'local')`, sp, spBucket)
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM object_head_cache WHERE tenant_id = $1`, `DELETE FROM buckets WHERE tenant_id = $1`,
			`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, `DELETE FROM tenant_quotas WHERE tenant_id = $1`,
			`DELETE FROM audit_logs WHERE tenant_id = $1`, `DELETE FROM tenants WHERE id = $1`} {
			_, _ = f.db.Exec(q, sp)
		}
	})

	// The sponsor suspended: its managed child is, its sponsored child is not.
	f.exec(`UPDATE tenants SET suspended_at = NOW() WHERE id = $1`, f.tenantID)
	assert.True(t, isTenantSuspended(ctx, f.db, managed), "suspension flows down a managed edge")
	assert.False(t, isTenantSuspended(ctx, f.db, sp), "never down a sponsored one")
	f.exec(`UPDATE tenants SET suspended_at = NULL WHERE id = $1`, f.tenantID)

	// The sponsor erased: the managed child with it, the sponsored child detached.
	res, err := f.runner.RunOnce(ctx)
	require.NoError(t, err, "%+v", res)
	require.Equal(t, 1, res.Erased, "%+v", res)
	te := res.Tenants[0]
	assert.Equal(t, []string{managed}, te.ChildrenErased)
	assert.Equal(t, []string{sp}, te.ChildrenDetached)
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM tenants WHERE id = $1`, managed))
	assert.False(t, fileExists(managedBlob))
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM tenants WHERE id = $1`, f.tenantID), "the sponsor is gone")

	var parent, kind sql.NullString
	var root string
	require.NoError(t, f.db.QueryRow(`SELECT parent_tenant_id, child_kind, pool_root_id FROM tenants WHERE id = $1`, sp).Scan(&parent, &kind, &root))
	assert.False(t, parent.Valid, "detached")
	assert.False(t, kind.Valid)
	assert.Equal(t, sp, root, "its own pool")
	assert.True(t, fileExists(spBlob), "its bytes untouched")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, sp), "its rows untouched")
	var used, pool, limit int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_used_bytes, pool_used_bytes, storage_limit_bytes FROM tenant_quotas WHERE tenant_id = $1`, sp).Scan(&used, &pool, &limit))
	assert.Equal(t, int64(13), used)
	assert.Equal(t, used, pool, "its own pool counts its own bytes")
	assert.Positive(t, limit, "a free tier of its own, never a limit of 0")
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM tenant_floor_quotas WHERE tenant_id = $1 AND storage_limit_bytes = 0`, sp),
		"the pool's counting floor rows went with the pool")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'account.sponsor_detached'`, sp))
}

// RB10-1: a member leaves an account on its own — its membership only; the
// owner cannot leave its own account.
func TestMembers_ALeaveRemovesTheMembershipOnly(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	viewer, viewerJWT := f.addMember(auth.RoleViewer)
	other, _ := f.addMember(auth.RoleDeveloper)

	code, out := f.call(http.MethodDelete, "/api/v1/manage/members/"+other.ID, viewerJWT, nil)
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "role_forbidden", errCode(out), "a viewer removes no one else")

	code, out = f.call(http.MethodDelete, "/api/v1/manage/members/"+viewer.ID, viewerJWT, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM users WHERE id = $1`, viewer.ID), "its sign-in stays")
	assert.Zero(t, countRows(t, f.db, `SELECT COUNT(*) FROM tenant_members WHERE user_id = $1`, viewer.ID))
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM tenants WHERE id = $1`, f.user.TenantID), "the account stays")
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'member.left'`, f.user.TenantID))

	code, out = f.call(http.MethodDelete, "/api/v1/manage/members/"+f.user.ID, f.jwt, nil)
	assert.Equal(t, http.StatusConflict, code, out)
	assert.Equal(t, "owner_is_permanent", errCode(out), "the owner cannot leave")
}

func countRows(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(q, args...).Scan(&n))
	return n
}

// RB10-1: a sign-in with no tenant of its own and no membership left asks
// for its own erasure (DELETE /api/v1/user/signin): on the date the user
// rows go, the keys it once made on a team stay the team's (RP84-1), and
// no tenant is touched. A member somewhere is refused (409 still_a_member).
func TestAccount_TenantlessSignInErasesTheUserOnly(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	f.s.registerUserAPIRoutes()
	ctx := context.Background()
	u, jwtOnTeam := f.addMember(auth.RoleDeveloper)
	code, out := f.call(http.MethodPost, "/api/v1/manage/keys", jwtOnTeam, map[string]any{"name": "made by u"})
	require.Equal(t, http.StatusCreated, code, out)
	keyRow := out["id"].(string)
	jwt, err := f.s.auth.GenerateJWTForTenant(u, "", "")
	require.NoError(t, err)

	code, out = f.call(http.MethodDelete, "/api/v1/user/signin", jwt, map[string]any{"reason": "done"})
	assert.Equal(t, http.StatusConflict, code, out)
	assert.Equal(t, "still_a_member", errCode(out), "a member leaves first")

	code, out = f.call(http.MethodDelete, "/api/v1/manage/members/"+u.ID, jwtOnTeam, nil)
	require.Equal(t, http.StatusOK, code, out)
	code, out = f.call(http.MethodDelete, "/api/v1/user/signin", jwt, map[string]any{"reason": "done"})
	require.Equal(t, http.StatusOK, code, out)
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM users WHERE id = $1`, u.ID).Scan(&status))
	assert.Equal(t, account.StatusPending, status)

	// On the date: the user rows only.
	_, err = f.db.Exec(`UPDATE users SET deletion_scheduled_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, u.ID)
	require.NoError(t, err)
	svc := account.NewService(f.db, nil)
	_, err = svc.EraseRows(ctx, u.ID, "", u.Email, time.Now())
	require.NoError(t, err)
	assert.Zero(t, countRows(t, f.db, `SELECT COUNT(*) FROM users WHERE id = $1`, u.ID))
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM tenants WHERE id = $1`, f.user.TenantID), "the team is untouched")
	var maker string
	require.NoError(t, f.db.QueryRow(`SELECT user_id::text FROM api_keys WHERE id = $1`, keyRow).Scan(&maker))
	assert.Equal(t, f.user.ID, maker, "the key it made is the team's owner's now")
}

// RP86-4: the "member of nothing" check ran only when a sign-in's erasure
// was scheduled. One that accepted an invitation during its 30-day grace was
// a member on the date, and the runner's EraseRows(user, "") deleted it with
// the membership. On the date a tenant-less erasure is re-checked under the
// user's lock: a member again → the schedule is cleared
// (account.deletion_cancelled, reason became_member), never an erasure —
// before the walk (the recovery realms are asked nothing) and again inside
// the row erase's own transaction.
func TestAccount_ASignInThatRejoinedIsNotErased(t *testing.T) {
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(24 * time.Hour)) // the team itself is not due
	ctx := context.Background()
	u := uuid.NewString()
	email := "rejoined-" + u[:8] + "@stored.ge"
	f.exec(`INSERT INTO users (id, email, password_hash, status, deletion_scheduled_at, deletion_reason)
	        VALUES ($1, $2, 'x', 'pending_deletion', NOW() - INTERVAL '1 hour', 'done')`, u, email)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM tenant_members WHERE user_id = $1`, u)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, u)
		_, _ = f.db.Exec(`DELETE FROM users WHERE id = $1`, u)
	})
	// The invitation accepted during the grace.
	f.exec(`INSERT INTO tenant_members (tenant_id, user_id, role) VALUES ($1, $2, 'viewer')`, f.tenantID, u)
	f.runner.onlyDue = func(d account.Due) bool { return d.UserID == u }

	res, err := f.runner.RunOnce(ctx)
	require.NoError(t, err, "%+v", res)
	assert.Zero(t, res.Erased, "%+v", res)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id = $1 AND status = 'active' AND deletion_scheduled_at IS NULL`, u),
		"the schedule is cleared")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM tenant_members WHERE user_id = $1`, u), "the membership stays")
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_logs WHERE user_id = $1 AND action = 'account.deletion_cancelled'
		AND metadata->>'reason' = 'became_member'`, u))

	// The row erase's own guard: a member again between the walk and the
	// erase is refused in the erase's transaction.
	f.exec(`UPDATE users SET status = 'pending_deletion', deletion_scheduled_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, u)
	_, err = account.NewService(f.db, nil).EraseRows(ctx, u, "", email, time.Now())
	assert.ErrorIs(t, err, account.ErrNotPending)
	assert.ErrorIs(t, err, account.ErrBecameMember)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM users WHERE id = $1 AND deletion_scheduled_at IS NULL`, u))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM tenant_members WHERE user_id = $1`, u))
}
