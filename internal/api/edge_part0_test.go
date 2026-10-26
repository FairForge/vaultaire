package api

// Stage 1a Prompt 45 Part 0: the WP-EDGE-1 Part A review's RP64-1 (the
// commit's terminal state on a cancelled Worker call; the backend delete
// outside the row-lock transaction), RP64-2 (the part admit reads the
// upload's recorded encryption), RP64-5 (a completed edge upload whose
// backend stamp failed is stamped by the reaper) and RP64-9 (a part commit
// racing Abort relays the backend's NoSuchUpload). Each test was red on
// main before its fix (docs/reviews/WP-EDGE-1A-REVIEW.md).

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 — the S3 ETag
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commitWithContext drives handleEdgeCommit with the caller's context — the
// Worker's call, which it may abort at any moment.
func (f *edgeFixture) commitWithContext(ctx context.Context, req edgeCommitRequest) *httptest.ResponseRecorder {
	f.t.Helper()
	b, err := json.Marshal(req)
	require.NoError(f.t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/edge/commit", bytes.NewReader(b)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(edgeTokenHeader, f.token)
	w := httptest.NewRecorder()
	f.srv.handleEdgeCommit(w, r)
	return w
}

// RP64-1: the Worker aborts its commit call at 10 s; the disconnect used to
// cancel the context the terminal UPDATE ran on, so the row stayed pending
// while the head row, the version row and the bill were committed — and the
// retry re-ran the whole settle (a second version row of the same bytes,
// billed twice). Now the handler hands the commit a context the request's
// end cannot cancel: the row is terminal whenever the settle ran, and the
// retry replays the recorded result.
func TestEdgeCommit_CancelledContextStillWritesTerminalState(t *testing.T) {
	f := setupEdgeFixture(t)
	f.exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	body := testBytes(4096)
	resp := f.admit(f.signed("/"+f.bucket+"/cancel.bin", nil, body, nil, ""))
	require.True(t, resp.Edge, "%+v", resp)
	etag := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)

	// The Worker goes away in the middle of the settle: after the head
	// row's upsert, inside the flip's transaction.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prev := headFlipHook
	headFlipHook = func(string, string, string) error { cancel(); return nil }
	t.Cleanup(func() { headFlipHook = prev })
	first := f.commitWithContext(ctx, okCommit(resp.AdmitID, body, etag))
	headFlipHook = prev

	assert.Equal(t, http.StatusOK, first.Code, first.Body.String())
	assert.Equal(t, "committed", f.admitState(resp.AdmitID), "the settle ran: the row is terminal")
	_, ok := f.head("cancel.bin")
	require.True(t, ok, "the head row landed")

	// The Worker's retry 2 s later: the recorded result, nothing re-run.
	relay, code := f.commit(okCommit(resp.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, http.StatusOK, relay.Status)
	assert.Equal(t, etag, relay.Headers["Etag"])
	var versions int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_versions WHERE tenant_id = $1 AND object_key = 'cancel.bin'`, f.tenantID).Scan(&versions))
	assert.Equal(t, 1, versions, "ONE version row")
	assert.Equal(t, int64(4096), f.used(), "billed once")
	var events int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM events WHERE tenant_id = $1 AND type = 'object.created'`, f.tenantID).Scan(&events))
	assert.Equal(t, 1, events, "one object.created")
}

// RP64-1 (the second half): the backend delete of a refused blob — up to
// 40 s — ran INSIDE the transaction that holds the admit row's lock, so
// every retry of the commit waited on it with a DB connection each. Now the
// terminal state is committed first and the delete runs after it.
func TestEdgeCommit_ETagMismatchDeleteOutsideTx(t *testing.T) {
	f := setupEdgeFixture(t)
	body := testBytes(2048)
	sum := md5.Sum(body) // #nosec G401
	md5b64 := base64.StdEncoding.EncodeToString(sum[:])
	resp := f.admit(f.signed("/"+f.bucket+"/mismatch.bin", nil, body, map[string]string{"Content-MD5": md5b64}, ""))
	require.True(t, resp.Edge, "%+v", resp)
	require.True(t, resp.Expect.MD5)
	// The backend answered an ETag of OTHER bytes (a backend that let them
	// through): the commit refuses, deletes the blob and queues it.
	other := testBytes(2049)
	otherSum := md5.Sum(other) // #nosec G401
	otherETag := `"` + hex.EncodeToString(otherSum[:]) + `"`
	f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, other)

	f.drv.deleteGate = make(chan struct{})
	f.drv.deleteEntered = make(chan struct{})
	done := make(chan *edgeRelay, 1)
	go func() {
		relay, _ := f.commit(okCommit(resp.AdmitID, body, otherETag))
		done <- relay
	}()
	select {
	case <-f.drv.deleteEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the backend delete never started")
	}
	// While the delete hangs, the admit row is already terminal and
	// unlocked: a retry (or the reaper) reads it without waiting.
	var state string
	err := f.db.QueryRow(`SELECT state FROM edge_admits WHERE id = $1 FOR UPDATE NOWAIT`, resp.AdmitID).Scan(&state)
	require.NoError(t, err, "the row must not be locked while the backend delete runs")
	assert.Equal(t, "failed", state)
	close(f.drv.deleteGate)
	select {
	case relay := <-done:
		require.NotNil(t, relay)
		assert.Equal(t, http.StatusBadRequest, relay.Status, relay.Body)
		assert.Contains(t, relay.Body, "BadDigest")
	case <-time.After(10 * time.Second):
		t.Fatal("the commit never answered")
	}
	assert.False(t, f.drv.has(f.tenantID, f.container(), resp.StorageKey), "the refused blob is gone")
	var queued int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1 AND storage_key = $2`, f.tenantID, resp.StorageKey).Scan(&queued))
	assert.Equal(t, 1, queued, "and queued for the collector")
	assert.Zero(t, f.used())
}

// RP64-2 (a): an upload created with x-amz-server-side-encryption: AES256
// on a plain bucket. The part's own headers never carry it, so the admit
// placed the part as a whole unsealed write and every part landed plaintext
// — then Complete's placement said "sealed" and refused the upload for
// good. The admit reads the upload row's sse_requested: the placement is
// whole_sealed and the part passes through to the origin, which stages it.
func TestEdgeAdmitPart_SSERequestedUploadPassesThrough(t *testing.T) {
	f := setupEdgeFixture(t)
	f.srv.sseService = sseFor(t, f.db, f.tenantID)
	id := f.createUploadWith("sse.zip", map[string]string{"x-amz-server-side-encryption": "AES256"})
	var sseRequested bool
	require.NoError(t, f.db.QueryRow(`SELECT sse_requested FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&sseRequested))
	require.True(t, sseRequested, "the Create recorded the request")

	resp := f.admit(f.signed("/"+f.bucket+"/sse.zip", url.Values{"uploadId": {id}, "partNumber": {"1"}}, testBytes(3000), nil, ""))
	assert.False(t, resp.Edge, "%+v", resp)
	assert.Equal(t, "placement_whole_sealed", resp.Reason)
	assert.Nil(t, resp.Refusal)
	var edge bool
	require.NoError(t, f.db.QueryRow(`SELECT edge FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&edge))
	assert.False(t, edge, "the upload stays a staged one")
	assert.Zero(t, f.pending())
	assert.Zero(t, f.used(), "nothing reserved")
}

// RP64-2 (b): a keyless part on an SSE-C upload. The origin's uploadPartSeal
// answers 400 InvalidArgument; the admit used to take it (a plaintext part on
// the backend, the upload turned edge, the client's correctly keyed parts
// then refused). The admit reads the upload row's ssec_key_md5 and passes
// the part through: the origin gives the right 400, or seals a keyed part.
func TestEdgeAdmitPart_SSECUploadPassesThrough(t *testing.T) {
	f := setupEdgeFixture(t)
	key := testBytes(32)
	id := f.createUploadWith("ssec.zip", ssecHeaders(key))
	var keyMD5 sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT ssec_key_md5 FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&keyMD5))
	require.True(t, keyMD5.Valid && keyMD5.String != "", "the Create recorded the key's MD5")

	resp := f.admit(f.signed("/"+f.bucket+"/ssec.zip", url.Values{"uploadId": {id}, "partNumber": {"1"}}, testBytes(3000), nil, ""))
	assert.False(t, resp.Edge, "%+v", resp)
	assert.Equal(t, "ssec", resp.Reason)
	assert.Nil(t, resp.Refusal)
	var edge bool
	require.NoError(t, f.db.QueryRow(`SELECT edge FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&edge))
	assert.False(t, edge, "the upload stays a staged one")
	assert.Zero(t, f.pending())
	assert.Zero(t, f.used())
}

// RP64-5: a completed edge upload whose backend_upload_ended_at stamp
// failed (a Warn'd UPDATE) was unpurgeable for ever: the purge excludes
// every edge row with a NULL stamp and the reaper's abort pass took aborted
// rows only. The pass takes completed rows too — AbortMultipart of a
// completed upload answers NoSuchUpload, mapped to success — and stamps.
func TestEdgeReaper_CompletedRowWithNullStampGetsAborted(t *testing.T) {
	f := setupEdgeFixture(t)
	id := f.createUpload("stamp.zip")
	p1 := testBytes(5000)
	_, e1 := f.edgePart("stamp.zip", id, 1, p1)
	xmlBody := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + e1 + `</ETag></Part></CompleteMultipartUpload>`
	w := f.s3(http.MethodPost, "/"+f.bucket+"/stamp.zip?uploadId="+id, []byte(xmlBody), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	// The stamp failed.
	f.exec(`UPDATE multipart_uploads SET backend_upload_ended_at = NULL WHERE upload_id = $1`, id)

	res, err := f.srv.multipartReaper.RunOnce(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.BackendAborted, 1)
	var status string
	var ended sql.NullTime
	require.NoError(t, f.db.QueryRow(`SELECT status, backend_upload_ended_at FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&status, &ended))
	assert.Equal(t, "completed", status, "the row stays completed")
	assert.True(t, ended.Valid, "stamped by the reaper")
	// And now purgeable: the purge takes it once past retention.
	f.exec(`UPDATE multipart_uploads SET created_at = NOW() - interval '30 days' WHERE upload_id = $1`, id)
	_, err = f.srv.multipartReaper.RunOnce(context.Background())
	require.NoError(t, err)
	var left int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&left))
	assert.Zero(t, left, "purged")
}

// RP64-9: a part commit racing Abort — the backend refused the part with
// NoSuchUpload because the upload was aborted between admit and PUT — was
// relayed as 503 SlowDown; the client retried once and only then got the
// admit's NoSuchUpload. The settle answers NoSuchUpload at once.
func TestEdgeCommitPart_NoSuchUploadRelayed(t *testing.T) {
	f := setupEdgeFixture(t)
	id := f.createUpload("race.zip")
	body := testBytes(3000)
	resp := f.admit(f.signed("/"+f.bucket+"/race.zip", url.Values{"uploadId": {id}, "partNumber": {"1"}}, body, nil, ""))
	require.True(t, resp.Edge, "%+v", resp)
	w := f.s3(http.MethodDelete, "/"+f.bucket+"/race.zip?uploadId="+id, nil, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())

	relay, code := f.commit(edgeCommitRequest{AdmitID: resp.AdmitID, Outcome: edgeOutcomeBackendError, BackendStatus: http.StatusNotFound,
		BackendCode: "NoSuchUpload", DurationMS: 3})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, http.StatusNotFound, relay.Status, relay.Body)
	assert.Contains(t, relay.Body, "<Code>NoSuchUpload</Code>")
	assert.Empty(t, relay.Headers["Retry-After"])
	assert.Equal(t, "failed", f.admitState(resp.AdmitID))
	assert.Zero(t, f.used(), "the abort released the holding; nothing is held")
}
