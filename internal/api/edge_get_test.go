package api

// WP-EDGE-1 Part B (Stage 1a Prompt 45): the read half of the edge mover
// at the handlers — the GET admit's pass-through matrix, the throttle
// decision, the origin's headers, the commit's metering compared with a
// GET through the origin of the same bytes, the reaper's branch on op.
// The backend is the Part A fake (edgeFakeDriver); the SigV4 descriptors
// are signed with the real signer against a real api_keys row.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/usage"
)

// signedRequest builds a client's SigV4-signed request of any method as the
// real signer makes it (an empty payload for a GET or a HEAD).
func (f *edgeFixture) signedRequest(method, path string, query url.Values, hdr map[string]string) *http.Request {
	f.t.Helper()
	u := "https://" + f.host + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	r, err := http.NewRequest(method, u, nil)
	require.NoError(f.t, err)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	sum := sha256.Sum256(nil)
	payloadHash := hex.EncodeToString(sum[:])
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	require.NoError(f.t, v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: f.ak, SecretAccessKey: f.sk},
		r, payloadHash, "s3", "us-east-1", time.Now()))
	return r
}

// signedGet is the Worker's description of a client's signed GET.
func (f *edgeFixture) signedGet(path string, query url.Values, hdr map[string]string) edgeAdmitRequest {
	f.t.Helper()
	r := f.signedRequest(http.MethodGet, path, query, hdr)
	headers := map[string]string{"host": f.host}
	for k, v := range r.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	return edgeAdmitRequest{Op: edgeOpGet, Method: http.MethodGet, Host: f.host, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery,
		Headers: headers, ClientIP: "203.0.113.9", RequestID: "req-get-" + strings.ToLower(hex.EncodeToString([]byte(path))[:8])}
}

// originGet reads key through the origin's own GET handler with a REAL
// SigV4 signature (testMode off: the metered path of handleS3Request).
func (f *edgeFixture) originGet(key string, hdr map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := f.signedRequest(http.MethodGet, "/"+f.bucket+"/"+key, nil, hdr)
	r.RemoteAddr = "203.0.113.9:4455"
	w := httptest.NewRecorder()
	f.srv.handleS3Request(w, r)
	return w
}

// wireEgress gives the fixture's server the origin's own egress meter and
// bandwidth tracker (nothing else records egress), with the two reads
// replaced: the allowance and the recorded month are the test's.
func (f *edgeFixture) wireEgress(allowance, recorded int64, enforced *bool) {
	f.t.Helper()
	m := newEgressMeter(f.db, usage.DefaultEgressThrottle(), func(string) bool { return enforced != nil && *enforced }, zap.NewNop())
	m.loadAllowance = func(context.Context, string) (usage.EgressAllowance, error) {
		return usage.EgressAllowance{Bytes: allowance, PlanBytes: allowance, Source: "test"}, nil
	}
	m.loadUsed = func(context.Context, string, time.Time) (int64, error) { return recorded, nil }
	f.srv.egress = m
	f.srv.bandwidthTracker = NewBandwidthTracker(f.db)
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM bandwidth_usage_daily WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM backend_bandwidth_daily WHERE backend_name = 'edgefake'`)
	})
}

// egressRecorded is the tenant's recorded egress this month.
func (f *edgeFixture) egressRecorded() int64 {
	f.t.Helper()
	f.srv.bandwidthTracker.Flush()
	var n int64
	require.NoError(f.t, f.db.QueryRow(`SELECT COALESCE(SUM(egress_bytes), 0) FROM bandwidth_usage_daily WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	return n
}

// putObject writes key through the origin and returns its head row.
func (f *edgeFixture) putObject(key string, body []byte, hdr map[string]string) edgeHeadRow {
	f.t.Helper()
	w := f.originPut(key, body, hdr)
	require.Equal(f.t, http.StatusOK, w.Code, w.Body.String())
	h, ok := f.head(key)
	require.True(f.t, ok)
	return h
}

func servedCommit(admitID string, n int64, status int) edgeCommitRequest {
	return edgeCommitRequest{AdmitID: admitID, Outcome: edgeGetServed, BackendStatus: status, BackendContentLength: n, DurationMS: 5}
}

// --- the admit ---

func TestEdgeAdmitGet_PassThroughMatrix(t *testing.T) {
	f := setupEdgeFixture(t)
	body := testBytes(8192)
	f.putObject("obj.bin", body, nil)
	path := "/" + f.bucket + "/obj.bin"

	// The baseline: admitted.
	resp := f.admit(f.signedGet(path, nil, nil))
	require.True(t, resp.Edge, "%+v", resp)
	assert.Equal(t, http.MethodGet, resp.Method)
	assert.Equal(t, http.StatusPartialContent, resp.ExpectStatus, "the backend answers the signed Range")
	assert.Equal(t, http.StatusOK, resp.Status, "the client gets the origin's 200")
	assert.Equal(t, "edgefake", resp.Backend)
	assert.Equal(t, int64(8192), resp.ContentLength, "the admitted span: the Worker's length check (RP66-2)")
	assert.Equal(t, 1, f.pending())
	f.exec(`DELETE FROM edge_admits WHERE tenant_id = $1`, f.tenantID)

	passes := func(name string, req edgeAdmitRequest, reason string) {
		t.Helper()
		resp := f.admit(req)
		assert.False(t, resp.Edge, "%s: %+v", name, resp)
		assert.Equal(t, reason, resp.Reason, name)
		assert.Nil(t, resp.Refusal, name)
		assert.Zero(t, f.pending(), "%s: nothing pending", name)
	}
	// HEAD never (Decision 2: cache-only).
	head := f.signedGet(path, nil, nil)
	head.Method = http.MethodHead
	passes("head", head, "not_get")
	// A presigned URL carries no Authorization header: the origin's.
	presigned := f.signedGet(path, nil, nil)
	delete(presigned.Headers, "authorization")
	presigned.Query = "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=00"
	passes("presigned", presigned, "unsigned")
	passes("versionId", f.signedGet(path, url.Values{"versionId": {"v1"}}, nil), "query")
	passes("response override", f.signedGet(path, url.Values{"response-content-disposition": {"attachment"}}, nil), "query")
	for _, h := range []string{"If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		passes(h, f.signedGet(path, nil, map[string]string{h: `"x"`}), "conditional")
	}
	passes("If-Match of another ETag (the origin's 412)", f.signedGet(path, nil, map[string]string{"If-Match": `"00000000000000000000000000000000"`}), "conditional")
	passes("multi-range", f.signedGet(path, nil, map[string]string{"Range": "bytes=0-1,5-6"}), "multi_range")
	passes("416", f.signedGet(path, nil, map[string]string{"Range": "bytes=99999-"}), "range")
	passes("ssec headers", f.signedGet(path, nil, ssecHeaders(testBytes(32))), "ssec")
	passes("no row", f.signedGet("/"+f.bucket+"/missing.bin", nil, nil), "no_row")
	passes("no bucket", f.signedGet("/nobucket/x.bin", nil, nil), "no_bucket")

	f.srv.edge.minBytes = 1 << 20
	passes("under EDGE_MIN_BYTES", f.signedGet(path, nil, nil), edgeReasonSmall)
	passes("a small range of a big enough object", f.signedGet(path, nil, map[string]string{"Range": "bytes=0-9"}), edgeReasonSmall)
	f.srv.edge.minBytes = 0

	require.NoError(t, f.fl.Set(context.Background(), flagEdgeIngest, f.tenantID, false, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	passes("flag off", f.signedGet(path, nil, nil), "flag_off")
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeIngest, f.tenantID, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))

	f.exec(`UPDATE buckets SET visibility = 'public-read' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	passes("public bucket", f.signedGet(path, nil, nil), "public")
	f.exec(`UPDATE buckets SET visibility = 'private' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)

	rowShape := func(name, set, reason string) {
		t.Helper()
		f.exec(`UPDATE object_head_cache SET `+set+` WHERE tenant_id = $1 AND object_key = 'obj.bin'`, f.tenantID)
		passes(name, f.signedGet(path, nil, nil), reason)
	}
	rowShape("chunked", `is_chunked = TRUE`, "chunked")
	rowShape("sealed", `is_chunked = FALSE, encryption_algorithm = 'AES256'`, "sealed")
	rowShape("ssec row", `encryption_algorithm = 'AES256-SSEC-V3'`, "sealed")
	rowShape("vault floor (tape, parity)", `encryption_algorithm = '', floor = 'vault'`, "vault")
	rowShape("sia", `floor = 'standard', backend_name = 'sia'`, "backend:sia")
	rowShape("blomp", `backend_name = 'blomp'`, "backend:blomp")
	rowShape("an unregistered backend", `backend_name = 'gone'`, "backend:gone")
	rowShape("no backend", `backend_name = NULL`, "backend:")
	rowShape("empty", `backend_name = 'edgefake', size_bytes = 0`, "empty")
	f.exec(`UPDATE object_head_cache SET size_bytes = 8192 WHERE tenant_id = $1 AND object_key = 'obj.bin'`, f.tenantID)
	// A stored Content-Encoding: the Workers runtime re-encodes a Response
	// body to match the header it is given, and the backend's answer carries
	// plain bytes with no encoding header — the origin serves it correctly,
	// the edge would gzip the gzip (RP66-1).
	rowShape("stored content encoding", `content_encoding = 'gzip'`, "content_encoding")
	f.exec(`UPDATE object_head_cache SET content_encoding = '' WHERE tenant_id = $1 AND object_key = 'obj.bin'`, f.tenantID)

	// Versioned, behind a delete marker: the origin's 404.
	f.exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	f.exec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, is_latest, is_delete_marker, etag, size_bytes, content_type)
		VALUES ($1, $2, 'obj.bin', 'marker-1', TRUE, TRUE, '', 0, '')`, f.tenantID, f.bucket)
	passes("delete marker", f.signedGet(path, nil, nil), "delete_marker")
	f.exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenantID)

	// And admitted again once every shape is undone.
	resp = f.admit(f.signedGet(path, nil, nil))
	assert.True(t, resp.Edge, "%+v", resp)
	assert.Equal(t, "null", resp.ResponseHeaders["X-Amz-Version-Id"], "a versioned bucket's object with no version row is the null version")
	// aws-cli's download shape: If-Match of the row's ETag + Range bytes=0-
	// is admitted (the condition holds; nothing is left for the origin).
	h, _ := f.head("obj.bin")
	resp = f.admit(f.signedGet(path, nil, map[string]string{"If-Match": `"` + h.etag + `"`, "Range": "bytes=0-"}))
	assert.True(t, resp.Edge, "%+v", resp)
	assert.Equal(t, http.StatusPartialContent, resp.Status)
	assert.Equal(t, "bytes=0-8191", resp.SendHeaders["range"])
	resp = f.admit(f.signedGet(path, nil, map[string]string{"If-Match": "*"}))
	assert.True(t, resp.Edge, "%+v", resp)
}

func TestEdgeAdmitGet_ThrottledTenantPassesThrough(t *testing.T) {
	f := setupEdgeFixture(t)
	f.putObject("big.bin", testBytes(16384), nil)
	path := "/" + f.bucket + "/big.bin"
	enforced := true
	f.wireEgress(1000, 5000, &enforced) // past the allowance

	resp := f.admit(f.signedGet(path, nil, nil))
	assert.False(t, resp.Edge, "%+v", resp)
	assert.Equal(t, "throttled", resp.Reason, "the origin paces it (D-25); the Worker never paces")
	assert.Nil(t, resp.Refusal)
	assert.Zero(t, f.pending())

	// The flag off for the tenant (the exemption): admitted, the decision
	// counted where the origin counts it.
	before := promtestutil.ToFloat64(egressWouldThrottle.WithLabelValues(edgeHost))
	enforced = false
	resp = f.admit(f.signedGet(path, nil, nil))
	assert.True(t, resp.Edge, "%+v", resp)
	assert.Equal(t, before+1, promtestutil.ToFloat64(egressWouldThrottle.WithLabelValues(edgeHost)))

	// Under the allowance: admitted whatever the flag.
	enforced = true
	f.srv.egress.loadUsed = func(context.Context, string, time.Time) (int64, error) { return 10, nil }
	f.srv.egress.mu.Lock()
	delete(f.srv.egress.tenants, f.tenantID)
	f.srv.egress.mu.Unlock()
	resp = f.admit(f.signedGet(path, nil, nil))
	assert.True(t, resp.Edge, "%+v", resp)
}

func TestEdgeAdmitGet_NothingMeteredAtAdmit(t *testing.T) {
	f := setupEdgeFixture(t)
	f.srv.accessLogTracker = NewS3AccessLogTracker(f.db)
	f.putObject("meter.bin", testBytes(4096), nil)
	f.wireEgress(1<<30, 0, nil)

	resp := f.admit(f.signedGet("/"+f.bucket+"/meter.bin", nil, nil))
	require.True(t, resp.Edge, "%+v", resp)
	assert.Zero(t, f.egressRecorded())
	st, err := f.srv.egress.EgressStatus(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.Zero(t, st.UsedBytes)
	f.srv.accessLogTracker.Flush()
	var logs, events int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM s3_access_log WHERE tenant_id = $1 AND operation = 'GetObject'`, f.tenantID).Scan(&logs))
	assert.Zero(t, logs)
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM events WHERE tenant_id = $1 AND type = 'object.downloaded'`, f.tenantID).Scan(&events))
	assert.Zero(t, events)
	var reserved int64
	require.NoError(t, f.db.QueryRow(`SELECT reserved_bytes FROM edge_admits WHERE id = $1`, resp.AdmitID).Scan(&reserved))
	assert.Zero(t, reserved, "a read reserves nothing")
}

func TestEdgeAdmitGet_RangeHeaders(t *testing.T) {
	f := setupEdgeFixture(t)
	h := f.putObject("range.bin", testBytes(8192), map[string]string{"Content-Type": "application/x-test"})
	path := "/" + f.bucket + "/range.bin"

	resp := f.admit(f.signedGet(path, nil, map[string]string{"Range": "bytes=100-299"}))
	require.True(t, resp.Edge, "%+v", resp)
	assert.Equal(t, http.StatusPartialContent, resp.ExpectStatus)
	assert.Equal(t, http.StatusPartialContent, resp.Status)
	assert.Equal(t, "bytes=100-299", resp.SendHeaders["range"])
	assert.Equal(t, `"`+h.etag+`"`, resp.SendHeaders["if-match"])
	assert.Equal(t, "bytes 100-299/8192", resp.ResponseHeaders["Content-Range"])
	assert.Equal(t, "200", resp.ResponseHeaders["Content-Length"])
	assert.Equal(t, "application/x-test", resp.ResponseHeaders["Content-Type"])
	assert.Equal(t, `"`+h.etag+`"`, resp.ResponseHeaders["Etag"])
	var admitted int64
	require.NoError(t, f.db.QueryRow(`SELECT content_length FROM edge_admits WHERE id = $1`, resp.AdmitID).Scan(&admitted))
	assert.Equal(t, int64(200), admitted, "the bytes to be served, not the object's size")

	// A suffix range and an open-ended one are sent to the backend
	// normalised (the backend's 206 then matches the origin's headers).
	resp = f.admit(f.signedGet(path, nil, map[string]string{"Range": "bytes=-100"}))
	require.True(t, resp.Edge)
	assert.Equal(t, "bytes=8092-8191", resp.SendHeaders["range"])
	assert.Equal(t, "bytes 8092-8191/8192", resp.ResponseHeaders["Content-Range"])
	resp = f.admit(f.signedGet(path, nil, map[string]string{"Range": "bytes=8000-"}))
	require.True(t, resp.Edge)
	assert.Equal(t, "bytes=8000-8191", resp.SendHeaders["range"])
	assert.Equal(t, "192", resp.ResponseHeaders["Content-Length"])

	// A whole GET: the whole object's span signed (the backend answers
	// 206 to it), the client's 200 with the origin's whole-object headers.
	resp = f.admit(f.signedGet(path, nil, nil))
	require.True(t, resp.Edge)
	assert.Equal(t, http.StatusPartialContent, resp.ExpectStatus)
	assert.Equal(t, http.StatusOK, resp.Status)
	assert.Equal(t, "bytes=0-8191", resp.SendHeaders["range"])
	assert.Equal(t, "8192", resp.ResponseHeaders["Content-Length"])
	assert.Empty(t, resp.ResponseHeaders["Content-Range"])
}

// The If-Match the link signs is the row's digest ONLY where it is provably
// the backend's own ETag (D-EDGE-10): an object the origin wrote as one
// backend PutObject (≤ drivers.S3SinglePutMaxBytes — its MD5, or the
// body_md5 of an origin-assembled multipart) and an edge-assembled
// multipart (the backend's own -N). A larger origin-written object is a
// backend multipart whose ETag the row does not know: no If-Match.
func TestEdgeAdmitGet_IfMatchIsTheRowsDigest(t *testing.T) {
	f := setupEdgeFixture(t)
	f.putObject("single.bin", testBytes(5000), nil)
	path := "/" + f.bucket + "/single.bin"
	ifMatchOf := func() string {
		t.Helper()
		resp := f.admit(f.signedGet(path, nil, nil))
		require.True(t, resp.Edge, "%+v", resp)
		u, err := url.Parse(resp.URL)
		require.NoError(t, err)
		assert.Equal(t, resp.SendHeaders["if-match"], u.Query().Get("x-probe-if-match"), "signed into the link")
		assert.NotEmpty(t, resp.SendHeaders["range"], "every link signs the byte span")
		return resp.SendHeaders["if-match"]
	}
	h, _ := f.head("single.bin")
	assert.Equal(t, `"`+h.etag+`"`, ifMatchOf(), "a single PUT: the MD5 ETag")
	// An origin-assembled multipart: the -N ETag with the whole body's MD5
	// recorded — the backend holds one PUT of the whole body.
	f.exec(`UPDATE object_head_cache SET etag = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-3', body_md5 = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' WHERE tenant_id = $1 AND object_key = 'single.bin'`, f.tenantID)
	assert.Equal(t, `"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`, ifMatchOf())
	// An edge-assembled multipart: the backend's own -N ETag, no body_md5.
	f.exec(`UPDATE object_head_cache SET etag = 'cccccccccccccccccccccccccccccccc-4', body_md5 = NULL WHERE tenant_id = $1 AND object_key = 'single.bin'`, f.tenantID)
	assert.Equal(t, `"cccccccccccccccccccccccccccccccc-4"`, ifMatchOf())
	// Past the single-PUT cutoff the origin wrote a backend multipart: its
	// -N is unknown to the row — no If-Match, whatever the row records.
	big := int64(drivers.S3SinglePutMaxBytes + 1)
	f.exec(`UPDATE object_head_cache SET etag = 'dddddddddddddddddddddddddddddddd', body_md5 = NULL, size_bytes = $2 WHERE tenant_id = $1 AND object_key = 'single.bin'`, f.tenantID, big)
	assert.Equal(t, "", ifMatchOf(), "a single MD5 ETag of a big object: the backend holds a multipart")
	f.exec(`UPDATE object_head_cache SET etag = 'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee-5', body_md5 = 'ffffffffffffffffffffffffffffffff' WHERE tenant_id = $1 AND object_key = 'single.bin'`, f.tenantID)
	assert.Equal(t, "", ifMatchOf(), "an origin-assembled multipart past the cutoff: the whole body went as a backend multipart")
	f.exec(`UPDATE object_head_cache SET etag = 'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee-5', body_md5 = NULL WHERE tenant_id = $1 AND object_key = 'single.bin'`, f.tenantID)
	assert.Equal(t, `"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee-5"`, ifMatchOf(), "an edge-assembled one of any size: the backend's own")
	// Exactly at the cutoff: one PUT.
	f.exec(`UPDATE object_head_cache SET etag = 'dddddddddddddddddddddddddddddddd', body_md5 = NULL, size_bytes = $2 WHERE tenant_id = $1 AND object_key = 'single.bin'`, f.tenantID, int64(drivers.S3SinglePutMaxBytes))
	assert.Equal(t, `"dddddddddddddddddddddddddddddddd"`, ifMatchOf())
}

func TestEdgeAdmitGet_HeadersAreTheOrigins(t *testing.T) {
	f := setupEdgeFixture(t)
	hdr := map[string]string{"Content-Type": "text/plain", "x-amz-meta-origin": "edge-test", "Cache-Control": "max-age=60",
		"Content-Disposition": "attachment; filename=a.txt", "Content-Language": "en", "x-amz-tagging": "k=v"}
	f.putObject("hdr.txt", testBytes(6000), hdr)
	path := "/" + f.bucket + "/hdr.txt"

	compare := func(name string, reqHdr map[string]string) {
		t.Helper()
		w := f.originGet("hdr.txt", reqHdr)
		require.True(t, w.Code == http.StatusOK || w.Code == http.StatusPartialContent, "%s: %d %s", name, w.Code, w.Body.String())
		origin := map[string]string{}
		for k, v := range w.Header() {
			origin[k] = v[0]
		}
		resp := f.admit(f.signedGet(path, nil, reqHdr))
		require.True(t, resp.Edge, "%s: %+v", name, resp)
		edge := resp.ResponseHeaders
		for _, k := range []string{"X-Amz-Request-Id", "Date"} {
			delete(origin, k)
			delete(edge, k)
		}
		assert.Equal(t, origin, edge, name)
		assert.Equal(t, w.Code, resp.Status, name)
		assert.Equal(t, http.StatusPartialContent, resp.ExpectStatus, name)
	}
	compare("whole", nil)
	compare("range", map[string]string{"Range": "bytes=10-1009"})
	// Versioned: the version the response names.
	f.exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	f.putObject("hdr.txt", testBytes(6000), hdr)
	compare("versioned", nil)
	var vid string
	require.NoError(t, f.db.QueryRow(`SELECT version_id FROM object_versions WHERE tenant_id = $1 AND object_key = 'hdr.txt' AND is_latest`, f.tenantID).Scan(&vid))
	resp := f.admit(f.signedGet(path, nil, nil))
	assert.Equal(t, vid, resp.ResponseHeaders["X-Amz-Version-Id"])
	// Object Lock headers ride along.
	f.exec(`INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date, legal_hold)
		VALUES ($1, $2, 'hdr.txt', 'GOVERNANCE', NOW() + interval '1 day', FALSE)`, f.tenantID, f.bucket)
	compare("locked", nil)
}

// --- the commit ---

func TestEdgeCommitGet_MetersBackendContentLength(t *testing.T) {
	f := setupEdgeFixture(t)
	body := testBytes(12000)
	f.putObject("cmp.bin", body, nil)
	f.wireEgress(1<<30, 0, nil)

	// The control: the same bytes through the origin.
	w := f.originGet("cmp.bin", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, body, w.Body.Bytes())
	viaOrigin := f.egressRecorded()
	require.Equal(t, int64(12000), viaOrigin)

	resp := f.admit(f.signedGet("/"+f.bucket+"/cmp.bin", nil, nil))
	require.True(t, resp.Edge, "%+v", resp)
	relay, code := f.commit(servedCommit(resp.AdmitID, 12000, http.StatusOK))
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, http.StatusOK, relay.Status)
	assert.Equal(t, "committed", f.admitState(resp.AdmitID))
	assert.Equal(t, 2*viaOrigin, f.egressRecorded(), "the edge read recorded exactly what the origin's did")
	st, err := f.srv.egress.EgressStatus(context.Background(), f.tenantID)
	require.NoError(t, err)
	assert.Equal(t, 2*viaOrigin, st.UsedBytes, "one number: the live counter")
	var byBackend int64
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(SUM(egress_bytes), 0) FROM backend_bandwidth_daily WHERE backend_name = 'edgefake'`).Scan(&byBackend))
	assert.GreaterOrEqual(t, byBackend, int64(12000), "attributed to the backend that served it")

	// A commit replayed meters nothing twice.
	again, _ := f.commit(servedCommit(resp.AdmitID, 12000, http.StatusOK))
	assert.Equal(t, relay, again)
	assert.Equal(t, 2*viaOrigin, f.egressRecorded())

	// A range: the backend's Content-Length of the range.
	resp = f.admit(f.signedGet("/"+f.bucket+"/cmp.bin", nil, map[string]string{"Range": "bytes=0-999"}))
	require.True(t, resp.Edge)
	before := promtestutil.ToFloat64(dataPlaneBytes.WithLabelValues(edgeHost, edgeOpGet))
	relay, _ = f.commit(servedCommit(resp.AdmitID, 1000, http.StatusPartialContent))
	assert.Equal(t, http.StatusOK, relay.Status)
	assert.Equal(t, 2*viaOrigin+1000, f.egressRecorded())
	assert.Equal(t, before+1000, promtestutil.ToFloat64(dataPlaneBytes.WithLabelValues(edgeHost, edgeOpGet)))

	// A report of more than was admitted is clamped to the admitted size.
	resp = f.admit(f.signedGet("/"+f.bucket+"/cmp.bin", nil, map[string]string{"Range": "bytes=0-499"}))
	require.True(t, resp.Edge)
	f.commit(servedCommit(resp.AdmitID, 1<<40, http.StatusPartialContent))
	assert.Equal(t, 2*viaOrigin+1000+500, f.egressRecorded())
}

func TestEdgeCommitGet_FallbackMetersNothingAndFeedsBreaker(t *testing.T) {
	f := setupEdgeFixture(t)
	f.srv.accessLogTracker = NewS3AccessLogTracker(f.db)
	f.putObject("fb.bin", testBytes(9000), nil)
	f.wireEgress(1<<30, 0, nil)

	fallback := func(status int, outcome, code string) {
		t.Helper()
		resp := f.admit(f.signedGet("/"+f.bucket+"/fb.bin", nil, nil))
		require.True(t, resp.Edge, "%+v", resp)
		relay, rc := f.commit(edgeCommitRequest{AdmitID: resp.AdmitID, Outcome: outcome, BackendStatus: status, BackendCode: code})
		require.Equal(t, http.StatusOK, rc)
		assert.Equal(t, http.StatusOK, relay.Status, "acknowledged")
		assert.Equal(t, "failed", f.admitState(resp.AdmitID))
	}
	// The object's state is never the backend's health.
	fallback(http.StatusNotFound, edgeGetFallback, "NoSuchKey")
	fallback(http.StatusPreconditionFailed, edgeGetFallback, "PreconditionFailed")
	require.NoError(t, f.srv.engine.CanAsk("edgefake"), "a 404 and a 412 charge nothing")
	assert.Zero(t, f.egressRecorded(), "nothing metered: the origin served it")
	f.srv.accessLogTracker.Flush()
	var logs int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM s3_access_log WHERE tenant_id = $1 AND operation = 'GetObject'`, f.tenantID).Scan(&logs))
	assert.Zero(t, logs, "no access-log row: the origin's serve writes its own")

	// Lyve's HEAD 200 / GET 500 of 2026-10-21, five times: the breaker
	// opens as it would on the origin's own reads (RE-3 closes for reads).
	for i := 0; i < 5; i++ {
		if i < 4 {
			fallback(http.StatusInternalServerError, edgeGetFallback, "InternalError")
		} else {
			fallback(0, edgeGetError, "connection reset")
		}
	}
	assert.Error(t, f.srv.engine.CanAsk("edgefake"), "the breaker is open")
	// And an open breaker passes the next GET through to the origin.
	resp := f.admit(f.signedGet("/"+f.bucket+"/fb.bin", nil, nil))
	assert.False(t, resp.Edge)
	assert.Equal(t, "backend:edgefake", resp.Reason)
	assert.Zero(t, f.egressRecorded())
}

func TestEdgeCommitGet_AccessLogAndEvent(t *testing.T) {
	f := setupEdgeFixture(t)
	f.srv.accessLogTracker = NewS3AccessLogTracker(f.db)
	f.putObject("log.bin", testBytes(7000), nil)
	f.wireEgress(1<<30, 0, nil)

	req := f.signedGet("/"+f.bucket+"/log.bin", nil, map[string]string{"User-Agent": "rclone/v1.70.0"})
	resp := f.admit(req)
	require.True(t, resp.Edge, "%+v", resp)
	relay, _ := f.commit(servedCommit(resp.AdmitID, 7000, http.StatusOK))
	require.Equal(t, http.StatusOK, relay.Status)
	f.srv.accessLogTracker.Flush()
	var op, ip, ua, reqID string
	var out, in int64
	var status int
	require.NoError(t, f.db.QueryRow(`SELECT operation, source_ip, user_agent, request_id, bytes_sent, bytes_received, status_code
		FROM s3_access_log WHERE tenant_id = $1 AND object_key = 'log.bin' AND operation = 'GetObject'`, f.tenantID).Scan(&op, &ip, &ua, &reqID, &out, &in, &status))
	assert.Equal(t, "GetObject", op)
	assert.Equal(t, "203.0.113.9", ip, "the client's IP, not the Worker's")
	assert.Equal(t, "rclone/v1.70.0", ua, "the client's user agent (RP64-11)")
	assert.Equal(t, req.RequestID, reqID)
	assert.Equal(t, int64(7000), out)
	assert.Zero(t, in)
	assert.Equal(t, http.StatusOK, status)
	var size int64
	require.NoError(t, f.db.QueryRow(`SELECT (data->>'size')::bigint FROM events WHERE tenant_id = $1 AND type = 'object.downloaded'`, f.tenantID).Scan(&size))
	assert.Equal(t, int64(7000), size)

	// A range: the 206 in the row.
	resp = f.admit(f.signedGet("/"+f.bucket+"/log.bin", nil, map[string]string{"Range": "bytes=0-99"}))
	require.True(t, resp.Edge)
	f.commit(servedCommit(resp.AdmitID, 100, http.StatusPartialContent))
	f.srv.accessLogTracker.Flush()
	require.NoError(t, f.db.QueryRow(`SELECT status_code, bytes_sent FROM s3_access_log WHERE tenant_id = $1 AND object_key = 'log.bin' AND status_code = 206`, f.tenantID).Scan(&status, &out))
	assert.Equal(t, int64(100), out)
}

// --- the reaper ---

func TestEdgeReaper_ExpiredGetMetersAdmittedSizeReleasesNothingQueuesNothing(t *testing.T) {
	f := setupEdgeFixture(t)
	h := f.putObject("reap.bin", testBytes(11000), nil)
	f.wireEgress(1<<30, 0, nil)
	resp := f.admit(f.signedGet("/"+f.bucket+"/reap.bin", nil, map[string]string{"Range": "bytes=0-4999"}))
	require.True(t, resp.Edge, "%+v", resp)
	assert.Equal(t, h.sk, resp.StorageKey, "the LIVE object's blob")
	used := f.used()

	f.exec(`UPDATE edge_admits SET expires_at = NOW() - interval '1 minute' WHERE id = $1`, resp.AdmitID)
	res, err := f.srv.edgeReaper.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Expired)
	assert.Equal(t, 1, res.GetsMetered)
	assert.Equal(t, int64(5000), res.MeteredBytes)
	assert.Zero(t, res.Retired)
	assert.Zero(t, res.ReleasedBytes)
	assert.Equal(t, "expired", f.admitState(resp.AdmitID))
	assert.Equal(t, int64(5000), f.egressRecorded(), "the admitted size, the upper bound")
	assert.Equal(t, used, f.used(), "nothing released")
	var queued int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1 AND storage_key = $2`, f.tenantID, h.sk).Scan(&queued))
	assert.Zero(t, queued, "the live blob is never queued")
	assert.True(t, f.drv.has(f.tenantID, f.container(), h.sk), "and still there")
	// A late commit answers 410 and meters nothing more.
	_, code := f.commit(servedCommit(resp.AdmitID, 5000, http.StatusPartialContent))
	assert.Equal(t, http.StatusGone, code)
	assert.Equal(t, int64(5000), f.egressRecorded())
}

// --- the structural rules ---

// A GET admit's pass-through is never a refusal and never reserves: every
// reason in the matrix leaves no row.
func TestEdgeAdmitGet_NeverRefusesOnItsOwn(t *testing.T) {
	f := setupEdgeFixture(t)
	f.putObject("r.bin", testBytes(4096), nil)
	f.drv.failPresign.Store(true)
	w := f.call("/api/v1/edge/admit", f.signedGet("/"+f.bucket+"/r.bin", nil, nil), f.token)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "a control-plane failure is the call's 5xx: the Worker passes through")
	assert.Zero(t, f.pending())
	f.drv.failPresign.Store(false)
	// A link that signs neither range nor if-match is never handed out.
	assert.Error(t, edgePresignSigns("https://b.invalid/x?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=ab&X-Amz-SignedHeaders=host", "if-match"))
	assert.Error(t, edgePresignSigns("https://b.invalid/x?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=ab&X-Amz-SignedHeaders=host%3Bif-match", "if-match", "range"))
	assert.NoError(t, edgePresignSigns("https://b.invalid/x?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=ab&X-Amz-SignedHeaders=host%3Bif-match%3Brange", "if-match", "range"))
}

// --- Part 0 of WP-EDGE-2 (the RP66 fixes) ---

// RP66-3 + RP66-2: a served edge read refreshes last_accessed exactly as the
// origin's cache hit does (the smart_demotion job reads it), and is counted
// once in vaultaire_object_integrity_unverified_total{path="edge"} — the
// edge path serves bytes no one checks beyond the length, and the counter
// says so. RP66-5: the response's x-amz-request-id is the Worker's (what the
// admit row and the access-log row carry).
func TestEdgeCommitGet_ServedTouchesLastAccessedAndCountsUnverified(t *testing.T) {
	f := setupEdgeFixture(t)
	f.putObject("touch.bin", testBytes(6000), nil)
	f.wireEgress(1<<30, 0, nil)
	f.exec(`UPDATE object_head_cache SET last_accessed = NOW() - interval '3 days' WHERE tenant_id = $1 AND object_key = 'touch.bin'`, f.tenantID)

	req := f.signedGet("/"+f.bucket+"/touch.bin", nil, nil)
	resp := f.admit(req)
	require.True(t, resp.Edge, "%+v", resp)
	assert.Equal(t, req.RequestID, resp.ResponseHeaders["X-Amz-Request-Id"],
		"the id the client sees is the one the logs hold (RP66-5)")

	before := promtestutil.ToFloat64(objectIntegrityUnverified.WithLabelValues("edge", "unverified"))
	_, code := f.commit(servedCommit(resp.AdmitID, 6000, http.StatusOK))
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, before+1, promtestutil.ToFloat64(objectIntegrityUnverified.WithLabelValues("edge", "unverified")),
		"every served edge read is unverified by design (RP66-2)")

	// The touch is fire-and-forget, as the origin's.
	assert.Eventually(t, func() bool {
		var age float64
		if err := f.db.QueryRow(`SELECT EXTRACT(EPOCH FROM (NOW() - last_accessed)) FROM object_head_cache
			WHERE tenant_id = $1 AND object_key = 'touch.bin'`, f.tenantID).Scan(&age); err != nil {
			return false
		}
		return age < 60
	}, 3*time.Second, 50*time.Millisecond, "last_accessed refreshed (RP66-3)")

	// A fallback touches nothing and counts nothing.
	f.exec(`UPDATE object_head_cache SET last_accessed = NOW() - interval '3 days' WHERE tenant_id = $1 AND object_key = 'touch.bin'`, f.tenantID)
	resp = f.admit(f.signedGet("/"+f.bucket+"/touch.bin", nil, nil))
	require.True(t, resp.Edge)
	served := promtestutil.ToFloat64(objectIntegrityUnverified.WithLabelValues("edge", "unverified"))
	_, code = f.commit(edgeCommitRequest{AdmitID: resp.AdmitID, Outcome: edgeGetFallback, BackendStatus: 500, BackendCode: "InternalError"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, served, promtestutil.ToFloat64(objectIntegrityUnverified.WithLabelValues("edge", "unverified")))
	time.Sleep(150 * time.Millisecond)
	var age float64
	require.NoError(t, f.db.QueryRow(`SELECT EXTRACT(EPOCH FROM (NOW() - last_accessed)) FROM object_head_cache
		WHERE tenant_id = $1 AND object_key = 'touch.bin'`, f.tenantID).Scan(&age))
	assert.Greater(t, age, float64(3600), "the origin's own serve touches it; the edge fallback does not")
}

// RP66-6: terminal GET rows are purged after 24 h (nothing later reads them;
// the access log has the bytes) — a 1 TB aws-cli download writes ~131,000 of
// them. Writes keep the 7-day retention.
func TestEdgeReaper_TerminalGetRowsPurgedAt24h(t *testing.T) {
	f := setupEdgeFixture(t)
	insert := func(id, op, state string, finishedAgo string) {
		f.exec(`INSERT INTO edge_admits (id, tenant_id, bucket, object_key, op, backend, storage_key, content_length, state, expires_at, finished_at)
			VALUES ($1, $2, $3, 'purge.bin', $4, 'edgefake', 'purge.bin@x', 100, $5, NOW(), NOW() - $6::interval)`,
			id, f.tenantID, f.bucket, op, state, finishedAgo)
	}
	insert("purgegetold0000000000000001", edgeOpGet, "committed", "25 hours")
	insert("purgegetnew0000000000000002", edgeOpGet, "committed", "23 hours")
	insert("purgeputmid0000000000000003", edgeOpPut, "failed", "25 hours")
	insert("purgeputold0000000000000004", edgeOpPut, "expired", "8 days")

	_, err := f.srv.edgeReaper.RunOnce(context.Background())
	require.NoError(t, err)

	left := map[string]bool{}
	rows, err := f.db.Query(`SELECT id FROM edge_admits WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		left[id] = true
	}
	require.NoError(t, rows.Err())
	assert.False(t, left["purgegetold0000000000000001"], "a terminal get row past 24 h is purged")
	assert.True(t, left["purgegetnew0000000000000002"], "a younger get row stays")
	assert.True(t, left["purgeputmid0000000000000003"], "a write keeps the 7-day retention")
	assert.False(t, left["purgeputold0000000000000004"], "a write past 7 days is purged")
}

// --- Prompt 46c PR B: the windowed reader (D-EDGE-32) ---

// A read of at least 64 MiB through the edge is the Worker's windowed
// reader's: the admit answers window {range_bytes: 16 MiB, in_flight: 6}
// and the span's start, and its link signs if-match where provable
// (D-EDGE-10) but NOT range — the Worker names each 16 MiB range. A read
// under 64 MiB keeps Part B's exact-span link (D-EDGE-11).
func TestEdgeAdmitGet_WindowForLargeReads(t *testing.T) {
	f := setupEdgeFixture(t)
	f.putObject("big.bin", testBytes(8192), nil)
	// The row of a 100 MiB edge-assembled object (its -N is the backend's
	// own: if-match provable) — the admit reads the row, never the bytes.
	f.exec(`UPDATE object_head_cache SET size_bytes = $2, etag = 'cccccccccccccccccccccccccccccccc-13', body_md5 = NULL WHERE tenant_id = $1 AND object_key = 'big.bin'`,
		f.tenantID, int64(100<<20))
	path := "/" + f.bucket + "/big.bin"
	// A Worker that does not say it reads a window (0.3.0 and before) gets
	// Part B's exact-span link whatever the size: it would GET a range-less
	// link, receive a 200 where it expects a 206 and fall back — charging
	// the backend's breaker on every large read until 0.4.0 is deployed.
	old := f.admit(f.signedGet(path, nil, nil))
	require.True(t, old.Edge, "%+v", old)
	assert.Nil(t, old.Window, "no window without the Worker's feature")
	assert.Equal(t, "bytes=0-104857599", old.SendHeaders["range"])

	resp := f.admit(windowed(f.signedGet(path, nil, nil)))
	require.True(t, resp.Edge, "%+v", resp)
	require.NotNil(t, resp.Window, "a 100 MiB read is windowed")
	assert.Equal(t, int64(16<<20), resp.Window.RangeBytes)
	assert.Equal(t, 6, resp.Window.InFlight)
	assert.Equal(t, int64(0), resp.Window.Start)
	assert.Equal(t, int64(100<<20), resp.ContentLength)
	u, err := url.Parse(resp.URL)
	require.NoError(t, err)
	assert.Empty(t, u.Query().Get("x-probe-range"), "the link signs no range")
	assert.NotContains(t, u.Query().Get("X-Amz-SignedHeaders"), "range")
	assert.Equal(t, `"cccccccccccccccccccccccccccccccc-13"`, resp.SendHeaders["if-match"], "if-match where provable")
	_, hasRange := resp.SendHeaders["range"]
	assert.False(t, hasRange)
	assert.Equal(t, http.StatusOK, resp.Status)

	// A client Range of ≥ 64 MiB: windowed from its start.
	ranged := f.admit(windowed(f.signedGet(path, nil, map[string]string{"Range": "bytes=10485760-"})))
	require.NotNil(t, ranged.Window, "%+v", ranged)
	assert.Equal(t, int64(10<<20), ranged.Window.Start)
	assert.Equal(t, int64(90<<20), ranged.ContentLength)
	assert.Equal(t, http.StatusPartialContent, ranged.Status)

	// No if-match provable (an origin-written object past the single-PUT
	// cutoff): still windowed, the link signing neither — the Worker
	// checks every range's ETag against the first's.
	f.exec(`UPDATE object_head_cache SET etag = 'dddddddddddddddddddddddddddddddd' WHERE tenant_id = $1 AND object_key = 'big.bin'`, f.tenantID)
	plain := f.admit(windowed(f.signedGet(path, nil, nil)))
	require.NotNil(t, plain.Window, "%+v", plain)
	assert.Empty(t, plain.SendHeaders["if-match"])

	// 10 MiB: Part B's link — the exact span signed, no window.
	f.exec(`UPDATE object_head_cache SET size_bytes = $2 WHERE tenant_id = $1 AND object_key = 'big.bin'`, f.tenantID, int64(10<<20))
	small := f.admit(windowed(f.signedGet(path, nil, nil)))
	require.True(t, small.Edge)
	assert.Nil(t, small.Window)
	assert.Equal(t, "bytes=0-10485759", small.SendHeaders["range"])
}

// The windowed read's commit: served meters the bytes the Worker sent (the
// sum of its ranges); a read cut mid-object (a later range failed after
// bytes went out) meters what was sent and charges the backend's breaker.
func TestEdgeCommitGet_WindowMetersWhatWasSent(t *testing.T) {
	f := setupEdgeFixture(t)
	f.putObject("win.bin", testBytes(8192), nil)
	f.exec(`UPDATE object_head_cache SET size_bytes = $2, etag = 'cccccccccccccccccccccccccccccccc-13', body_md5 = NULL WHERE tenant_id = $1 AND object_key = 'win.bin'`,
		f.tenantID, int64(100<<20))
	f.wireEgress(1<<40, 0, nil)
	path := "/" + f.bucket + "/win.bin"
	a := f.admit(windowed(f.signedGet(path, nil, nil)))
	require.NotNil(t, a.Window)
	before := f.egressRecorded()
	relay, code := f.commit(edgeCommitRequest{AdmitID: a.AdmitID, Outcome: edgeGetServed, BackendStatus: 206, BackendContentLength: 100 << 20, BytesSent: 100 << 20})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, http.StatusOK, relay.Status)
	assert.Equal(t, before+int64(100<<20), f.egressRecorded())

	cut := f.admit(windowed(f.signedGet(path, nil, nil)))
	before = f.egressRecorded()
	_, code = f.commit(edgeCommitRequest{AdmitID: cut.AdmitID, Outcome: edgeGetError, BackendStatus: 500, BackendCode: "InternalError", BytesSent: 48 << 20})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, before+int64(48<<20), f.egressRecorded(), "a cut read meters what the client received")
	assert.Equal(t, "failed", f.admitState(cut.AdmitID))
}

// windowed marks a description as a Worker that reads a window (0.4.0).
func windowed(req edgeAdmitRequest) edgeAdmitRequest {
	req.Features = append(req.Features, edgeFeatureWindow)
	return req
}
