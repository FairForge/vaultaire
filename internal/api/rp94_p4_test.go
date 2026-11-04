package api

// RP94-7 … RP94-16 (Stage 1a Prompt 58 Part 0, the P4s of the review of
// WP-POST-1).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/payments"
)

// RP94-7: an unknown access key in a policy is InvalidAccessKeyId (AWS),
// never AccessDenied.
func TestPostPolicy_AnUnknownKeyIsInvalidAccessKeyId(t *testing.T) {
	f := setupEdgeFixture(t)
	conds := []any{map[string]any{"bucket": f.bucket}, []any{"starts-with", "$key", "in/"}, []any{"content-length-range", 1, 1000}}
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "in/u.bin"})
	for i := range fields {
		if fields[i].name == "x-amz-credential" {
			fields[i].value = strings.Replace(fields[i].value, f.ak, "AKIANOSUCHKEY0000000", 1)
		}
	}
	body, ct := buildPostForm(t, fields, "u.bin", []byte("x"))
	w := f.post(f.bucket, body, ct)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, ErrInvalidAccessKeyId, postErrCode(t, w))
}

// RP94-8: aws-chunked is transport framing, never an object attribute: a
// form's Content-Encoding naming it is refused before any byte is stored.
func TestPostPolicy_AWSChunkedContentEncodingIsRefused(t *testing.T) {
	f := setupEdgeFixture(t)
	conds := []any{map[string]any{"bucket": f.bucket}, []any{"starts-with", "$key", "in/"}, []any{"content-length-range", 1, 1000},
		[]any{"starts-with", "$Content-Encoding", ""}}
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "in/c.bin"}, formField{"Content-Encoding", "gzip, aws-chunked"})
	before := f.storageUsed()
	body, ct := buildPostForm(t, fields, "c.bin", []byte("chunky"))
	w := f.post(f.bucket, body, ct)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, ErrInvalidArgument, postErrCode(t, w))
	assert.Contains(t, w.Body.String(), "aws-chunked")
	_, _, _, ok := f.headRow("in/c.bin")
	assert.False(t, ok)
	assert.Equal(t, before, f.storageUsed())
}

// RP94-9: a POST is the handler's even with data_plane_slc on — the
// divergence is documented in internal/api/CLAUDE.md (the slc host
// re-describes a SigV4 request; a form's credential is a field).
func TestPostPolicy_DataPlaneSLCLeavesFormsToTheHandler(t *testing.T) {
	f := setupEdgeFixture(t)
	f.srv.dataPlaneTransport = f.drv
	require.NoError(t, f.fl.Set(context.Background(), flagDataPlaneSLC, f.tenantID, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	conds := []any{map[string]any{"bucket": f.bucket}, []any{"starts-with", "$key", "in/"}, []any{"content-length-range", 1, 1000}}
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "in/slc.bin"})
	body, ct := buildPostForm(t, fields, "slc.bin", []byte("slc form"))
	w := f.post(f.bucket, body, ct)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.Equal(t, 0, countRows(t, f.db, `SELECT COUNT(*) FROM edge_admits WHERE tenant_id = $1 AND object_key = 'in/slc.bin'`, f.tenantID),
		"no host=slc admit: the handler stored it")
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "api", "CLAUDE.md"))
	require.NoError(t, err)
	doc := string(raw)
	assert.Contains(t, doc, "RP94-9", "the divergence is written down")
}

// RP94-10: the edge admit bounds the fields before the file as the origin
// does (maxPostPreData): a form the origin refuses is refused at the edge.
func TestPostPolicy_EdgeRefusesFieldsPastTheOriginsBound(t *testing.T) {
	f := setupEdgeFixture(t)
	conds := []any{map[string]any{"bucket": f.bucket}, []any{"starts-with", "$key", "in/"}, []any{"content-length-range", 1, 10 << 20}}
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "in/${filename}"})
	req := f.postAdmit(fields, "big-fields.png", 8192)
	req.Form.PreBytes = maxPostPreData + 2048
	resp := f.admit(req)
	require.NotNil(t, resp.Refusal, "%+v", resp)
	assert.Equal(t, http.StatusBadRequest, resp.Refusal.Status)
	assert.Contains(t, resp.Refusal.Body, ErrInvalidArgument)
	assert.Contains(t, resp.Refusal.Body, "exceed")
}

// RP94-12: the bytes counter's Help says a committed form counts under
// op="put" (the admits count it under op="post").
func TestEdge_DataPlaneBytesHelpNamesForms(t *testing.T) {
	ch := make(chan *prometheus.Desc, 1)
	dataPlaneBytes.Describe(ch)
	d := (<-ch).String()
	assert.Contains(t, d, "op=put")
	assert.Contains(t, d, "POST policy form")
}

// RP94-13 (the count half): a tenant's pending MEMBER count is member
// invitations only — family invitations do not fill the member limit. (The
// delete half — one kind's invite replacing the other's — is held by
// migration 120's idx_tenant_invitations_one_pending, one pending row per
// address of any kind: an open question of the closeout, a migration.)
func TestInvitations_FamilyInvitationsDoNotFillTheMemberLimit(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()
	ctx := context.Background()
	addr := "rp94-13-" + uuid.NewString()[:8] + "@family.example"
	for i := 0; i < auth.MaxPendingInvitations; i++ {
		_, err := f.db.Exec(`INSERT INTO tenant_invitations (id, tenant_id, kind, email, role, token_hash, expires_at, created_at)
			VALUES ($1, $2, 'family', $3, 'viewer', $4, NOW() + interval '7 days', NOW())`,
			uuid.NewString(), f.user.TenantID, fmt.Sprintf("fam-%d-%s", i, addr), uuid.NewString())
		require.NoError(t, err)
	}
	_, _, err := auth.CreateInvitation(ctx, f.db, f.user.TenantID, f.user.ID, "member-"+addr, auth.RoleViewer, time.Now())
	assert.NoError(t, err, "pending FAMILY invitations do not count against the member limit")
}

// RP94-15: the reconcile's drift query skips a sponsored member — its heal
// (SetHouseTx) is refused for ever, a daily Warn and a permanent note.
func TestEntitlementLapse_DriftSkipsASponsoredMember(t *testing.T) {
	db := setupTestDBFixed(t)
	org, child := "rp94-15-org-"+uuid.NewString()[:8], "rp94-15-kid-"+uuid.NewString()[:8]
	for _, id := range []string{org, child} {
		_, err := db.Exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'x', $2)`, id, id+"@test.local")
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, tier) VALUES ($1, 0, 'free')`, id)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, child)
		_, _ = db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id IN ($1, $2)`, org, child)
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id IN ($1, $2)`, org, child)
		_, _ = db.Exec(`UPDATE tenants SET parent_tenant_id = NULL, child_kind = NULL, pool_root_id = id WHERE id = $1`, child)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id IN ($1, $2)`, child, org)
	})
	_, err := db.Exec(`UPDATE tenants SET parent_tenant_id = $2, child_kind = 'sponsored', pool_root_id = $2 WHERE id = $1`, child, org)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO entitlements (tenant_id, std_tb, period, paid_through, renewal, provider)
		VALUES ($1, 1, 'monthly', NOW() + interval '20 days', 'auto', 'stripe')`, child)
	require.NoError(t, err)

	r := NewEntitlementLapseRunner(db, nil, zap.NewNop(), 14*24*time.Hour)
	r.onlyTenants = func(id string) bool { return id == child }
	res, err := r.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Empty(t, res.Errors, "no heal attempted on a sponsored member")
	assert.Zero(t, res.Healed)
}

// RP94-16: the admin writers answer a sponsored member's refusal 409
// in_a_family, never 400/500.
func TestAdminPayments_InAFamilyIs409(t *testing.T) {
	w := httptest.NewRecorder()
	adminMoneyError(w, fmt.Errorf("record a manual payment: %w", payments.ErrInAFamily))
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "in_a_family")
}
