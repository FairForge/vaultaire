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

	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
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

// RP72-2: a header-signed aws-chunked write's length is the backend's to
// enforce from x-amz-decoded-content-length — and Lyve does not (the probe
// of 2026-10-27, edge/s3-ingest/lab/declenprobe.go: one byte more or less
// declared, 200 and the bytes sent stored; iDrive 400 IncompleteBody / 503).
// Where EnforcesDecodedLength is false the mover's PUT commit HEADs the blob
// and refuses a size that is not the admitted one (IncompleteBody,
// everything given back, the blob removed); where it is true, no HEAD.
func TestEdgeCommit_AWSChunkedLengthIsCheckedWhereTheBackendDoesNotEnforceIt(t *testing.T) {
	f := setupEdgeFixture(t)
	f.drv.streaming = engine.EdgeStreamingProps{Supported: true, SignatureMaxAge: 15 * time.Minute} // Lyve's: no decoded-length enforcement
	const n = 5 << 20
	path := "/" + f.bucket + "/short.bin"

	// A body one byte short of the decoded length the client signed.
	resp := f.admit(f.signedStreaming(path, nil, n, "x-amz-checksum-crc64nvme", ""))
	require.True(t, resp.Edge, "%+v", resp)
	usedAfterAdmit := f.used()
	short := testBytes(n - 1)
	etag := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, short)
	heads := f.drv.heads.Load()
	relay, _ := f.commit(okCommit(resp.AdmitID, short, etag))
	require.NotNil(t, relay)
	assert.Equal(t, http.StatusBadRequest, relay.Status, relay.Body)
	assert.Contains(t, relay.Body, "IncompleteBody")
	assert.Equal(t, heads+1, f.drv.heads.Load(), "the commit HEADs the blob")
	_, ok := f.head("short.bin")
	assert.False(t, ok, "nothing committed")
	assert.Less(t, f.used(), usedAfterAdmit, "the reservation given back")
	time.Sleep(100 * time.Millisecond)
	assert.False(t, f.drv.has(f.tenantID, f.container(), resp.StorageKey), "the blob removed")

	// The right length commits (one HEAD).
	resp = f.admit(f.signedStreaming(path, nil, n, "x-amz-checksum-crc64nvme", ""))
	body := testBytes(n)
	etag = f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	relay, _ = f.commit(okCommit(resp.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)

	// A backend that enforces it (iDrive): no HEAD at commit.
	f.drv.streaming = engine.EdgeStreamingProps{Supported: true, VerifiesTrailer: true, EnforcesDecodedLength: true, SignatureMaxAge: 15 * time.Minute}
	resp = f.admit(f.signedStreaming("/"+f.bucket+"/enforced.bin", nil, n, "x-amz-checksum-crc64nvme", ""))
	require.True(t, resp.Edge, "%+v", resp)
	etag = f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	heads = f.drv.heads.Load()
	relay, _ = f.commit(okCommit(resp.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	assert.Equal(t, heads, f.drv.heads.Load(), "the backend enforced the length: nothing to HEAD")
}

// RP72-2, the parts: a part cannot be HEADed, so an edge upload's Complete
// on a backend that does not enforce the decoded length HEADs the ASSEMBLED
// object and refuses a size that is not the parts' recorded sum —
// IncompleteBody, the upload ended with its holding released, the blob
// retired, no head row.
func TestEdgeComplete_AssembledSizeIsCheckedWhereTheBackendDoesNotEnforceIt(t *testing.T) {
	f := setupEdgeFixture(t)
	f.drv.streaming = engine.EdgeStreamingProps{Supported: true, SignatureMaxAge: 15 * time.Minute}
	id := f.createUpload("short.zip")
	partOf := func(n int, declared int, sent []byte) string {
		t.Helper()
		q := url.Values{"uploadId": {id}, "partNumber": {fmt.Sprint(n)}}
		resp := f.admit(f.signedStreaming("/"+f.bucket+"/short.zip", q, int64(declared), "x-amz-checksum-crc64nvme", ""))
		require.True(t, resp.Edge, "%+v", resp)
		var buid string
		require.NoError(t, f.db.QueryRow(`SELECT backend_upload_id FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&buid))
		etag := f.drv.workerPutPart(f.tenantID, f.container(), resp.StorageKey, buid, n, sent)
		relay, _ := f.commit(okCommit(resp.AdmitID, sent, etag))
		require.Equal(t, http.StatusOK, relay.Status, relay.Body)
		return relay.Headers["Etag"]
	}
	e1 := partOf(1, 6000, testBytes(6000))
	e2 := partOf(2, 7000, testBytes(6999)) // one byte short of what the part declared
	completeXML := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, e1, e2)
	wc := f.s3(http.MethodPost, "/"+f.bucket+"/short.zip?uploadId="+id, []byte(completeXML), nil)
	assert.Equal(t, http.StatusBadRequest, wc.Code, wc.Body.String())
	assert.Contains(t, wc.Body.String(), "IncompleteBody")
	_, ok := f.head("short.zip")
	assert.False(t, ok, "nothing committed")
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&status))
	assert.Equal(t, "aborted", status, "the upload is ended: the backend's upload is gone")
	assert.Zero(t, f.used(), "the parts' holding released")
}

// RP72-4: a non-last part under 5 MiB is a real part of the backend's
// multipart on the edge-whole path (D-EDGE-29 admits every part), and the
// backend's Complete refuses it with EntityTooSmall — the client's 400 with
// S3's own code, never a 500; the upload stays active (the client may
// upload the part again and complete).
func TestEdgeComplete_BackendEntityTooSmallIsTheClients400(t *testing.T) {
	f := setupEdgeFixture(t)
	id := f.createUpload("tiny.zip")
	_, e1 := f.edgePart("tiny.zip", id, 1, testBytes(6000))
	_, e2 := f.edgePart("tiny.zip", id, 2, testBytes(7000))
	f.drv.completeErr = fmt.Errorf("edgefake complete multipart: %w",
		&smithy.GenericAPIError{Code: "EntityTooSmall", Message: "Your proposed upload is smaller than the minimum allowed object size."})
	completeXML := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, e1, e2)
	wc := f.s3(http.MethodPost, "/"+f.bucket+"/tiny.zip?uploadId="+id, []byte(completeXML), nil)
	assert.Contains(t, wc.Body.String(), "<Code>EntityTooSmall</Code>", "S3's own code")
	assert.NotContains(t, wc.Body.String(), "InternalError")
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&status))
	assert.Equal(t, "active", status, "the upload stays active")
}
