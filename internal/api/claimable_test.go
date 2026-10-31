package api

// WP-APP-1 PR D (docs/ACCOUNTS.md § Apps; F-42): claimable apps for AI app
// builders. A PARTNER's management key (an operator grants the scope; no
// anonymous creation) makes a provisional tenant — 1 GiB, never Public,
// 7 days — and a single-use claim link (only its sha256 kept). A person who
// claims it keeps it, with its data, as an app under their own account, in
// one transaction: the pool counter moves, the keys stay the tenant's.
// Unclaimed, it is purged when its 7 days end.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/modes"
	"github.com/FairForge/vaultaire/internal/usage"
)

// partner grants the fixture's tenant the partner scope (an operator's act)
// and mints its partner key.
func (f *platformFixture) partner() string {
	f.t.Helper()
	code, out := f.call(http.MethodPut, "/api/v1/admin/tenants/"+f.user.TenantID+"/partner", f.adminJWT, map[string]any{"partner": true})
	require.Equal(f.t, http.StatusOK, code, out)
	key, _ := f.mgmtKey(nil, auth.ScopePartner)
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM claims WHERE partner_tenant_id = $1`, f.user.TenantID)
	})
	return key
}

func (f *platformFixture) claimable(key, label string) map[string]any {
	f.t.Helper()
	code, out := f.call(http.MethodPost, "/api/v1/partner/claimables", key, map[string]any{"label": label})
	require.Equal(f.t, http.StatusCreated, code, out)
	return out
}

func claimToken(out map[string]any) string {
	u := out["claim_url"].(string)
	return u[strings.LastIndex(u, "/")+1:]
}

func TestClaimable_PartnerOnlyAndExpires(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withApps(true, true)
	f.s.registerPartnerRoutes()
	cleanupDescendants(f)

	// No anonymous creation, no person, no other key: a partner's key only.
	_, _, err := auth.MintManagementKey(context.Background(), f.db, f.user.TenantID, auth.ManagementKeyOptions{Name: "x", Scopes: []string{auth.ScopePartner}})
	assert.ErrorIs(t, err, auth.ErrManagementKeyScope, "the scope is an operator's grant")
	server, _ := f.mgmtKey(nil, auth.ScopeManageWrite)
	for _, tok := range []string{"", f.jwt, server} {
		code, _ := f.call(http.MethodPost, "/api/v1/partner/claimables", tok, map[string]any{"label": "x"})
		assert.Contains(t, []int{http.StatusNotFound, http.StatusUnauthorized}, code)
	}
	key := f.partner()
	out := f.claimable(key, "my lovable app")
	id := out["tenant_id"].(string)
	token := claimToken(out)
	assert.Contains(t, out["claim_url"], "/claim/")
	assert.True(t, strings.HasPrefix(out["management_key"].(map[string]any)["secret"].(string), auth.ManagementKeyLivePrefix), "the partner's builder stores with it")
	exp, err := time.Parse(time.RFC3339Nano, out["expires_at"].(string))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(7*24*time.Hour), exp, time.Minute)

	var limit int64
	var parent string
	require.NoError(t, f.db.QueryRow(`SELECT q.storage_limit_bytes, t.parent_tenant_id FROM tenants t JOIN tenant_quotas q ON q.tenant_id = t.id WHERE t.id = $1`, id).
		Scan(&limit, &parent))
	assert.EqualValues(t, 1<<30, limit, "1 GiB")
	assert.Equal(t, f.user.TenantID, parent, "the partner holds it while it is provisional")
	sum := sha256.Sum256([]byte(token))
	var stored string
	require.NoError(t, f.db.QueryRow(`SELECT token_hash FROM claims WHERE tenant_id = $1`, id).Scan(&stored))
	assert.Equal(t, hex.EncodeToString(sum[:]), stored, "only the token's sha256 is kept")

	// Never Public.
	_, err = f.db.Exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, 'prov-site')`, id)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, id) })
	public := modes.VisibilityPublic
	_, err = modes.SetBucket(context.Background(), f.db, id, "prov-site", modes.Change{Visibility: &public})
	assert.True(t, errors.Is(err, modes.ErrUnverifiedNeverPublic), "%v", err)

	// 7 days: then purged (scheduled now, the erasure runner's), and the
	// link is dead.
	_, err = f.db.Exec(`UPDATE tenants SET provisional_until = NOW() - INTERVAL '1 second' WHERE id = $1`, id)
	require.NoError(t, err)
	n, err := f.s.purgeExpiredClaimables(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	var due bool
	require.NoError(t, f.db.QueryRow(`SELECT deletion_scheduled_at <= NOW() FROM tenants WHERE id = $1`, id).Scan(&due))
	assert.True(t, due, "due now: the deletion runner erases it")
	code, out := f.call(http.MethodPost, "/api/v1/manage/claims/"+token, f.jwt, nil)
	assert.Equal(t, http.StatusGone, code, out)
}

func TestClaimable_ClaimMovesTheDataUnderTheClaimer(t *testing.T) {
	// The partner and the claimer are two accounts.
	partner := setupPlatformFixture(t).withManagement().withApps(true, true)
	partner.s.registerPartnerRoutes()
	cleanupDescendants(partner)
	key := partner.partner()
	claimer := setupPlatformFixture(t).withManagement().withApps(true, false)
	claimer.s = partner.s // one server: its auth service signs the claimer's token
	jwt, err := partner.s.auth.GenerateJWT(claimer.user)
	require.NoError(t, err)
	claimer.jwt = jwt
	cleanupDescendants(claimer)

	out := partner.claimable(key, "a bolt app")
	id, token := out["tenant_id"].(string), claimToken(out)
	appKey := out["management_key"].(map[string]any)["secret"].(string)
	code, kout := partner.call(http.MethodPost, "/api/v1/manage/keys", appKey, map[string]any{"name": "the app's s3 key"})
	require.Equal(t, http.StatusCreated, code, kout)
	s3Key := kout["id"].(string)
	qm := usage.NewQuotaManager(partner.db)
	res, err2 := qm.ReserveOnFloor(context.Background(), id, usage.FloorStandard, 500)
	err = err2
	require.NoError(t, err)
	require.True(t, res.OK)
	_, err = partner.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag) VALUES ($1, 'data', 'a.txt', 500, 'e')`, id)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = partner.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, id) })
	poolUsed := func(root string) int64 {
		var n int64
		require.NoError(t, partner.db.QueryRow(`SELECT pool_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, root).Scan(&n))
		return n
	}
	partnerBefore, claimerBefore := poolUsed(partner.user.TenantID), poolUsed(claimer.user.TenantID)

	// A management key never claims: a person does.
	claimerKey, _ := claimer.mgmtKey(nil, auth.ScopeManageWrite)
	code, out = claimer.call(http.MethodPost, "/api/v1/manage/claims/"+token, claimerKey, nil)
	assert.Equal(t, http.StatusForbidden, code, out)
	code, out = claimer.call(http.MethodPost, "/api/v1/manage/claims/"+token, claimer.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, id, out["id"])

	var parent, kind, root string
	var provisional bool
	require.NoError(t, partner.db.QueryRow(`SELECT parent_tenant_id, child_kind, pool_root_id, provisional_until IS NOT NULL FROM tenants WHERE id = $1`, id).
		Scan(&parent, &kind, &root, &provisional))
	assert.Equal(t, claimer.user.TenantID, parent, "an app under the claimer")
	assert.Equal(t, "app", kind)
	assert.Equal(t, claimer.user.TenantID, root, "in the claimer's pool")
	assert.False(t, provisional)
	assert.Equal(t, partnerBefore-500, poolUsed(partner.user.TenantID), "the bytes left the partner's pool")
	assert.Equal(t, claimerBefore+500, poolUsed(claimer.user.TenantID), "and are the claimer's")
	var objects int
	require.NoError(t, partner.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, id).Scan(&objects))
	assert.Equal(t, 1, objects, "its data with it")
	var keyUser string
	require.NoError(t, partner.db.QueryRow(`SELECT user_id::text FROM api_keys WHERE id = $1`, s3Key).Scan(&keyUser))
	assert.Equal(t, claimer.user.ID, keyUser, "its keys stay the tenant's and hang on the claimer now")

	code, out = claimer.call(http.MethodPost, "/api/v1/manage/claims/"+token, claimer.jwt, nil)
	assert.Equal(t, http.StatusGone, code, "single-use: %v", out)
	// The claimer's apps list it.
	code, out = claimer.call(http.MethodGet, "/api/v1/manage/apps", claimer.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	require.Len(t, out["data"], 1)
}
