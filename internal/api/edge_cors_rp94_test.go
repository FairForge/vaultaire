package api

// RP94-3 (the review of WP-POST-1): the edge's answers carried no CORS
// headers. A page on the customer's origin doing fetch(url, {method:
// 'POST', body: formData}) to s3.stored.cloud had its upload STORED and got
// an opaque network error — the browser will not let a page read a 201
// without Access-Control-Allow-Origin. The origin's answers carry the
// bucket's rule (applyS3CORSForTenant) and a refusal before the tenant is
// known the pre-auth Allow-Origin (applyS3CORSPreAuth); the edge's now do
// too — the admit answers them, the Worker relays every header it is given.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const corsPageOrigin = "https://page.example"

func (f *edgeFixture) setBucketCORS() {
	f.t.Helper()
	f.exec(`UPDATE buckets SET cors_rules = $3 WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket,
		`[{"allowed_origins":["https://page.example"],"allowed_methods":["GET","PUT","POST"],"expose_headers":["ETag","Location"]}]`)
}

func TestEdge_AnswersCarryTheBucketsCORSHeaders(t *testing.T) {
	f := setupEdgeFixture(t)
	f.setBucketCORS()
	conds := []any{map[string]any{"bucket": f.bucket}, []any{"starts-with", "$key", "in/"},
		map[string]any{"success_action_status": "201"}, []any{"content-length-range", 1, 10 << 20}}
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "in/${filename}"}, formField{"success_action_status", "201"})

	// A form from the page: the commit's 201 carries the bucket's rule.
	body := testBytes(8192)
	req := f.postAdmit(fields, "page.png", int64(len(body)))
	req.Headers["origin"] = corsPageOrigin
	resp := f.admit(req)
	require.True(t, resp.Edge, "%+v", resp)
	etag := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	relay, code := f.commit(okCommit(resp.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, http.StatusCreated, relay.Status, relay.Body)
	assert.Equal(t, corsPageOrigin, relay.Headers["Access-Control-Allow-Origin"], "%v", relay.Headers)
	assert.Equal(t, "ETag, Location", relay.Headers["Access-Control-Expose-Headers"])
	assert.Contains(t, relay.Headers["Vary"], "Origin")
	// A retried commit replays the same answer, headers and all.
	again, _ := f.commit(okCommit(resp.AdmitID, body, etag))
	assert.Equal(t, corsPageOrigin, again.Headers["Access-Control-Allow-Origin"])

	// A refused form (a bad signature, before any tenant is known): the
	// pre-auth Allow-Origin, as the origin answers it.
	bad := append([]formField(nil), fields...)
	for i := range bad {
		if bad[i].name == "x-amz-signature" {
			bad[i].value = strings.Repeat("0", 64)
		}
	}
	req = f.postAdmit(bad, "bad.png", 8192)
	req.Headers["origin"] = corsPageOrigin
	resp = f.admit(req)
	require.NotNil(t, resp.Refusal, "%+v", resp)
	assert.Equal(t, http.StatusForbidden, resp.Refusal.Status)
	assert.Equal(t, corsPageOrigin, resp.Refusal.Headers["Access-Control-Allow-Origin"], "%v", resp.Refusal.Headers)

	// An origin no rule allows: nothing (and nothing for a plain client).
	req = f.postAdmit(bad, "bad.png", 8192)
	req.Headers["origin"] = "https://elsewhere.example"
	resp = f.admit(req)
	require.NotNil(t, resp.Refusal)
	assert.Empty(t, resp.Refusal.Headers["Access-Control-Allow-Origin"])

	// A SigV4-header PUT from browser JavaScript: the same.
	put := testBytes(8 << 20)
	preq := f.signed("/"+f.bucket+"/js.bin", nil, put, map[string]string{"Origin": corsPageOrigin}, "")
	resp = f.admit(preq)
	require.True(t, resp.Edge, "%+v", resp)
	etag = f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, put)
	relay, code = f.commit(okCommit(resp.AdmitID, put, etag))
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	assert.Equal(t, corsPageOrigin, relay.Headers["Access-Control-Allow-Origin"], "%v", relay.Headers)

	// And a GET: the response headers the Worker answers with.
	greq := f.signedGet("/"+f.bucket+"/js.bin", nil, map[string]string{"Origin": corsPageOrigin})
	resp = f.admit(greq)
	require.True(t, resp.Edge, "%+v", resp)
	assert.Equal(t, corsPageOrigin, resp.ResponseHeaders["Access-Control-Allow-Origin"], "%v", resp.ResponseHeaders)
	assert.Equal(t, "ETag, Location", resp.ResponseHeaders["Access-Control-Expose-Headers"])
}
