package api

// RP90-1 (Stage 1a Prompt 56 Part 0): the agent's versioning promise holds
// on EVERY door (F-41: "a mistake is a version back"), not only on the
// management API. An S3 key an agent grant minted makes versioned buckets,
// never deletes a version for good, and never deletes from a bucket whose
// versioning is not Enabled — the person keeps those (D-STRIPE-85 is the
// owner's; until then this rule).

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- Content-MD5 of a test body, the S3 wire format
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
)

// agentS3Key mints an agent grant on the fixture's tenant and an S3 key of
// that grant (what POST /api/v1/manage/keys does for an agent): the
// ceiling's permissions and agent_grant_id. It answers the grant and the
// key pair.
func (f *edgeFixture) agentS3Key() (grant *auth.AgentGrant, ak, sk string) {
	f.t.Helper()
	g, _, err := auth.MintAgentGrant(context.Background(), f.db, f.tenantID, auth.AgentGrantOptions{AgentName: "Claude", GrantedBy: f.userID})
	require.NoError(f.t, err)
	// An agent's S3 key works only while the account's flag is on (RP90-13).
	require.NoError(f.t, f.fl.Set(context.Background(), flagAgentGrants, f.tenantID, true, "test"))
	require.NoError(f.t, f.fl.Refresh(context.Background()))
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM api_keys WHERE agent_grant_id = $1::uuid`, g.ID)
		_, _ = f.db.Exec(`DELETE FROM agent_grants WHERE id = $1::uuid`, g.ID)
		_, _ = f.db.Exec(`DELETE FROM management_keys WHERE tenant_id = $1 OR tenant_id IN (SELECT id FROM tenants WHERE sandbox_of = $1)`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM tenants WHERE sandbox_of = $1`, f.tenantID)
	})
	perms, err := json.Marshal(auth.AgentS3Permissions)
	require.NoError(f.t, err)
	id := uuid.NewString()
	ak = "VKAGENT" + strings.ToUpper(id[:8])
	sk = "SKAGENT" + id
	f.exec(`INSERT INTO api_keys (user_id, tenant_id, name, key_id, secret_hash, secret_key, permissions, agent_grant_id)
		VALUES ($1, $2, 'agent s3', $3, 'h', $4, $5::jsonb, $6::uuid)`, f.userID, f.tenantID, ak, sk, string(perms), g.ID)
	return g, ak, sk
}

// signedAs signs method path?query with body under the key pair (any
// method: the fixture's signedHTTP drops a non-PUT body).
func (f *edgeFixture) signedAs(ak, sk, method, path string, query url.Values, body []byte, hdr map[string]string) *http.Request {
	f.t.Helper()
	u := "https://" + f.host + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	r, err := http.NewRequest(method, u, bytes.NewReader(body))
	require.NoError(f.t, err)
	r.ContentLength = int64(len(body))
	if len(body) == 0 {
		r.Body = http.NoBody
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	require.NoError(f.t, v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: ak, SecretAccessKey: sk},
		r, payloadHash, "s3", "us-east-1", time.Now()))
	return r
}

func deleteObjectsBody(entries ...[2]string) ([]byte, map[string]string) {
	var b strings.Builder
	b.WriteString(`<Delete>`)
	for _, e := range entries {
		b.WriteString(`<Object><Key>` + e[0] + `</Key>`)
		if e[1] != "" {
			b.WriteString(`<VersionId>` + e[1] + `</VersionId>`)
		}
		b.WriteString(`</Object>`)
	}
	b.WriteString(`</Delete>`)
	body := []byte(b.String())
	sum := md5.Sum(body) // #nosec G401 -- the S3 Content-MD5 header
	return body, map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(sum[:]), "Content-Type": "application/xml"}
}

func TestAgent_S3CreateBucketIsVersioned(t *testing.T) {
	f := setupEdgeFixture(t)
	_, ak, sk := f.agentS3Key()

	name := "agents3-" + strings.ToLower(uuid.NewString()[:8])
	w := f.serve(f.signedAs(ak, sk, http.MethodPut, "/"+name, nil, nil, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "Enabled", getBucketVersioningStatus(context.Background(), f.db, f.tenantID, name),
		"an agent's S3 CreateBucket is versioned in the same statement, as the management API's (F-41)")

	person := "persons3-" + strings.ToLower(uuid.NewString()[:8])
	w = f.serve(f.signedAs(f.ak, f.sk, http.MethodPut, "/"+person, nil, nil, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotEqual(t, "Enabled", getBucketVersioningStatus(context.Background(), f.db, f.tenantID, person), "a person's bucket is as before")
}

func TestAgent_NeverDeletesForGood(t *testing.T) {
	f := setupEdgeFixture(t)
	_, ak, sk := f.agentS3Key()

	// The person's own UNVERSIONED bucket (f.bucket): the agent writes, never deletes.
	w := f.serve(f.signedAs(ak, sk, http.MethodPut, "/"+f.bucket+"/a.txt", nil, []byte("hello"), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = f.serve(f.signedAs(ak, sk, http.MethodDelete, "/"+f.bucket+"/a.txt", nil, nil, nil))
	assert.Equal(t, http.StatusForbidden, w.Code, "a delete on an unversioned bucket is for good: the person's")
	assert.Contains(t, w.Body.String(), "AccessDenied")
	assert.Contains(t, w.Body.String(), "account holder", "the refusal says who can do it")
	body, hdr := deleteObjectsBody([2]string{"a.txt", ""})
	w = f.serve(f.signedAs(ak, sk, http.MethodPost, "/"+f.bucket, url.Values{"delete": {""}}, body, hdr))
	assert.Equal(t, http.StatusForbidden, w.Code, "the batch is the same delete: %s", w.Body.String())
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'a.txt'`,
		f.tenantID, f.bucket).Scan(&n))
	assert.Equal(t, 1, n, "nothing was deleted")

	// An agent's own (versioned) bucket: a plain delete is a delete marker — a version back.
	vb := "agentv-" + strings.ToLower(uuid.NewString()[:8])
	w = f.serve(f.signedAs(ak, sk, http.MethodPut, "/"+vb, nil, nil, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "Enabled", getBucketVersioningStatus(context.Background(), f.db, f.tenantID, vb))
	w = f.serve(f.signedAs(ak, sk, http.MethodPut, "/"+vb+"/b.txt", nil, []byte("v1"), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	versionID := w.Header().Get("x-amz-version-id")
	require.NotEmpty(t, versionID)

	w = f.serve(f.signedAs(ak, sk, http.MethodDelete, "/"+vb+"/b.txt", url.Values{"versionId": {versionID}}, nil, nil))
	assert.Equal(t, http.StatusForbidden, w.Code, "DELETE ?versionId is for good: never an agent's")
	body, hdr = deleteObjectsBody([2]string{"b.txt", versionID})
	w = f.serve(f.signedAs(ak, sk, http.MethodPost, "/"+vb, url.Values{"delete": {""}}, body, hdr))
	assert.Equal(t, http.StatusForbidden, w.Code, "DeleteObjects with a version id is for good: %s", w.Body.String())
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM object_versions WHERE tenant_id = $1 AND bucket = $2 AND version_id = $3`,
		f.tenantID, vb, versionID).Scan(&n))
	assert.Equal(t, 1, n, "the version is still there")

	w = f.serve(f.signedAs(ak, sk, http.MethodDelete, "/"+vb+"/b.txt", nil, nil, nil))
	assert.Equal(t, http.StatusNoContent, w.Code, "a plain delete on a versioned bucket leaves the version: %s", w.Body.String())
	assert.Equal(t, "true", w.Header().Get("x-amz-delete-marker"))

	// The person deletes for good with their own key.
	w = f.serve(f.signedAs(f.ak, f.sk, http.MethodDelete, "/"+f.bucket+"/a.txt", nil, nil, nil))
	assert.Equal(t, http.StatusNoContent, w.Code, fmt.Sprintf("the person's key: %s", w.Body.String()))
}
