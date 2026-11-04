package api

// WP-POST-1 PR B (Stage 1a Prompt 57): a browser's POST policy upload is
// admitted at the edge like a PUT. The host (the Worker on s3.stored.cloud)
// reads the form up to its file part and describes it; the control plane
// verifies the policy (the one verifier's fourth entrance), builds the same
// inner PUT the origin would, and admits it by the file's exact size within
// the policy's signed content-length-range — op 'put' with attrs.via
// 'post' (edge_admits.op's CHECK is unchanged). The commit answers as the
// origin's POST would: success_action_status, Location, PostResponse.

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/dataplane"
)

func (f *edgeFixture) postAdmit(fields []formField, filename string, size int64) edgeAdmitRequest {
	f.t.Helper()
	m := map[string]string{}
	for _, fl := range fields {
		m[strings.ToLower(fl.name)] = fl.value
	}
	return edgeAdmitRequest{Op: "post", Method: http.MethodPost, Host: f.host, Path: "/" + f.bucket,
		Headers:  map[string]string{"host": f.host, "content-type": "multipart/form-data; boundary=x", "user-agent": "a-browser"},
		ClientIP: "203.0.113.9", RequestID: "post-" + filename, Features: []string{dataplane.FeaturePostTail},
		Form: &dataplane.AdmitForm{Fields: m, Filename: filename, FileSize: size, PreBytes: 1200}}
}

func TestPostPolicy_EdgeAdmitsTheSignedSize(t *testing.T) {
	f := setupEdgeFixture(t)
	tr := NewS3AccessLogTracker(f.db)
	tr.SetRequestLog(func(string) bool { return true })
	f.srv.accessLogTracker = tr
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM request_log WHERE tenant_id = $1`, f.tenantID) })
	conds := func(max int) []any {
		return []any{map[string]any{"bucket": f.bucket}, []any{"starts-with", "$key", "in/"}, []any{"starts-with", "$Content-Type", "image/"},
			map[string]any{"success_action_status": "201"}, []any{"content-length-range", 1, max}}
	}
	form := func(max int) []formField {
		return f.policyFields(time.Now().Add(time.Hour), conds(max), formField{"key", "in/${filename}"}, formField{"Content-Type", "image/png"},
			formField{"success_action_status", "201"})
	}

	body := testBytes(8192)
	resp := f.admit(f.postAdmit(form(10<<20), "cat.png", int64(len(body))))
	require.True(t, resp.Edge, "%+v", resp)
	require.NotEmpty(t, resp.URL)
	assert.Equal(t, http.MethodPut, resp.Method, "the file part goes to a backend PUT")
	assert.Nil(t, resp.Codec, "a form's file is moved whole")
	var op, via string
	var length int64
	require.NoError(t, f.db.QueryRow(`SELECT op, attrs->>'via', content_length FROM edge_admits WHERE id = $1`, resp.AdmitID).Scan(&op, &via, &length))
	assert.Equal(t, "put", op, "edge_admits.op's CHECK stays put|part|get")
	assert.Equal(t, "post", via)
	assert.EqualValues(t, len(body), length, "the link signs the file part's exact size")

	etag := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	relay, code := f.commit(okCommit(resp.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, http.StatusCreated, relay.Status, relay.Body)
	assert.Contains(t, relay.Body, "<Key>in/cat.png</Key>")
	var pr PostResponse
	require.NoError(t, xml.Unmarshal([]byte(relay.Body), &pr), relay.Body)
	assert.Equal(t, etag, pr.ETag, "the backend's ETag: the file part's MD5")
	assert.True(t, strings.HasSuffix(relay.Headers["Location"], "/"+f.bucket+"/in/cat.png"), relay.Headers["Location"])
	got, ok := f.head("in/cat.png")
	require.True(t, ok, "the head row, as a PUT's")
	assert.Equal(t, "image/png", got.ct)
	tr.Flush()
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM request_log WHERE tenant_id = $1 AND op = 'PostObject'`, f.tenantID),
		"the access log names the POST")

	// A policy whose signed max is past the Worker's body cap (less the
	// fields) is the origin's: the edge never admits what it cannot carry.
	resp = f.admit(f.postAdmit(form(200<<20), "big.png", 8192))
	assert.False(t, resp.Edge)
	assert.Equal(t, "post_over_edge_cap", resp.Reason)

	// The file larger than the policy allows: the origin's refusal, relayed.
	resp = f.admit(f.postAdmit(form(4096), "over.png", 8192))
	require.NotNil(t, resp.Refusal, "%+v", resp)
	assert.Equal(t, http.StatusBadRequest, resp.Refusal.Status)
	assert.Contains(t, resp.Refusal.Body, "EntityTooLarge")

	// A bad signature: the origin's refusal, relayed.
	bad := form(10 << 20)
	for i := range bad {
		if bad[i].name == "x-amz-signature" {
			bad[i].value = strings.Repeat("0", 64)
		}
	}
	resp = f.admit(f.postAdmit(bad, "x.png", 8192))
	require.NotNil(t, resp.Refusal)
	assert.Equal(t, http.StatusForbidden, resp.Refusal.Status)
	assert.Contains(t, resp.Refusal.Body, "SignatureDoesNotMatch")
	_, ok = f.head("in/x.png")
	assert.False(t, ok)
}
