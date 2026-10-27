package api

// Stage 1a Prompt 47 Part 0: the RP72 fixes (docs/reviews/WP-EDGE-2C-REVIEW.md).

import (
	"crypto/md5" // #nosec G501 -- the S3 ETag
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/drivers"
)

// RP72-1 (now): the If-Match rule takes the single-PUT cutoff that was in
// force when the row was written. Before the #1011 deploy the origin's
// uploader cut 16 MiB parts, so a 20 MiB row of then is a backend multipart
// whose -N the row does not know: no If-Match (it was the MD5 → 412 → the
// origin, on every read, for ever). A row with the backend's own ETag
// recorded (backend_etag, migration 112) signs that, whatever its size or
// age.
func TestEdgeGetIfMatch_TheCutoffInForceWhenTheRowWasWritten(t *testing.T) {
	md5hex := "0123456789abcdef0123456789abcdef"
	before := s3SinglePutCutoffBefore.Add(-time.Hour)
	after := s3SinglePutCutoffBefore.Add(time.Hour)

	twenty := int64(20 << 20)
	assert.Equal(t, "", edgeGetIfMatch(md5hex, "", "", twenty, before), "a 20 MiB row of before #1011: a backend multipart (16 MiB parts)")
	assert.Equal(t, `"`+md5hex+`"`, edgeGetIfMatch(md5hex, "", "", twenty, after), "a 20 MiB row of after: one PutObject")
	assert.Equal(t, `"`+md5hex+`"`, edgeGetIfMatch(md5hex, "", "", 16<<20, before), "16 MiB was one PutObject then too")
	assert.Equal(t, "", edgeGetIfMatch(md5hex, "", "", drivers.S3SinglePutMaxBytes+1, after))

	// The recorded backend ETag wins, whatever the size or the age.
	assert.Equal(t, `"aaaa-7"`, edgeGetIfMatch(md5hex, "", "aaaa-7", 1<<30, before))
	assert.Equal(t, `"`+md5hex+`"`, edgeGetIfMatch(md5hex, "", `"`+md5hex+`"`, twenty, before), "quotes on the recorded value are normalised")
	// An edge-assembled -N with no body_md5 is the backend's own, as before.
	assert.Equal(t, `"cccc-4"`, edgeGetIfMatch("cccc-4", "", "", 1<<30, before))
}

// RP72-1 (properly): every mover commit and origin write records the
// backend's ETag on the head row (object_head_cache.backend_etag), and an
// edge read's link signs If-Match from it.
func TestBackendETag_RecordedByMoverAndOriginWritesAndSigned(t *testing.T) {
	f := setupEdgeFixture(t)
	backendETag := func(key string) string {
		t.Helper()
		var v string
		require.NoError(t, f.db.QueryRow(`SELECT COALESCE(backend_etag, '') FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			f.tenantID, f.bucket, key).Scan(&v))
		return v
	}

	// An origin PUT: the driver's answer (the fake reports the body's MD5).
	body := testBytes(5000)
	sum := md5.Sum(body) // #nosec G401
	f.putObject("origin.bin", body, nil)
	assert.Equal(t, hex.EncodeToString(sum[:]), backendETag("origin.bin"), "the origin's write records the driver's answer")

	// A mover PUT: the backend's ETag the Worker reported.
	resp := f.admit(f.signed("/"+f.bucket+"/moved.bin", nil, body, nil, ""))
	require.True(t, resp.Edge, "%+v", resp)
	etag := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	relay, _ := f.commit(okCommit(resp.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	assert.Equal(t, hex.EncodeToString(sum[:]), backendETag("moved.bin"))

	// An edge Complete: the backend's own -N.
	id := f.createUpload("mp.zip")
	_, e1 := f.edgePart("mp.zip", id, 1, testBytes(6000))
	_, e2 := f.edgePart("mp.zip", id, 2, testBytes(7000))
	completeXML := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, e1, e2)
	wc := f.s3(http.MethodPost, "/"+f.bucket+"/mp.zip?uploadId="+id, []byte(completeXML), nil)
	require.Equal(t, http.StatusOK, wc.Code, wc.Body.String())
	h, ok := f.head("mp.zip")
	require.True(t, ok)
	assert.Equal(t, h.etag, backendETag("mp.zip"), "the backend's Complete answer")
	assert.Contains(t, backendETag("mp.zip"), "-2")

	// The read signs the recorded value — even where the size rule would
	// sign nothing (a large row of before #1011).
	f.exec(`UPDATE object_head_cache SET backend_etag = 'abcdabcdabcdabcdabcdabcdabcdabcd-9', size_bytes = $2, updated_at = $3
		WHERE tenant_id = $1 AND object_key = 'origin.bin'`, f.tenantID, int64(1<<30), s3SinglePutCutoffBefore.Add(-time.Hour))
	g := f.admit(f.signedGet("/"+f.bucket+"/origin.bin", nil, nil))
	require.True(t, g.Edge, "%+v", g)
	assert.Equal(t, `"abcdabcdabcdabcdabcdabcdabcdabcd-9"`, g.SendHeaders["if-match"])
	u, err := url.Parse(g.URL)
	require.NoError(t, err)
	assert.Equal(t, `"abcdabcdabcdabcdabcdabcdabcdabcd-9"`, u.Query().Get("x-probe-if-match"), "signed into the link")

	// A row of before #1011 with no recorded value, 20 MiB: no If-Match.
	f.exec(`UPDATE object_head_cache SET backend_etag = NULL, size_bytes = $2, updated_at = $3
		WHERE tenant_id = $1 AND object_key = 'origin.bin'`, f.tenantID, int64(20<<20), s3SinglePutCutoffBefore.Add(-time.Hour))
	g = f.admit(f.signedGet("/"+f.bucket+"/origin.bin", nil, nil))
	require.True(t, g.Edge, "%+v", g)
	assert.Empty(t, g.SendHeaders["if-match"], "a backend multipart of then: the address is the guard")
}
