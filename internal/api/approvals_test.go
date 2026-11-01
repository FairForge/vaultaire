package api

// WP-AGENT-1 PR B (F-41): what could lose or expose data is never in a
// default agent scope. It answers 403 approval_required with an approval
// link; the person approves in the dashboard or the API (a fresh TOTP code
// for a destructive act); the approval names ONE action on ONE resource, is
// used once and lives 24 h. Buckets an agent creates are versioned, and a
// grant never reaches a Private bucket (contract line 8).

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/modes"
)

const approvalsTOTPSecret = "JBSWY3DPEHPK3PXP"

// withApprovals gives the fixture's owner two-factor sign-in (approvals of a
// destructive act need a fresh code) and the server its MFA service.
func (f *platformFixture) withApprovals() *platformFixture {
	f.t.Helper()
	f.s.mfaService = auth.NewMFAService("stored.ge")
	require.NoError(f.t, f.s.auth.EnableMFA(context.Background(), f.user.ID, approvalsTOTPSecret, nil))
	// The management API's 10-request burst is not what these tests are
	// about either.
	rl, _ := f.s.jsonAPIMiddleware()
	rl.mu.Lock()
	rl.rps, rl.burst = rate.Inf, 1000
	rl.mu.Unlock()
	// A paying account: the free tier's one bucket and few keys are not
	// what these tests are about.
	_, err := f.db.Exec(`UPDATE tenant_quotas SET tier = 'standard' WHERE tenant_id = $1`, f.user.TenantID)
	require.NoError(f.t, err)
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM approvals WHERE tenant_id = $1`, f.user.TenantID)
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.user.TenantID)
		_, _ = f.db.Exec(`DELETE FROM user_mfa WHERE user_id = $1`, f.user.ID)
	})
	return f
}

// totpCode is a fresh code; the server accepts each once, so a test that
// approves twice waits for the next step.
func (f *platformFixture) totpCode(after time.Time) string {
	f.t.Helper()
	at := time.Now()
	if !after.IsZero() && at.Unix()/30 <= after.Unix()/30 {
		at = time.Unix((after.Unix()/30+1)*30, 0)
	}
	c, err := totp.GenerateCode(approvalsTOTPSecret, at)
	require.NoError(f.t, err)
	return c
}

func (f *platformFixture) bucket(prefix string) string {
	f.t.Helper()
	name := prefix + "-" + strings.ToLower(uuid.NewString()[:8])
	code, out := f.call(http.MethodPost, "/api/v1/manage/buckets", f.jwt, map[string]any{"name": name})
	require.Equal(f.t, http.StatusCreated, code, out)
	return name
}

func TestAgent_DefaultScopeCannotDeleteABucket(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents().withApprovals()
	_, sec := f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude"})
	name := f.bucket("agdel")

	code, out := f.call(http.MethodDelete, "/api/v1/manage/buckets/"+name, sec.Live, nil)
	require.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "approval_required", errCode(out))
	e := out["error"].(map[string]any)
	id, _ := e["approval_id"].(string)
	require.NotEmpty(t, id)
	assert.Contains(t, e["approval_url"], "/dashboard/agents")
	assert.Contains(t, e["approval_url"], id)

	code, out = f.call(http.MethodDelete, "/api/v1/manage/buckets/"+name, sec.Live, nil)
	require.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, id, out["error"].(map[string]any)["approval_id"], "asking again names the same pending approval")
	var pending int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM approvals WHERE tenant_id = $1 AND decided_at IS NULL`, f.user.TenantID).Scan(&pending))
	assert.Equal(t, 1, pending)

	// The bucket is still there; a server's key (no person behind it, no
	// agent) is the account's own authority, as before.
	code, _ = f.call(http.MethodGet, "/api/v1/manage/buckets/"+name, f.jwt, nil)
	assert.Equal(t, http.StatusOK, code)
	server, _ := f.mgmtKey(nil, auth.ScopeManageWrite)
	code, out = f.call(http.MethodDelete, "/api/v1/manage/buckets/"+name, server, nil)
	assert.Equal(t, http.StatusOK, code, out)

	// Revoking a key it did not mint asks too; its own it revokes freely.
	code, out = f.call(http.MethodPost, "/api/v1/manage/keys", f.jwt, map[string]any{"name": "a person's"})
	require.Equal(t, http.StatusCreated, code, out)
	code, out = f.call(http.MethodDelete, "/api/v1/manage/keys/"+out["id"].(string), sec.Live, nil)
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "approval_required", errCode(out))
	code, out = f.call(http.MethodPost, "/api/v1/manage/keys", sec.Live, map[string]any{"name": "its own"})
	require.Equal(t, http.StatusCreated, code, out)
	code, out = f.call(http.MethodDelete, "/api/v1/manage/keys/"+out["id"].(string), sec.Live, nil)
	assert.Equal(t, http.StatusOK, code, out)
}

func TestAgent_ApprovalGrantsOnce(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents().withApprovals()
	_, sec := f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude"})
	a, b := f.bucket("agone"), f.bucket("agtwo")

	code, out := f.call(http.MethodDelete, "/api/v1/manage/buckets/"+a, sec.Live, nil)
	require.Equal(t, http.StatusForbidden, code, out)
	id := out["error"].(map[string]any)["approval_id"].(string)

	code, out = f.call(http.MethodGet, "/api/v1/manage/approvals", f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	require.Len(t, out["data"], 1)
	assert.Equal(t, "bucket.delete", out["data"].([]any)[0].(map[string]any)["action"])

	// The agent cannot approve itself; a person must, with a fresh code.
	code, out = f.call(http.MethodPost, "/api/v1/manage/approvals/"+id+"/approve", sec.Live, map[string]any{"code": f.totpCode(time.Time{})})
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "person_required", errCode(out))
	code, out = f.call(http.MethodPost, "/api/v1/manage/approvals/"+id+"/approve", f.jwt, map[string]any{})
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "totp_required", errCode(out), "a destructive act needs the authenticator code")
	approvedAt := time.Now()
	code, out = f.call(http.MethodPost, "/api/v1/manage/approvals/"+id+"/approve", f.jwt, map[string]any{"code": f.totpCode(time.Time{})})
	require.Equal(t, http.StatusOK, code, out)

	// One action on one resource: b is still refused.
	code, out = f.call(http.MethodDelete, "/api/v1/manage/buckets/"+b, sec.Live, nil)
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "approval_required", errCode(out))
	// a: once.
	code, out = f.call(http.MethodDelete, "/api/v1/manage/buckets/"+a, sec.Live, nil)
	require.Equal(t, http.StatusOK, code, out)
	var used bool
	require.NoError(t, f.db.QueryRow(`SELECT used_at IS NOT NULL FROM approvals WHERE id = $1`, id).Scan(&used))
	assert.True(t, used)
	code, out = f.call(http.MethodDelete, "/api/v1/manage/buckets/"+a, sec.Live, nil)
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "approval_required", errCode(out), "an approval is used once (asked again before the handler)")

	// Expired after 24 h, approved or not.
	idB := func() string {
		_, out := f.call(http.MethodDelete, "/api/v1/manage/buckets/"+b, sec.Live, nil)
		return out["error"].(map[string]any)["approval_id"].(string)
	}()
	code, out = f.call(http.MethodPost, "/api/v1/manage/approvals/"+idB+"/approve", f.jwt, map[string]any{"code": f.totpCode(approvedAt)})
	require.Equal(t, http.StatusOK, code, out)
	_, err := f.db.Exec(`UPDATE approvals SET expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, idB)
	require.NoError(t, err)
	code, out = f.call(http.MethodDelete, "/api/v1/manage/buckets/"+b, sec.Live, nil)
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "approval_required", errCode(out), "an expired approval grants nothing")
	assert.NotEqual(t, idB, out["error"].(map[string]any)["approval_id"])

	// Denied: refused, and the denial is final.
	idD := out["error"].(map[string]any)["approval_id"].(string)
	code, out = f.call(http.MethodPost, "/api/v1/manage/approvals/"+idD+"/deny", f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	code, out = f.call(http.MethodPost, "/api/v1/manage/approvals/"+idD+"/approve", f.jwt, map[string]any{"code": f.totpCode(time.Now())})
	assert.Equal(t, http.StatusConflict, code, out)
}

func TestAgent_CreatedBucketsAreVersioned(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents().withApprovals()
	_, sec := f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude"})
	name := "agver-" + strings.ToLower(uuid.NewString()[:8])
	code, out := f.call(http.MethodPost, "/api/v1/manage/buckets", sec.Live, map[string]any{"name": name})
	require.Equal(t, http.StatusCreated, code, out)
	assert.Equal(t, "Enabled", out["versioning"])
	assert.Equal(t, "Enabled", getBucketVersioningStatus(context.Background(), f.db, f.user.TenantID, name), "an agent's mistake is a version back")
	person := f.bucket("agperson")
	assert.NotEqual(t, "Enabled", getBucketVersioningStatus(context.Background(), f.db, f.user.TenantID, person), "a person's bucket is as before")
}

func TestAgent_NeverReadsPrivate(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents().withApprovals()
	_, sec := f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude"})
	private := "agpriv-" + strings.ToLower(uuid.NewString()[:8])
	code, out := f.call(http.MethodPost, "/api/v1/manage/buckets", f.jwt, map[string]any{"name": private, "mode": modes.Private})
	require.Equal(t, http.StatusCreated, code, out)
	standard := f.bucket("agstd")

	code, out = f.call(http.MethodGet, "/api/v1/manage/buckets", sec.Live, nil)
	require.Equal(t, http.StatusOK, code, out)
	var names []string
	for _, b := range out["data"].([]any) {
		names = append(names, b.(map[string]any)["name"].(string))
	}
	assert.Contains(t, names, standard)
	assert.NotContains(t, names, private, "a list leaves a Private bucket out")

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/manage/buckets/" + private},
		{http.MethodGet, "/api/v1/manage/buckets/" + private + "/objects"},
		{http.MethodPatch, "/api/v1/manage/buckets/" + private},
		{http.MethodDelete, "/api/v1/manage/buckets/" + private},
	} {
		code, out = f.call(c.method, c.path, sec.Live, map[string]any{"ssec_required": false})
		assert.Equal(t, http.StatusForbidden, code, "%s %s: %v", c.method, c.path, out)
		assert.Equal(t, "private_bucket", errCode(out), "%s %s", c.method, c.path)
	}
	code, _ = f.call(http.MethodGet, "/api/v1/manage/buckets/"+private, f.jwt, nil)
	assert.Equal(t, http.StatusOK, code, "the person reads their own Private bucket's settings")
	// An S3 key the agent mints is the grant's: its scope says so.
	code, out = f.call(http.MethodPost, "/api/v1/manage/keys", sec.Live, map[string]any{"name": "agent s3"})
	require.Equal(t, http.StatusCreated, code, out)
	cred, err := auth.NewAuth(f.db, zap.NewNop(), nil).LookupCredential(context.Background(), out["key"].(string))
	require.NoError(t, err)
	assert.NotEmpty(t, cred.Scope.AgentGrantID, "the S3 path knows the key is an agent's")
}
