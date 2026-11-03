package api

// WP-FAMILY-1 PR A (Stage 1a Prompt 56; docs/ACCOUNTS.md § A family; F-40):
// a family is a POOL of personal tenants. The organizer's root pays; each
// member is a sponsored child — its own login, keys, recovery and library —
// joined through a real invitation (tenant_invitations.kind = 'family').
// The organizer sees usage totals and caps, and nothing else: never a key,
// a bucket, an object or a request of a member.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/usage"
)

// withFamily turns the family flag on for the organizer (the fixture's
// tenant) and gives it two-factor sign-in (the invitation's fresh code).
func (f *platformFixture) withFamily() *platformFixture {
	f.t.Helper()
	f.withAgents().withApprovals()
	require.NoError(f.t, f.s.flags.Set(context.Background(), flagFamily, f.user.TenantID, true, "test"))
	require.NoError(f.t, f.s.flags.Refresh(context.Background()))
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM tenant_invitations WHERE tenant_id = $1`, f.user.TenantID)
		// A member is a person's own account: never erased with the test's organizer.
		_, _ = f.db.Exec(`UPDATE tenants SET parent_tenant_id = NULL, child_kind = NULL, pool_root_id = id
			WHERE parent_tenant_id = $1 AND child_kind = 'sponsored'`, f.user.TenantID)
	})
	return f
}

// familyPerson is another person with an account of their own.
type familyPerson struct {
	user *auth.User
	jwt  string
}

func (f *platformFixture) person(prefix string) familyPerson {
	f.t.Helper()
	u, _, _, err := f.s.auth.CreateUserWithTenant(context.Background(), prefix+"-"+uuid.NewString()[:8]+"@family.example", "Str0ngPassw0rd!", "x")
	require.NoError(f.t, err)
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`UPDATE tenants SET parent_tenant_id = NULL, child_kind = NULL, pool_root_id = id WHERE id = $1`, u.TenantID)
		_, _ = f.db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, u.TenantID)
		_, _ = f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, u.TenantID)
		cleanupPlatform(f.db, u.TenantID)
	})
	jwt, err := f.s.auth.GenerateJWT(u)
	require.NoError(f.t, err)
	return familyPerson{user: u, jwt: jwt}
}

// invite invites p's address to the organizer's family (the function the
// API calls; the API's own route is tested once, with its TOTP code).
func (f *platformFixture) invite(email string) (string, error) {
	f.t.Helper()
	_, token, err := auth.InviteFamilyMember(context.Background(), f.db, f.user.TenantID, f.user.ID, email, time.Now())
	return token, err
}

func (f *platformFixture) join(p familyPerson, token string) (int, map[string]any) {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/manage/family/join/"+token, p.jwt, nil)
}

func TestFamily_SixMembersMax(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()

	// The API's invitation: a person, the owner, a fresh authenticator code.
	first := f.person("fam1")
	code, out := f.call(http.MethodPost, "/api/v1/manage/family/invitations", f.jwt, map[string]any{"email": first.user.Email})
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "totp_required", errCode(out))
	code, out = f.call(http.MethodPost, "/api/v1/manage/family/invitations", f.jwt, map[string]any{"email": first.user.Email, "code": f.totpCode(time.Time{})})
	require.Equal(t, http.StatusCreated, code, out)
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM tenant_invitations WHERE tenant_id = $1 AND kind = 'family'`, f.user.TenantID))
	key, _ := f.mgmtKey(nil, auth.ScopeManageWrite)
	code, out = f.call(http.MethodPost, "/api/v1/manage/family/invitations", key, map[string]any{"email": "x@family.example"})
	assert.Equal(t, http.StatusForbidden, code, "a management key never invites: %v", out)
	assert.Equal(t, "person_required", errCode(out))

	// Six people: the organizer and five members, pending invitations counted.
	people := []familyPerson{first}
	tokens := []string{}
	tok, err := f.invite(first.user.Email) // re-invite = the resend: the first link dies
	require.NoError(t, err)
	tokens = append(tokens, tok)
	for i := 2; i <= auth.MaxFamilyPeople-1; i++ {
		p := f.person("fam")
		people = append(people, p)
		tok, err := f.invite(p.user.Email)
		require.NoError(t, err, "invitation %d", i)
		tokens = append(tokens, tok)
	}
	extra := f.person("famx")
	_, err = f.invite(extra.user.Email)
	assert.ErrorIs(t, err, auth.ErrFamilyFull, "a sixth member is a seventh person: pending invitations count")

	for i, p := range people {
		code, out := f.join(p, tokens[i])
		require.Equal(t, http.StatusOK, code, "member %d joins: %v", i+1, out)
		var parent, kind, root string
		require.NoError(t, f.db.QueryRow(`SELECT parent_tenant_id, child_kind, pool_root_id FROM tenants WHERE id = $1`, p.user.TenantID).Scan(&parent, &kind, &root))
		assert.Equal(t, f.user.TenantID, parent)
		assert.Equal(t, "sponsored", kind)
		assert.Equal(t, f.user.TenantID, root, "the organizer's pool")
	}
	_, err = f.invite(extra.user.Email)
	assert.ErrorIs(t, err, auth.ErrFamilyFull, "five members and the organizer: full")
}

func TestFamily_JoinRefusedWhenTheMemberPaysItsOwnPlan(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()
	p := f.person("fampaid")
	tok, err := f.invite(p.user.Email)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO entitlements (tenant_id, std_tb, period, paid_through, renewal, provider)
		VALUES ($1, 1, 'monthly', NOW() + interval '20 days', 'auto', 'manual')`, p.user.TenantID)
	require.NoError(t, err)
	code, out := f.join(p, tok)
	assert.Equal(t, http.StatusConflict, code, out)
	assert.Equal(t, "member_pays_own_plan", errCode(out), "F-40: join after cancelling — no proration")
	var kind *string
	require.NoError(t, f.db.QueryRow(`SELECT child_kind FROM tenants WHERE id = $1`, p.user.TenantID).Scan(&kind))
	assert.Nil(t, kind, "not attached")

	// Another address's link is no one else's.
	other := f.person("famother")
	code, out = f.join(other, tok)
	assert.Equal(t, http.StatusForbidden, code, out)
}

func TestFamily_OrganizerCannotMintAMembersKey(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()
	// The organizer is also a platform: its platform key reaches its managed
	// children — never a sponsored one.
	code, out := f.call(http.MethodPut, "/api/v1/admin/tenants/"+f.user.TenantID+"/platform", f.adminJWT, map[string]any{"platform": true})
	require.Equal(t, http.StatusOK, code, out)
	platformKey, _ := f.mgmtKey(nil, auth.ScopePlatform)
	_, agent := f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude"})
	m := f.person("fammint")
	tok, err := f.invite(m.user.Email)
	require.NoError(t, err)
	code, out = f.join(m, tok)
	require.Equal(t, http.StatusOK, code, out)
	member := m.user.TenantID
	keysBefore := countRows(t, f.db, `SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1`, member)

	// Every platform route that names a tenant, on the member.
	var paths []struct{ method, path string }
	require.NoError(t, chi.Walk(f.s.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/api/v1/platform/tenants/{id}") {
			p := strings.ReplaceAll(route, "{id}", member)
			p = strings.ReplaceAll(p, "{key}", uuid.NewString())
			paths = append(paths, struct{ method, path string }{method, p})
		}
		return nil
	}))
	require.NotEmpty(t, paths)
	for _, cred := range []struct{ name, token string }{{"the organizer's JWT", f.jwt}, {"a platform key", platformKey}, {"an agent grant", agent.Live}} {
		for _, p := range paths {
			code, out := f.call(p.method, p.path, cred.token, map[string]any{"name": "x", "label": "x"})
			assert.Contains(t, []int{http.StatusNotFound, http.StatusForbidden, http.StatusUnauthorized}, code,
				"%s: %s %s reaches the member: %v", cred.name, p.method, p.path, out)
		}
		code, out := f.call(http.MethodGet, "/api/v1/platform/tenants", cred.token, nil)
		if code == http.StatusOK {
			assert.NotContains(t, string(mustJSON(t, out)), member, "%s: a list never names a sponsored member", cred.name)
		}
	}
	// The organizer's JWT naming the member's tenant: no member of it.
	asMember, err := f.s.auth.GenerateJWTForTenant(f.user, member, auth.RoleOwner)
	require.NoError(t, err)
	for _, p := range []string{"/api/v1/manage/buckets", "/api/v1/manage/keys", "/api/v1/manage/requests"} {
		code, out := f.call(http.MethodGet, p, asMember, nil)
		assert.Equal(t, http.StatusForbidden, code, "%s: %v", p, out)
	}
	code, out = f.call(http.MethodPost, "/api/v1/manage/keys", asMember, map[string]any{"name": "sneaky"})
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, keysBefore, countRows(t, f.db, `SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1`, member), "no key was minted on the member")
}

func TestFamily_OrganizerSeesUsageNeverNames(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()
	m := f.person("famusage")
	tok, err := f.invite(m.user.Email)
	require.NoError(t, err)
	code, out := f.join(m, tok)
	require.Equal(t, http.StatusOK, code, out)
	member := m.user.TenantID

	// The member uploads into their own library: counted in the pool.
	const secretBucket, secretKey = "holiday-photos-2026", "passport-scan.jpg"
	_, err = f.db.Exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, $2)`, member, secretBucket)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag) VALUES ($1, $2, $3, 700, 'e')`, member, secretBucket, secretKey)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, member) })
	poolBefore := countRows(t, f.db, `SELECT pool_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.user.TenantID)
	res, err := usage.NewQuotaManager(f.db).ReserveOnFloor(context.Background(), member, usage.FloorStandard, 700)
	require.NoError(t, err)
	require.True(t, res.OK)
	assert.Equal(t, poolBefore+700, countRows(t, f.db, `SELECT pool_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.user.TenantID),
		"a member's upload is counted in the organizer's pool")

	code, out = f.call(http.MethodGet, "/api/v1/manage/family", f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	body := string(mustJSON(t, out))
	assert.NotContains(t, body, secretBucket, "never a bucket name")
	assert.NotContains(t, body, secretKey, "never an object name")
	members := out["members"].([]any)
	require.Len(t, members, 1)
	row := members[0].(map[string]any)
	assert.Equal(t, member, row["id"])
	assert.Equal(t, m.user.Email, row["label"], "the address the organizer invited")
	assert.EqualValues(t, 700, row["storage_used_bytes"])
	assert.Nil(t, row["storage_limit_bytes"], "no cap: the pool decides")
	allowed := map[string]bool{"object": true, "id": true, "label": true, "storage_used_bytes": true, "storage_limit_bytes": true, "joined_at": true}
	for k := range row {
		assert.True(t, allowed[k], "a member row says %q: only usage and caps", k)
	}
	assert.EqualValues(t, poolBefore+700, out["pool_used_bytes"])

	// A member is no organizer: the family view is the organizer's.
	code, out = f.call(http.MethodGet, "/api/v1/manage/family", m.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, "member", out["role"])
	assert.NotContains(t, out, "members", "a member sees whose family they are in, not the others' usage")
}
