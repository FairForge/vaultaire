package api

// RP90-6 + RP90-18 (Stage 1a Prompt 56 Part 0): an approval is bound to the
// resource's identity — a bucket's creation, a key's id — read when the
// agent asks and checked when it spends: approving "delete ghost" never
// deletes a ghost created afterwards. An ask for a resource that does not
// exist is a 404 and writes no row; a grant holds at most 20 pending asks.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
)

func TestApproval_BoundToTheBucketNotItsName(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents().withApprovals()
	g, sec := f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude"})
	pending := func() int {
		var n int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM approvals WHERE grant_id = $1::uuid AND decided_at IS NULL`, g.ID).Scan(&n))
		return n
	}

	// No such bucket, no such key: a 404, and no row.
	ghost := "ghost-" + strings.ToLower(uuid.NewString()[:8])
	code, out := f.call(http.MethodDelete, "/api/v1/manage/buckets/"+ghost, sec.Live, nil)
	assert.Equal(t, http.StatusNotFound, code, out)
	code, out = f.call(http.MethodDelete, "/api/v1/manage/keys/"+uuid.NewString(), sec.Live, nil)
	assert.Equal(t, http.StatusNotFound, code, out)
	assert.Zero(t, pending(), "an ask for nothing writes nothing (RP90-18)")

	// Approved for THIS bucket; the bucket is deleted and re-created by its
	// person: the approval does not reach the new one.
	name := f.bucket("bound")
	code, out = f.call(http.MethodDelete, "/api/v1/manage/buckets/"+name, sec.Live, nil)
	require.Equal(t, http.StatusForbidden, code, out)
	id := out["error"].(map[string]any)["approval_id"].(string)
	code, out = f.call(http.MethodPost, "/api/v1/manage/approvals/"+id+"/approve", f.jwt, map[string]any{"code": f.totpCode(time.Time{})})
	require.Equal(t, http.StatusOK, code, out)
	code, out = f.call(http.MethodDelete, "/api/v1/manage/buckets/"+name, f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	time.Sleep(2 * time.Millisecond) // a new created_at
	code, out = f.call(http.MethodPost, "/api/v1/manage/buckets", f.jwt, map[string]any{"name": name})
	require.Equal(t, http.StatusCreated, code, out)
	code, out = f.call(http.MethodDelete, "/api/v1/manage/buckets/"+name, sec.Live, nil)
	assert.Equal(t, http.StatusForbidden, code, "an approval of the old bucket never deletes the new one: %v", out)
	assert.Equal(t, "approval_required", errCode(out))
	assert.NotEqual(t, id, out["error"].(map[string]any)["approval_id"], "a new ask, for the new bucket")
	code, _ = f.call(http.MethodGet, "/api/v1/manage/buckets/"+name, f.jwt, nil)
	assert.Equal(t, http.StatusOK, code, "the new bucket is still there")

	// At most 20 pending asks per grant.
	for i := pending(); i < maxPendingApprovalsPerGrant; i++ {
		_, err := f.db.Exec(`INSERT INTO approvals (tenant_id, grant_id, action, resource, resource_ref) VALUES ($1, $2::uuid, 'bucket.delete', $3, 'x')`,
			f.user.TenantID, g.ID, fmt.Sprintf("bucket:filler-%d", i))
		require.NoError(t, err)
	}
	other := f.bucket("over")
	code, out = f.call(http.MethodDelete, "/api/v1/manage/buckets/"+other, sec.Live, nil)
	assert.Equal(t, http.StatusTooManyRequests, code, "the 21st pending ask: %v", out)
	assert.Equal(t, "too_many_pending_approvals", errCode(out))
	assert.Equal(t, maxPendingApprovalsPerGrant, pending())
}
